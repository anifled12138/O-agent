package agent

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/projectpolicy"
)

const (
	maxSubmoduleDepth = 8
	maxSubmoduleCount = 128
)

type gitSubmodule struct {
	Name string
	Path string
	URL  string
}

func parseGitBlobEntry(output, expectedPath string) (string, bool, error) {
	if output == "" {
		return "", false, nil
	}
	var blob string
	for _, record := range strings.Split(output, "\x00") {
		if record == "" {
			continue
		}
		metadata, resultPath, ok := strings.Cut(record, "\t")
		if !ok || resultPath != expectedPath {
			return "", false, errors.New("Git returned an unexpected committed .gitmodules tree entry")
		}
		fields := strings.Fields(metadata)
		if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || !validGitObjectID(strings.ToLower(fields[2])) {
			return "", false, errors.New("committed .gitmodules must be a regular Git blob")
		}
		if blob != "" {
			return "", false, errors.New("Git returned duplicate committed .gitmodules entries")
		}
		blob = strings.ToLower(fields[2])
	}
	return blob, blob != "", nil
}

// parseGitSubmoduleConfig parses `git config --null --get-regexp` output.
// Git emits each key/value record separated by LF and terminates the record by NUL.
func parseGitSubmoduleConfig(output string) ([]gitSubmodule, error) {
	modules := map[string]*gitSubmodule{}
	if output == "" {
		return nil, nil
	}
	for _, record := range strings.Split(output, "\x00") {
		if record == "" {
			continue
		}
		key, value, ok := strings.Cut(record, "\n")
		if !ok || strings.ContainsAny(key+value, "\r\x00") {
			return nil, errors.New("Git returned malformed .gitmodules configuration")
		}
		lowerKey := strings.ToLower(key)
		if !strings.HasPrefix(lowerKey, "submodule.") {
			return nil, errors.New("Git returned an unexpected .gitmodules key")
		}
		field := ""
		name := ""
		switch {
		case strings.HasSuffix(lowerKey, ".path"):
			field, name = "path", key[len("submodule."):len(key)-len(".path")]
		case strings.HasSuffix(lowerKey, ".url"):
			field, name = "url", key[len("submodule."):len(key)-len(".url")]
		default:
			continue
		}
		if name == "" || len(name) > 128 || strings.IndexFunc(name, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-')
		}) >= 0 {
			return nil, errors.New("Git submodule name is invalid")
		}
		module := modules[name]
		if module == nil {
			module = &gitSubmodule{Name: name}
			modules[name] = module
		}
		switch field {
		case "path":
			if module.Path != "" || value == "" {
				return nil, fmt.Errorf("Git submodule %q has a duplicate or empty path", name)
			}
			module.Path = value
		case "url":
			if module.URL != "" || value == "" {
				return nil, fmt.Errorf("Git submodule %q has a duplicate or empty URL", name)
			}
			module.URL = value
		}
	}
	result := make([]gitSubmodule, 0, len(modules))
	for _, module := range modules {
		if module.Path == "" || module.URL == "" {
			return nil, fmt.Errorf("Git submodule %q must declare both path and URL", module.Name)
		}
		if err := validateGitSubmodulePath(module.Path); err != nil {
			return nil, fmt.Errorf("Git submodule %q: %w", module.Name, err)
		}
		result = append(result, *module)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path == result[j].Path {
			return result[i].Name < result[j].Name
		}
		return result[i].Path < result[j].Path
	})
	for i := 1; i < len(result); i++ {
		if result[i-1].Path == result[i].Path {
			return nil, fmt.Errorf("multiple Git submodules use path %q", result[i].Path)
		}
	}
	return result, nil
}

func validateGitSubmodulePath(value string) error {
	if len(value) > 1024 || strings.ContainsAny(value, "\\:\x00\r\n") || strings.HasPrefix(value, "/") || filepath.IsAbs(value) {
		return errors.New("path must be a relative repository path")
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return errors.New("path is not normalized or escapes the repository")
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("path contains an unsafe component")
		}
	}
	return nil
}

