package pluginforge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"axiom.local/agent/internal/domain"
)

const (
	maxSourceFileBytes  = 512 << 10
	maxSourcePatchBytes = 1 << 20
	maxSourceFiles      = 256
	maxSourceDiffBytes  = 512 << 10
)

type PatchInput struct {
	ExpectedRevision string `json:"expectedRevision"`
	Patch            string `json:"patch"`
}

func (s *Service) SourceTree(ctx context.Context, userID, projectID string) (SourceTree, error) {
	unlock := s.lockSourceProject(projectID)
	defer unlock()
	project, err := s.sourceProject(ctx, userID, projectID, false)
	if err != nil {
		return SourceTree{}, err
	}
	if err = ensureSourceRepositoryClean(ctx, project.SourceDir); err != nil {
		return SourceTree{}, err
	}
	revision, err := gitText(ctx, project.SourceDir, "rev-parse", "HEAD")
	if err != nil {
		return SourceTree{}, err
	}
	files, err := inspectSourceTree(project.SourceDir)
	if err != nil {
		return SourceTree{}, err
	}
	return SourceTree{ProjectID: project.ID, Revision: revision, State: project.State, Files: files}, nil
}

func (s *Service) ReadSourceFile(ctx context.Context, userID, projectID, path string) (SourceFile, error) {
	unlock := s.lockSourceProject(projectID)
	defer unlock()
	project, err := s.sourceProject(ctx, userID, projectID, false)
	if err != nil {
		return SourceFile{}, err
	}
	relative, target, err := resolveSourcePath(project.SourceDir, path)
	if err != nil {
		return SourceFile{}, err
	}
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SourceFile{}, domain.ErrNotFound
		}
		return SourceFile{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSourceFileBytes {
		return SourceFile{}, errors.New("plugin source must be a regular text file no larger than 512 KiB")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return SourceFile{}, err
	}
	if strings.ContainsRune(string(raw), '\x00') {
		return SourceFile{}, errors.New("plugin source must be text")
	}
	revision, err := gitText(ctx, project.SourceDir, "rev-parse", "HEAD")
	if err != nil {
		return SourceFile{}, err
	}
	return SourceFile{ProjectID: project.ID, Revision: revision, Path: filepath.ToSlash(relative), Content: string(raw), SHA256: sourceDigest(raw)}, nil
}

func (s *Service) SourceDiff(ctx context.Context, userID, projectID string) (SourceDiff, error) {
	unlock := s.lockSourceProject(projectID)
	defer unlock()
	project, err := s.sourceProject(ctx, userID, projectID, false)
	if err != nil {
		return SourceDiff{}, err
	}
	return latestSourceDiff(ctx, project)
}

func (s *Service) ApplySourcePatch(ctx context.Context, userID, projectID string, in PatchInput) (Project, SourceDiff, error) {
	unlock := s.lockSourceProject(projectID)
	defer unlock()
	project, err := s.sourceProject(ctx, userID, projectID, true)
	if err != nil {
		return Project{}, SourceDiff{}, err
	}
	if err = ensureSourceRepositoryClean(ctx, project.SourceDir); err != nil {
		return project, SourceDiff{}, err
	}
	currentRevision, err := gitText(ctx, project.SourceDir, "rev-parse", "HEAD")
	if err != nil {
		return project, SourceDiff{}, err
	}
	if strings.TrimSpace(in.ExpectedRevision) == "" || strings.TrimSpace(in.ExpectedRevision) != currentRevision {
		return project, SourceDiff{}, errors.New("source revision changed; inspect the source tree again before applying a patch")
	}
	if len(in.Patch) == 0 || len(in.Patch) > maxSourcePatchBytes {
		return project, SourceDiff{}, errors.New("source patch must be between 1 byte and 1 MiB")
	}
	paths, err := validateSourcePatch(in.Patch)
	if err != nil {
		return project, SourceDiff{}, err
	}
	if err = gitWithInput(ctx, project.SourceDir, in.Patch, "apply", "--check", "--index", "--whitespace=error-all", "-"); err != nil {
		return project, SourceDiff{}, fmt.Errorf("patch does not apply cleanly: %w", err)
	}
	if err = gitWithInput(ctx, project.SourceDir, in.Patch, "apply", "--index", "--whitespace=fix", "-"); err != nil {
		return project, SourceDiff{}, fmt.Errorf("apply source patch: %w", err)
	}
	if err = verifyPatchedWorkspace(ctx, project.SourceDir, paths); err != nil {
		return project, SourceDiff{}, err
	}
	if err = commitSourcePatch(ctx, project.SourceDir, paths); err != nil {
		return project, SourceDiff{}, err
	}
	diff, err := latestSourceDiff(ctx, project)
	if err != nil {
		return project, SourceDiff{}, err
	}
	s.audit(ctx, project, "source.patch_applied", map[string]any{"paths": paths, "revision": diff.Revision})
	return project, diff, nil
}

