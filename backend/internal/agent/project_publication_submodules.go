package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/projectpolicy"
)

// PlanProjectSubmodulePublications derives a finite child-ref plan from the
// immutable parent commit. It performs no network writes; callers must persist
// the returned plan before publishing any ref.
func (s *Service) PlanProjectSubmodulePublications(ctx context.Context, repositoryURL, workdir, commitSHA string) ([]domain.ProjectPublicationSubmodule, error) {
	_, normalizedURL, err := projectpolicy.ValidateRepositoryURL(repositoryURL)
	if err != nil || normalizedURL != strings.TrimSpace(repositoryURL) || !validGitObjectID(commitSHA) {
		return nil, errors.New("submodule publication plan target is invalid")
	}
	root, err := filepath.Abs(filepath.Clean(workdir))
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve submodule publication worktree: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("publication worktree is not a directory")
		}
		return nil, err
	}
	config := s.executionConfigSnapshot(repositoryURL)
	run := func(repositoryPath string, args []string) (string, error) {
		if repositoryPath != "" {
			args = append([]string{"-C", filepath.ToSlash(repositoryPath)}, args...)
		}
		stdout, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", args, root, []string{root}, nil, false, 30*time.Second)
		if runErr != nil || cleanupErr != nil {
			return "", errors.Join(fmt.Errorf("inspect pinned submodule publication: %w: %s", runErr, strings.TrimSpace(stderr)), cleanupErr)
		}
		return stdout, nil
	}
	if _, err := run("", []string{"cat-file", "-e", strings.ToLower(commitSHA) + "^{commit}"}); err != nil {
		return nil, err
	}
	plan := make([]domain.ProjectPublicationSubmodule, 0)
	seen := make(map[string]bool)
	count := 0
	var walk func(repositoryPath, parentURL, pinnedCommit string, depth int) error
	walk = func(repositoryPath, parentURL, pinnedCommit string, depth int) error {
		if depth > maxSubmoduleDepth {
			return fmt.Errorf("submodule publication nesting exceeds %d levels: %w", maxSubmoduleDepth, domain.ErrInvalid)
		}
		manifest, err := run(repositoryPath, []string{"config", "--null", "--blob", strings.ToLower(pinnedCommit) + ":.gitmodules", "--get-regexp", `^submodule\..*\.(path|url)$`})
		if err != nil {
			// Commits without .gitmodules are valid leaves. Confirm absence from the pinned tree.
			tree, treeErr := run(repositoryPath, []string{"ls-tree", "-z", strings.ToLower(pinnedCommit), "--", ".gitmodules"})
			if treeErr != nil {
				return errors.Join(err, treeErr)
			}
			if strings.TrimSpace(tree) == "" {
				return nil
			}
			return err
		}
		modules, err := parseGitSubmoduleConfig(manifest)
		if err != nil {
			return err
		}
		for _, module := range modules {
			count++
			if count > maxSubmoduleCount {
				return fmt.Errorf("submodule publication exceeds %d repositories: %w", maxSubmoduleCount, domain.ErrInvalid)
			}
			globalPath := path.Join(repositoryPath, module.Path)
			if err := validateGitSubmodulePath(globalPath); err != nil {
				return err
			}
			if seen[globalPath] {
				return fmt.Errorf("duplicate submodule publication path %q: %w", globalPath, domain.ErrConflict)
			}
			seen[globalPath] = true
			link, err := run(repositoryPath, []string{"ls-tree", "-z", strings.ToLower(pinnedCommit), "--", module.Path})
			if err != nil {
				return err
			}
			childCommit, ok := gitSubmoduleLinkCommit(link, module.Path)
			if !ok {
				return fmt.Errorf("pinned submodule %q is not a gitlink: %w", globalPath, domain.ErrConflict)
			}
			childURL, err := resolveGitSubmoduleURL(parentURL, module.URL)
			if err != nil {
				return err
			}
			if err := ensureSafeSubmoduleDestination(root, globalPath); err != nil {
				return err
			}
			childDir := filepath.Join(root, filepath.FromSlash(globalPath))
			childInfo, err := os.Lstat(childDir)
			if err != nil || !childInfo.IsDir() || childInfo.Mode()&os.ModeSymlink != 0 {
				return errors.Join(fmt.Errorf("pinned submodule worktree %q is unavailable or unsafe: %w", globalPath, domain.ErrConflict), err)
			}
			origin, err := run(globalPath, []string{"remote", "get-url", "origin"})
			if err != nil {
				return err
			}
			_, normalizedOrigin, err := projectpolicy.ValidateRepositoryURL(strings.TrimSpace(origin))
			if err != nil || normalizedOrigin != childURL {
				return errors.Join(fmt.Errorf("submodule origin for %q differs from committed URL: %w", globalPath, domain.ErrConflict), err)
			}
			head, err := run(globalPath, []string{"rev-parse", "--verify", "HEAD^{commit}"})
			if err != nil {
				return err
			}
			if !strings.EqualFold(strings.TrimSpace(head), childCommit) {
				return fmt.Errorf("submodule %q is not checked out at pinned gitlink %s: %w", globalPath, childCommit, domain.ErrConflict)
			}
			ref := "refs/heads/o-agent-objects/" + childCommit
			plan = append(plan, domain.ProjectPublicationSubmodule{Path: globalPath, RepositoryURL: childURL, CommitSHA: childCommit, Ref: ref, Status: "pending"})
			if err := walk(globalPath, childURL, childCommit, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk("", normalizedURL, strings.ToLower(commitSHA), 0); err != nil {
		return nil, err
	}
	return plan, nil
}

// PublishProjectSubmoduleRef creates an immutable commit-addressed remote ref
// only if absent, and requires exact remote read-back before returning success.
// It never removes refs: parent-repository gitlinks are not visible to the
// child repository's reachability walk, so automatic GC can break old parents.
func (s *Service) PublishProjectSubmoduleRef(ctx context.Context, workdir string, target domain.ProjectPublicationSubmodule) (string, bool, error) {
	return s.projectSubmoduleRef(ctx, workdir, target, true)
}

// ReadProjectSubmoduleRef performs read-only reconciliation of a child ref.
func (s *Service) ReadProjectSubmoduleRef(ctx context.Context, workdir string, target domain.ProjectPublicationSubmodule) (string, error) {
	sha, _, err := s.projectSubmoduleRef(ctx, workdir, target, false)
	return sha, err
}

func (s *Service) projectSubmoduleRef(ctx context.Context, workdir string, target domain.ProjectPublicationSubmodule, publish bool) (string, bool, error) {
	_, normalizedURL, err := projectpolicy.ValidateRepositoryURL(target.RepositoryURL)
	if err != nil || normalizedURL != strings.TrimSpace(target.RepositoryURL) || !validGitObjectID(target.CommitSHA) || target.Ref != "refs/heads/o-agent-objects/"+strings.ToLower(target.CommitSHA) {
		return "", false, errors.New("submodule publication ref target is invalid")
	}
	if err := validateGitSubmodulePath(target.Path); err != nil {
		return "", false, fmt.Errorf("validate submodule publication path: %w", err)
	}
	root, err := filepath.Abs(filepath.Clean(workdir))
	if err != nil {
		return "", false, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", false, err
	}
	if err := ensureSafeSubmoduleDestination(root, target.Path); err != nil {
		return "", false, err
	}
	childDir := filepath.Join(root, filepath.FromSlash(target.Path))
	childDir, err = filepath.EvalSymlinks(childDir)
	if err != nil {
		return "", false, err
	}
	if !isPathWithin(root, childDir) {
		return "", false, domain.ErrConflict
	}
	info, err := os.Stat(childDir)
	if err != nil || !info.IsDir() {
		return "", false, errors.Join(errors.New("submodule directory is unavailable"), err)
	}
	execution := s.executionConfigSnapshot(target.RepositoryURL)
	gitCommonDir, err := resolveSubmoduleGitCommonDir(ctx, s, execution, childDir, root)
	if err != nil {
		return "", false, err
	}
	git := func(args []string, network bool) (string, error) {
		writePaths := []string{}
		if network {
			writePaths = []string{gitCommonDir}
		}
		stdout, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, execution, "git", args, childDir, []string{childDir}, writePaths, network, 60*time.Second)
		if cleanupErr != nil {
			return stdout, errors.Join(runErr, fmt.Errorf("clean up submodule Git command: %w", cleanupErr))
		}
		if runErr != nil {
			return stdout, fmt.Errorf("sandboxed submodule Git %s failed: %w: %s", args[0], runErr, strings.TrimSpace(stderr))
		}
		return stdout, nil
	}
	origin, err := git([]string{"remote", "get-url", "origin"}, false)
	if err != nil {
		return "", false, err
	}
	_, gotURL, err := projectpolicy.ValidateRepositoryURL(strings.TrimSpace(origin))
	if err != nil || gotURL != normalizedURL {
		return "", false, errors.Join(errors.New("submodule origin changed after publication plan was persisted"), err)
	}
	head, err := git([]string{"rev-parse", "--verify", "HEAD^{commit}"}, false)
	if err != nil {
		return "", false, err
	}
	if !strings.EqualFold(strings.TrimSpace(head), target.CommitSHA) {
		return "", false, domain.ErrConflict
	}
	status, err := git([]string{"status", "--porcelain", "--untracked-files=all"}, false)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(status) != "" {
		return "", false, errors.New("submodule publication requires a clean child worktree")
	}
	if _, err := git([]string{"cat-file", "-e", target.CommitSHA + "^{commit}"}, false); err != nil {
		return "", false, err
	}
	readRemote := func() (string, error) {
		out, err := git([]string{"ls-remote", "--heads", "origin", target.Ref}, true)
		if err != nil {
			return "", err
		}
		fields := strings.Fields(out)
		if len(fields) == 0 {
			return "", nil
		}
		if len(fields) != 2 || fields[1] != target.Ref || !validGitObjectID(fields[0]) {
			return "", errors.New("Git remote returned an invalid submodule ref identity")
		}
		return strings.ToLower(fields[0]), nil
	}
	remoteSHA, err := readRemote()
	if err != nil {
		return "", false, err
	}
	if remoteSHA == target.CommitSHA {
		return remoteSHA, false, nil
	}
	if remoteSHA != "" {
		return remoteSHA, false, fmt.Errorf("commit-addressed submodule ref already points to %s, expected %s: %w", remoteSHA, target.CommitSHA, domain.ErrConflict)
	}
	if !publish {
		return "", false, nil
	}
	pushAttempted := true
	_, pushErr := git([]string{"push", "--force-with-lease=" + target.Ref + ":", "origin", target.CommitSHA + ":" + target.Ref}, true)
	readBack, readErr := readRemote()
	if readErr == nil && readBack == target.CommitSHA {
		return readBack, pushAttempted, nil
	}
	if pushErr != nil || readErr != nil {
		return readBack, pushAttempted, errors.Join(pushErr, readErr)
	}
	return readBack, pushAttempted, fmt.Errorf("submodule ref push returned without exact read-back: %w", domain.ErrConflict)
}

func resolveSubmoduleGitCommonDir(ctx context.Context, s *Service, execution coretools.ExecutionConfig, childDir, root string) (string, error) {
	stdout, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, execution, "git", []string{"rev-parse", "--git-common-dir"}, childDir, []string{childDir}, nil, false, 30*time.Second)
	if runErr != nil || cleanupErr != nil {
		return "", errors.Join(fmt.Errorf("resolve submodule Git metadata directory: %w: %s", runErr, strings.TrimSpace(stderr)), cleanupErr)
	}
	commonDir := strings.TrimSpace(stdout)
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(childDir, commonDir)
	}
	commonDir, err := filepath.Abs(filepath.Clean(commonDir))
	if err != nil {
		return "", err
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", fmt.Errorf("resolve submodule Git metadata links: %w", err)
	}
	if !isPathWithin(root, commonDir) {
		return "", fmt.Errorf("submodule Git metadata escaped project root: %w", domain.ErrConflict)
	}
	return commonDir, nil
}

func isPathWithin(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