func resolveGitSubmoduleURL(parentURL, raw string) (string, error) {
	if strings.TrimSpace(raw) != raw || raw == "" || strings.ContainsAny(raw, "\\\x00\r\n") {
		return "", errors.New("submodule URL is malformed")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse submodule URL: %w", err)
	}
	if parsed.IsAbs() || parsed.Host != "" {
		_, normalized, err := projectpolicy.ValidateRepositoryURL(raw)
		if err != nil {
			return "", errors.New("submodule URL must target a GitHub or Gitee HTTPS repository")
		}
		return normalized, nil
	}
	if strings.HasPrefix(raw, "/") || strings.ContainsAny(raw, "?#%") || strings.Contains(raw, ":") {
		return "", errors.New("relative submodule URL contains an unsafe URL component")
	}
	_, parent, err := projectpolicy.ValidateRepositoryURL(parentURL)
	if err != nil {
		return "", errors.New("parent repository URL is not authorized")
	}
	base, err := url.Parse(parent)
	if err != nil {
		return "", fmt.Errorf("parse parent repository URL: %w", err)
	}
	parentPath := base.Path
	base.Path = path.Clean(base.Path + "/" + raw)
	base.RawPath = ""
	_, normalized, err := projectpolicy.ValidateRepositoryURL(base.String())
	if err != nil {
		return "", errors.New("relative submodule URL does not resolve to a supported GitHub or Gitee repository")
	}
	resolved, _ := url.Parse(normalized)
	parentParts := strings.Split(strings.Trim(strings.TrimSuffix(parentPath, ".git"), "/"), "/")
	resolvedParts := strings.Split(strings.Trim(strings.TrimSuffix(resolved.Path, ".git"), "/"), "/")
	if len(parentParts) != 2 || len(resolvedParts) != 2 || parentParts[0] != resolvedParts[0] {
		return "", errors.New("relative submodule URL must remain under the parent repository owner")
	}
	return normalized, nil
}

type submoduleWork struct {
	relative string
	url      string
	depth    int
}