func (s *Service) sourceProject(ctx context.Context, userID, projectID string, mutable bool) (Project, error) {
	project, err := s.repo.Project(ctx, userID, projectID)
	if err != nil {
		return Project{}, err
	}
	if mutable && project.State != StateGenerated && project.State != StateBuildFailed {
		return Project{}, errors.New("source can only be edited before a successful release build")
	}
	switch project.State {
	case StateProposed, StateGenerating, StateGenerationFailed:
		return Project{}, errors.New("plugin source has not been generated")
	}
	if _, err := os.Stat(filepath.Join(project.SourceDir, ".git")); err != nil {
		return Project{}, errors.New("plugin source repository is unavailable")
	}
	return project, nil
}

func inspectSourceTree(root string) ([]SourceEntry, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	entries := make([]SourceEntry, 0, 16)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "build" || entry.Name() == "dist" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !allowedSourcePath(relative) {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		if info.Size() > maxSourceFileBytes {
			return fmt.Errorf("source file %s exceeds 512 KiB", filepath.ToSlash(relative))
		}
		if len(entries) >= maxSourceFiles {
			return fmt.Errorf("plugin source exceeds %d files", maxSourceFiles)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, SourceEntry{Path: filepath.ToSlash(relative), Size: info.Size(), SHA256: sourceDigest(raw)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func resolveSourcePath(root, path string) (string, string, error) {
	relative := filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))
	if !allowedSourcePath(relative) {
		return "", "", errors.New("source path is outside the plugin contract")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	target, err := filepath.Abs(filepath.Join(absoluteRoot, relative))
	if err != nil || !pathWithin(absoluteRoot, target) {
		return "", "", errors.New("source path escapes the plugin workspace")
	}
	return relative, target, nil
}

func validateSourcePatch(patch string) ([]string, error) {
	if strings.ContainsRune(patch, '\x00') {
		return nil, errors.New("source patch must be text")
	}
	paths := make([]string, 0, 4)
	seen := map[string]bool{}
	current := ""
	inHunk := false
	for _, line := range strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			fields := strings.Fields(line)
			if len(fields) != 4 || !strings.HasPrefix(fields[2], "a/") || !strings.HasPrefix(fields[3], "b/") {
				return nil, errors.New("patch has an unsupported diff header")
			}
			left, right := strings.TrimPrefix(fields[2], "a/"), strings.TrimPrefix(fields[3], "b/")
			if left != right {
				return nil, errors.New("patch cannot rename plugin source files")
			}
			clean := filepath.Clean(filepath.FromSlash(right))
			if !allowedSourcePath(clean) {
				return nil, fmt.Errorf("patch path %q is outside the plugin contract", right)
			}
			current = filepath.ToSlash(clean)
			inHunk = false
			if !seen[current] {
				if len(paths) >= maxSourceFiles {
					return nil, fmt.Errorf("source patch exceeds %d files", maxSourceFiles)
				}
				seen[current] = true
				paths = append(paths, current)
			}
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			inHunk = true
			continue
		}
		if inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(line, "deleted file mode"), strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "rename to "), strings.HasPrefix(line, "similarity index "), strings.HasPrefix(line, "GIT binary patch"), strings.HasPrefix(line, "Binary files "), strings.HasPrefix(line, "old mode "), strings.HasPrefix(line, "new mode "):
			return nil, errors.New("patch cannot delete, rename, change modes, or modify binary files")
		case strings.HasPrefix(line, "new file mode ") && line != "new file mode 100644":
			return nil, errors.New("new plugin source files must use mode 100644")
		case strings.HasPrefix(line, "+++ "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			if path == "/dev/null" {
				return nil, errors.New("patch cannot delete plugin source files")
			}
			if !strings.HasPrefix(path, "b/") || filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimPrefix(path, "b/")))) != current {
				return nil, errors.New("patch contains an invalid destination path")
			}
		case strings.HasPrefix(line, "--- "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "--- "))
			if path != "/dev/null" && (!strings.HasPrefix(path, "a/") || filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimPrefix(path, "a/")))) != current) {
				return nil, errors.New("patch contains an invalid source path")
			}
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("patch must use git unified diff format")
	}
	sort.Strings(paths)
	return paths, nil
}