func (s *Service) initializeGitSubmodules(ctx context.Context, root, rootURL string) error {
	queue := []submoduleWork{{url: rootURL}}
	authorized := []string{rootURL}
	seenPaths := map[string]struct{}{}
	initialized := 0
	for len(queue) > 0 {
		work := queue[0]
		queue = queue[1:]
		config := s.executionConfigSnapshot(authorized...)
		gitPath := gitSubmoduleGitPath(work.relative)
		manifestTree, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", []string{"-C", gitPath, "ls-tree", "-z", "HEAD", "--", ".gitmodules"}, root, []string{root}, nil, false, 30*time.Second)
		if runErr != nil || cleanupErr != nil {
			return errors.Join(fmt.Errorf("inspect committed Git submodule manifest: %w: %s", runErr, strings.TrimSpace(stderr)), cleanupErr)
		}
		manifestBlob, hasManifest, err := parseGitBlobEntry(manifestTree, ".gitmodules")
		if err != nil {
			return err
		}
		if !hasManifest {
			continue
		}
		manifestPath := filepath.Join(root, filepath.FromSlash(path.Join(work.relative, ".gitmodules")))
		manifestInfo, err := os.Lstat(manifestPath)
		if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 {
			return errors.Join(fmt.Errorf("committed .gitmodules file is missing or unsafe in the worktree: %w", domain.ErrConflict), err)
		}
		manifestHash, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", []string{"-C", gitPath, "hash-object", ".gitmodules"}, root, []string{root}, nil, false, 30*time.Second)
		if runErr != nil || cleanupErr != nil || !strings.EqualFold(strings.TrimSpace(manifestHash), manifestBlob) {
			return errors.Join(fmt.Errorf("worktree .gitmodules does not match the pinned commit; preserving local changes: %w: %s", domain.ErrConflict, strings.TrimSpace(stderr)), runErr, cleanupErr)
		}
		output, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", []string{"-C", gitPath, "config", "--null", "--blob", "HEAD:.gitmodules", "--get-regexp", `^submodule\..*\.(path|url)$`}, root, []string{root}, nil, false, 30*time.Second)
		if runErr != nil || cleanupErr != nil {
			return errors.Join(fmt.Errorf("read pinned Git submodule manifest: %w: %s", runErr, strings.TrimSpace(stderr)), cleanupErr)
		}
		modules, err := parseGitSubmoduleConfig(output)
		if err != nil {
			return err
		}
		if len(modules) > 0 && work.depth >= maxSubmoduleDepth {
			return fmt.Errorf("Git submodule nesting exceeds %d levels", maxSubmoduleDepth)
		}
		for _, module := range modules {
			if initialized >= maxSubmoduleCount {
				return fmt.Errorf("Git submodule count exceeds %d", maxSubmoduleCount)
			}
			relative := module.Path
			if work.relative != "" {
				relative = path.Join(work.relative, module.Path)
			}
			if err := validateGitSubmodulePath(relative); err != nil {
				return fmt.Errorf("Git submodule path %q: %w", relative, err)
			}
			if _, exists := seenPaths[relative]; exists {
				return fmt.Errorf("Git submodule path %q is declared more than once", relative)
			}
			seenPaths[relative] = struct{}{}
			url, err := resolveGitSubmoduleURL(work.url, module.URL)
			if err != nil {
				return fmt.Errorf("Git submodule %q URL: %w", module.Name, err)
			}
			if err := ensureSafeSubmoduleDestination(root, relative); err != nil {
				return err
			}
			moduleGitPath := gitSubmoduleGitPath(work.relative)
			gitlinkOutput, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", []string{"-C", moduleGitPath, "ls-tree", "-z", "HEAD", "--", module.Path}, root, []string{root}, nil, false, 30*time.Second)
			if runErr != nil || cleanupErr != nil {
				return errors.Join(fmt.Errorf("verify Git submodule %q gitlink: %w: %s", module.Name, runErr, strings.TrimSpace(stderr)), cleanupErr)
			}
			gitlinkCommit, ok := gitSubmoduleLinkCommit(gitlinkOutput, module.Path)
			if !ok {
				return fmt.Errorf("Git submodule %q does not match a gitlink in the pinned commit", module.Name)
			}
			_, stderr, runErr, cleanupErr = s.runSandboxedCommand(ctx, config, "git", []string{"-C", moduleGitPath, "submodule", "init", "--", module.Path}, root, []string{root}, []string{root}, false, 30*time.Second)
			if runErr != nil || cleanupErr != nil {
				return errors.Join(fmt.Errorf("initialize Git submodule %q: %w: %s", module.Name, runErr, strings.TrimSpace(stderr)), cleanupErr)
			}
			configOutput, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", []string{"-C", moduleGitPath, "config", "--get", "submodule." + module.Name + ".url"}, root, []string{root}, nil, false, 30*time.Second)
			if runErr != nil || cleanupErr != nil {
				return errors.Join(fmt.Errorf("read initialized Git submodule URL: %w: %s", runErr, strings.TrimSpace(stderr)), cleanupErr)
			}
			resolvedConfigURL, err := resolveGitSubmoduleURL(work.url, strings.TrimSpace(configOutput))
			if err != nil || resolvedConfigURL != url {
				return errors.Join(fmt.Errorf("initialized Git submodule URL does not match the validated manifest: %q", module.Name), err)
			}
			moduleRoot := filepath.Join(root, filepath.FromSlash(relative))
			_, markerErr := os.Lstat(filepath.Join(moduleRoot, ".git"))
			alreadyAtPinnedCommit := false
			if markerErr == nil {
				headOutput, headStderr, headErr, headCleanupErr := s.runSandboxedCommand(ctx, config, "git", []string{"-C", relative, "rev-parse", "--verify", "HEAD^{commit}"}, root, []string{root}, nil, false, 30*time.Second)
				if headErr != nil || headCleanupErr != nil {
					return errors.Join(fmt.Errorf("read initialized Git submodule HEAD: %w: %s", headErr, strings.TrimSpace(headStderr)), headCleanupErr)
				}
				actualURL, urlStderr, urlErr, urlCleanupErr := s.runSandboxedCommand(ctx, config, "git", []string{"-C", relative, "remote", "get-url", "origin"}, root, []string{root}, nil, false, 30*time.Second)
				if urlErr != nil || urlCleanupErr != nil {
					return errors.Join(fmt.Errorf("read initialized Git submodule origin: %w: %s", urlErr, strings.TrimSpace(urlStderr)), urlCleanupErr)
				}
				_, normalizedActual, normalizeErr := projectpolicy.ValidateRepositoryURL(strings.TrimSpace(actualURL))
				if normalizeErr != nil || normalizedActual != url {
					return errors.Join(fmt.Errorf("initialized Git submodule origin differs from its authorized URL: %w", domain.ErrConflict), normalizeErr)
				}
				alreadyAtPinnedCommit = strings.EqualFold(strings.TrimSpace(headOutput), gitlinkCommit)
				if !alreadyAtPinnedCommit {
					return fmt.Errorf("Git submodule %q is already initialized at a different commit; preserving its worktree without reset: %w", module.Name, domain.ErrConflict)
				}
			} else if !errors.Is(markerErr, os.ErrNotExist) {
				return fmt.Errorf("inspect initialized Git submodule marker: %w", markerErr)
			}
			authorized = append(authorized, url)
			config = s.executionConfigSnapshot(authorized...)
			if !alreadyAtPinnedCommit {
				_, stderr, runErr, cleanupErr = s.runSandboxedCommand(ctx, config, "git", []string{"-C", moduleGitPath, "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "http.followRedirects=false", "submodule", "update", "--init", "--", module.Path}, root, []string{root}, []string{root}, true, 180*time.Second)
				if runErr != nil || cleanupErr != nil {
					return errors.Join(fmt.Errorf("fetch Git submodule %q: %w: %s", module.Name, runErr, strings.TrimSpace(stderr)), cleanupErr)
				}
			}
			actualURL, _, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", []string{"-C", relative, "remote", "get-url", "origin"}, root, []string{root}, nil, false, 30*time.Second)
			if runErr != nil || cleanupErr != nil {
				return errors.Join(fmt.Errorf("verify fetched Git submodule origin: %w", runErr), cleanupErr)
			}
			_, normalizedActual, err := projectpolicy.ValidateRepositoryURL(strings.TrimSpace(actualURL))
			if err != nil || normalizedActual != url {
				return errors.Join(fmt.Errorf("fetched Git submodule origin differs from its authorized URL"), err)
			}
			headOutput, _, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", []string{"-C", relative, "rev-parse", "--verify", "HEAD^{commit}"}, root, []string{root}, nil, false, 30*time.Second)
			if runErr != nil || cleanupErr != nil || !strings.EqualFold(strings.TrimSpace(headOutput), gitlinkCommit) {
				return errors.Join(fmt.Errorf("fetched Git submodule commit did not match its pinned gitlink: %w", domain.ErrConflict), runErr, cleanupErr)
			}
			initialized++
			queue = append(queue, submoduleWork{relative: relative, url: url, depth: work.depth + 1})
		}
	}
	return nil
}

func gitSubmoduleGitPath(relative string) string {
	if relative == "" {
		return "."
	}
	return filepath.ToSlash(relative)
}

func gitOutputHasSubmoduleLink(output, expectedPath string) bool {
	_, ok := gitSubmoduleLinkCommit(output, expectedPath)
	return ok
}

func gitSubmoduleLinkCommit(output, expectedPath string) (string, bool) {
	for _, record := range strings.Split(output, "\x00") {
		metadata, resultPath, ok := strings.Cut(record, "\t")
		if !ok || resultPath != expectedPath {
			continue
		}
		fields := strings.Fields(metadata)
		if len(fields) != 3 || fields[0] != "160000" || fields[1] != "commit" || !validGitObjectID(strings.ToLower(fields[2])) {
			return "", false
		}
		return strings.ToLower(fields[2]), true
	}
	return "", false
}

func ensureSafeSubmoduleDestination(root, relative string) error {
	if err := validateGitSubmodulePath(relative); err != nil {
		return err
	}
	current := root
	parts := strings.Split(filepath.FromSlash(relative), string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect Git submodule destination: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Git submodule destination %q crosses a symbolic link", relative)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("Git submodule destination parent %q is not a directory", relative)
		}
	}
	return nil
}