func verifyPatchedWorkspace(ctx context.Context, root string, expected []string) error {
	deleted, err := gitText(ctx, root, "diff", "--cached", "--name-only", "--diff-filter=D", "HEAD", "--")
	if err != nil {
		return err
	}
	if deleted != "" {
		return errors.New("patch deleted plugin source; deletion is not permitted")
	}
	changed, err := gitText(ctx, root, "diff", "--cached", "--name-only", "HEAD", "--")
	if err != nil {
		return err
	}
	actual := nonEmptyLines(changed)
	sort.Strings(actual)
	if strings.Join(actual, "\n") != strings.Join(expected, "\n") {
		return errors.New("patch changed files outside its declared diff headers")
	}
	for _, path := range actual {
		_, target, err := resolveSourcePath(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxSourceFileBytes {
			return fmt.Errorf("patched source %s is not a valid regular text file", path)
		}
		raw, err := os.ReadFile(target)
		if err != nil || strings.ContainsRune(string(raw), '\x00') {
			return fmt.Errorf("patched source %s is not valid text", path)
		}
	}
	return nil
}

func ensureNoSymlinkPath(root, target string) error {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(absoluteRoot, target)
	if err != nil || !pathWithin(absoluteRoot, target) {
		return errors.New("source path escapes the plugin workspace")
	}
	cursor := absoluteRoot
	for _, part := range strings.Split(relative, string(os.PathSeparator)) {
		cursor = filepath.Join(cursor, part)
		info, statErr := os.Lstat(cursor)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("source path traverses a symbolic link")
		}
	}
	return nil
}

func commitSourcePatch(ctx context.Context, root string, paths []string) error {
	args := append([]string{"add", "--"}, paths...)
	if _, err := gitCommand(ctx, root, "", args...); err != nil {
		return fmt.Errorf("stage source patch: %w", err)
	}
	if _, err := gitCommand(ctx, root, "", "commit", "-m", "feat: apply agent source patch"); err != nil {
		return fmt.Errorf("commit source patch: %w", err)
	}
	return nil
}

func latestSourceDiff(ctx context.Context, project Project) (SourceDiff, error) {
	revision, err := gitText(ctx, project.SourceDir, "rev-parse", "HEAD")
	if err != nil {
		return SourceDiff{}, err
	}
	raw, err := gitCommand(ctx, project.SourceDir, "", "show", "--format=", "--no-ext-diff", "--unified=3", "HEAD", "--")
	if err != nil {
		return SourceDiff{}, err
	}
	truncated := len(raw) > maxSourceDiffBytes
	if truncated {
		raw = raw[:maxSourceDiffBytes]
	}
	return SourceDiff{ProjectID: project.ID, Revision: revision, Patch: string(raw), Truncated: truncated}, nil
}

func gitWithInput(ctx context.Context, root, input string, args ...string) error {
	_, err := gitCommand(ctx, root, input, args...)
	return err
}

func ensureSourceRepositoryClean(ctx context.Context, root string) error {
	raw, err := gitCommand(ctx, root, "", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if len(line) < 4 {
			return errors.New("plugin source repository has an unreadable worktree state")
		}
		path := filepath.ToSlash(strings.TrimSpace(line[3:]))
		if path == "build" || strings.HasPrefix(path, "build/") {
			continue
		}
		return fmt.Errorf("plugin source repository has an uncommitted change at %s", path)
	}
	return nil
}

func gitText(ctx context.Context, root string, args ...string) (string, error) {
	raw, err := gitCommand(ctx, root, "", args...)
	return strings.TrimSpace(string(raw)), err
}

func gitCommand(ctx context.Context, root, input string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = root
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	raw, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(raw))
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return raw, nil
}

func nonEmptyLines(value string) []string {
	result := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, filepath.ToSlash(line))
		}
	}
	return result
}

func sourceDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
