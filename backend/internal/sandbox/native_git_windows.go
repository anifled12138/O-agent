//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Git for Windows recognizes /dev/null even when Windows device names are rejected.
const nativeGitNullPath = "/dev/null"

type nativeRepositoryScope struct {
	ReadRoots             []string
	WriteRoots            []string
	Protected             []nativeACLRecord
	Workspace             string
	CommonDir             string
	SafeDirectories       []string
	CredentialHelpers     []string
	IdentityName          string
	IdentityEmail         string
	IdentitySigningFormat string
	IdentitySigningKey    string
	IdentityCommitSigning string
	IdentityTagSigning    string
	HasSSHSigningConfig   bool
	RemoteURLs            []string
}

type nativeGitMetadata struct {
	Workspace string
	GitDir    string
	CommonDir string
	Config    string
	Hooks     string
}

func discoverNativeRepositoryScope(ctx context.Context, workdir string, allowedRoots []string, writeAccess bool) (*nativeRepositoryScope, error) {
	var allowedBase string
	for _, root := range allowedRoots {
		resolved, err := canonicalNativeDirectory(root)
		if err != nil {
			continue
		}
		if pathWithin(resolved, workdir) && len(resolved) > len(allowedBase) {
			allowedBase = resolved
		}
	}
	if allowedBase == "" {
		return nil, fmt.Errorf("working directory %s is outside every host-authorized repository root", workdir)
	}
	marker, err := hasNativeGitMarker(workdir, allowedBase)
	if err != nil {
		return nil, err
	}
	if !marker {
		return nil, nil
	}
	gitExe, err := exec.LookPath("git.exe")
	if err != nil {
		gitExe, err = exec.LookPath("git")
	}
	if err != nil {
		return nil, fmt.Errorf("a Git repository is present but Git cannot be resolved from the trusted host environment: %w", err)
	}
	main, err := inspectNativeGitMetadata(ctx, gitExe, workdir)
	if err != nil {
		return nil, fmt.Errorf("inspect Git repository scope: %w", err)
	}
	workspace := main.Workspace
	if !pathWithin(allowedBase, workspace) || !pathWithin(workspace, workdir) {
		return nil, fmt.Errorf("Git worktree root %s is outside the selected project root %s", workspace, allowedBase)
	}
	metadata := []nativeGitMetadata{main}
	submodules, err := discoverNativeGitSubmodules(ctx, gitExe, main, writeAccess)
	if err != nil {
		return nil, fmt.Errorf("inspect Git submodule scope: %w", err)
	}
	metadata = append(metadata, submodules...)
	scope := &nativeRepositoryScope{Workspace: workspace, CommonDir: main.CommonDir}
	credentialHelpers := make(map[string]bool)
	for _, repo := range metadata {
		scope.SafeDirectories = append(scope.SafeDirectories, repo.Workspace)
		remotes, err := nativeGitRemoteURLs(ctx, gitExe, repo.Workspace)
		if err != nil {
			return nil, fmt.Errorf("inspect Git remotes for credential scope: %w", err)
		}
		scope.RemoteURLs = append(scope.RemoteURLs, remotes...)
		helperKeys, err := nativeGitCredentialHelperKeys(ctx, gitExe, repo.Workspace)
		if err != nil {
			return nil, fmt.Errorf("inspect repository-local Git credential helpers: %w", err)
		}
		for _, key := range helperKeys {
			if !credentialHelpers[key] {
				credentialHelpers[key] = true
				scope.CredentialHelpers = append(scope.CredentialHelpers, key)
			}
		}
		hasLocalSSHSigning, err := nativeGitSSHSigningConfigured(ctx, gitExe, repo.Workspace)
		if err != nil {
			return nil, fmt.Errorf("read repository SSH signing configuration: %w", err)
		}
		if hasLocalSSHSigning {
			scope.HasSSHSigningConfig = true
		}
	}
	scope.IdentityName, err = hostGitIdentityValue(ctx, gitExe, "user.name")
	if err != nil {
		return nil, fmt.Errorf("read host Git author name: %w", err)
	}
	scope.IdentityEmail, err = hostGitIdentityValue(ctx, gitExe, "user.email")
	if err != nil {
		return nil, fmt.Errorf("read host Git author email: %w", err)
	}
	scope.IdentitySigningFormat, err = hostGitIdentityValue(ctx, gitExe, "gpg.format")
	if err != nil {
		return nil, fmt.Errorf("read host Git signing format: %w", err)
	}
	scope.IdentitySigningKey, err = hostGitIdentityValue(ctx, gitExe, "user.signingkey")
	if err != nil {
		return nil, fmt.Errorf("read host Git SSH signing identity: %w", err)
	}
	scope.IdentityCommitSigning, err = hostGitIdentityValue(ctx, gitExe, "commit.gpgsign")
	if err != nil {
		return nil, fmt.Errorf("read host Git commit-signing preference: %w", err)
	}
	scope.IdentityTagSigning, err = hostGitIdentityValue(ctx, gitExe, "tag.gpgsign")
	if err != nil {
		return nil, fmt.Errorf("read host Git tag-signing preference: %w", err)
	}
	for _, root := range []string{main.GitDir, main.CommonDir} {
		if writeAccess {
			scope.WriteRoots = append(scope.WriteRoots, root)
		} else {
			scope.ReadRoots = append(scope.ReadRoots, root)
		}
	}
	if writeAccess {
		seen := map[string]bool{}
		for _, repo := range metadata {
			paths := []struct {
				path      string
				directory bool
				required  bool
			}{
				{repo.Config, false, true},
				{filepath.Join(repo.GitDir, "config"), false, false},
				{filepath.Join(repo.GitDir, "config.worktree"), false, false},
				{filepath.Join(repo.CommonDir, "config"), false, false},
				{repo.Hooks, true, repo.Hooks != ""},
				{filepath.Join(repo.CommonDir, "hooks"), true, false},
			}
			for _, item := range paths {
				if item.path == "" {
					continue
				}
				key := strings.ToLower(filepath.Clean(item.path))
				if seen[key] {
					continue
				}
				seen[key] = true
				info, statErr := os.Stat(item.path)
				if errors.Is(statErr, os.ErrNotExist) {
					if item.required {
						return nil, fmt.Errorf("cannot protect missing Git config or hooks path %s", item.path)
					}
					continue
				}
				if statErr != nil {
					return nil, fmt.Errorf("inspect protected Git metadata %s: %w", item.path, statErr)
				}
				if item.directory != info.IsDir() {
					return nil, fmt.Errorf("Git metadata object has the wrong type: %s", item.path)
				}
				if item.directory {
					if _, err := canonicalNativeDirectory(item.path); err != nil {
						return nil, err
					}
				} else if _, _, err := nativeFileIdentity(item.path); err != nil {
					return nil, err
				}
				volume, index, err := nativeFileIdentity(item.path)
				if err != nil {
					return nil, err
				}
				scope.Protected = append(scope.Protected, nativeACLRecord{Path: filepath.Clean(item.path), VolumeSerial: volume, FileIndex: index, Write: true, Deny: true})
			}
		}
	}
	return scope, nil
}

func nativeGitRemoteURLs(ctx context.Context, executable, worktree string) ([]string, error) {
	output, err := runNativeGit(ctx, executable, "-C", worktree, "remote")
	if err != nil {
		return nil, err
	}
	var remotes []string
	for _, name := range strings.Fields(string(output)) {
		urls, err := runNativeGit(ctx, executable, "-C", worktree, "remote", "get-url", "--all", name)
		if err != nil {
			return nil, err
		}
		for _, value := range strings.Split(strings.TrimSpace(string(urls)), "\n") {
			value = strings.TrimSpace(strings.TrimSuffix(value, "\r"))
			if value != "" && (strings.HasPrefix(strings.ToLower(value), "https://") || isNativeSSHRemoteURL(value)) {
				remotes = append(remotes, value)
			}
		}
	}
	return remotes, nil
}

func isNativeSSHRemoteURL(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	parsed, err := url.Parse(value)
	if err == nil && strings.EqualFold(parsed.Scheme, "ssh") {
		hasPassword := false
		validUser := true
		if parsed.User != nil {
			_, hasPassword = parsed.User.Password()
			validUser = safeNativeSSHUsername(parsed.User.Username())
		}
		return validUser && safeNativeSSHHost(parsed.Hostname()) && !hasPassword && parsed.RawQuery == "" && parsed.Fragment == "" && strings.Trim(parsed.Path, "/") != ""
	}
	colon := strings.IndexByte(value, ':')
	if colon <= 0 || colon == 1 || strings.ContainsAny(value[:colon], `/\\`) || strings.ContainsAny(value[colon+1:], "?#\r\n\x00") || strings.HasPrefix(value[colon+1:], "/") || strings.HasPrefix(value[colon+1:], `\`) {
		return false
	}
	userAndHost := value[:colon]
	if at := strings.LastIndexByte(userAndHost, '@'); at >= 0 {
		if strings.Contains(userAndHost[:at], "@") || !safeNativeSSHUsername(userAndHost[:at]) {
			return false
		}
		userAndHost = userAndHost[at+1:]
	}
	return safeNativeSSHHost(userAndHost) && strings.TrimSpace(value[colon+1:]) != ""
}

func safeNativeSSHUsername(value string) bool {
	if value == "" || len(value) > 128 || strings.HasPrefix(value, "-") {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", character)) {
			return false
		}
	}
	return true
}

func safeNativeSSHHost(value string) bool {
	if value == "" || len(value) > 255 || strings.HasPrefix(value, "-") {
		return false
	}
	if net.ParseIP(value) != nil {
		return true
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", character)) {
			return false
		}
	}
	return true
}

func hasNativeSSHRemote(urls []string) bool {
	for _, value := range urls {
		if isNativeSSHRemoteURL(value) {
			return true
		}
	}
	return false
}

func nativeSSHSigningConfigured(scope *nativeRepositoryScope) bool {
	return scope != nil && (nativeSSHSigningIdentityConfigured(scope) || scope.HasSSHSigningConfig)
}

func nativeSSHSigningIdentityConfigured(scope *nativeRepositoryScope) bool {
	return scope != nil && strings.EqualFold(strings.TrimSpace(scope.IdentitySigningFormat), "ssh") && strings.TrimSpace(scope.IdentitySigningKey) != ""
}

func nativeGitNeedsSSHAgent(scope *nativeRepositoryScope, networkAccess bool) bool {
	return scope != nil && ((networkAccess && hasNativeSSHRemote(scope.RemoteURLs)) || nativeSSHSigningConfigured(scope))
}

func inspectNativeGitMetadata(ctx context.Context, executable, workdir string) (nativeGitMetadata, error) {
	baseArgs := []string{"-C", workdir, "-c", "core.fsmonitor=false"}
	output, err := runNativeGit(ctx, executable, append(baseArgs, "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir", "--git-path", "config", "--git-path", "hooks")...)
	if err != nil {
		return nativeGitMetadata{}, err
	}
	queryOutput := strings.TrimSuffix(string(output), "\n")
	lines := strings.Split(queryOutput, "\n")
	if len(lines) != 5 {
		return nativeGitMetadata{}, fmt.Errorf("Git metadata query returned %d paths; expected 5", len(lines))
	}
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
		if lines[i] == "" {
			return nativeGitMetadata{}, errors.New("Git metadata query returned an empty path")
		}
	}
	workspace, err := canonicalNativeDirectory(lines[0])
	if err != nil {
		return nativeGitMetadata{}, fmt.Errorf("validate Git worktree root: %w", err)
	}
	resolve := func(value, base string, directory bool) (string, error) {
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		path, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return "", err
		}
		if directory {
			return canonicalNativeDirectory(path)
		}
		if _, _, err := nativeFileIdentity(path); err != nil {
			return "", err
		}
		return path, nil
	}
	gitDir, err := resolve(lines[1], workspace, true)
	if err != nil {
		return nativeGitMetadata{}, fmt.Errorf("validate Git administrative directory: %w", err)
	}
	commonDir, err := resolve(lines[2], gitDir, true)
	if err != nil {
		return nativeGitMetadata{}, fmt.Errorf("validate Git common directory: %w", err)
	}
	configPath, err := resolve(lines[3], gitDir, false)
	if err != nil {
		return nativeGitMetadata{}, fmt.Errorf("validate Git config path: %w", err)
	}
	hooksValue, hooksConfigured, err := nativeGitConfigValue(ctx, executable, workspace, "core.hooksPath")
	if err != nil {
		return nativeGitMetadata{}, fmt.Errorf("read effective repository Git hooks path: %w", err)
	}
	hooksPath := ""
	if hooksConfigured && (strings.EqualFold(strings.TrimSpace(hooksValue), "NUL") || strings.TrimSpace(hooksValue) == nativeGitNullPath) {
		// The repository explicitly disables hooks.
	} else {
		hooksValue = lines[4]
		if hooksConfigured {
			if strings.HasPrefix(hooksValue, "~") || strings.HasPrefix(strings.ToLower(hooksValue), "%prefix%") {
				return nativeGitMetadata{}, errors.New("Git hooks path uses a home or runtime-prefix expansion unsupported by the native sandbox")
			}
		}
		hooksPath, err = resolve(hooksValue, workspace, true)
		if err != nil {
			return nativeGitMetadata{}, fmt.Errorf("validate Git hooks path: %w", err)
		}
		if !pathWithin(commonDir, hooksPath) && !pathWithin(workspace, hooksPath) {
			return nativeGitMetadata{}, fmt.Errorf("Git hooks path %s is outside the verified repository scope", hooksPath)
		}
	}
	listed, err := nativeGitWorktreeListed(ctx, executable, workspace)
	if err != nil {
		return nativeGitMetadata{}, fmt.Errorf("verify Git worktree membership: %w", err)
	}
	if !listed {
		return nativeGitMetadata{}, errors.New("Git did not list the selected directory as a worktree")
	}
	return nativeGitMetadata{Workspace: workspace, GitDir: gitDir, CommonDir: commonDir, Config: configPath, Hooks: hooksPath}, nil
}

func nativeGitConfigValue(ctx context.Context, executable, worktree, key string) (string, bool, error) {
	output, err := runNativeGit(ctx, executable, "-C", worktree, "-c", "core.fsmonitor=false", "config", "--get", key)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(output), "\n"), "\r")
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", false, fmt.Errorf("Git config key %s has multiple or invalid values", key)
	}
	return value, true, nil
}

func nativeGitSSHSigningConfigured(ctx context.Context, executable, worktree string) (bool, error) {
	output, err := runNativeGit(ctx, executable, "-C", worktree, "-c", "core.fsmonitor=false", "config", "--null", "--get-regexp", `^(gpg\.format|user\.signingkey)$`)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	values := make(map[string]string, 2)
	for _, item := range strings.Split(string(output), "\x00") {
		if item == "" {
			continue
		}
		key, value, ok := strings.Cut(item, "\n")
		key = strings.ToLower(strings.TrimSpace(key))
		if !ok || (key != "gpg.format" && key != "user.signingkey") || strings.ContainsAny(value, "\r\x00") {
			return false, errors.New("Git returned malformed SSH signing configuration")
		}
		if _, duplicate := values[key]; duplicate {
			return false, fmt.Errorf("Git returned multiple values for %s", key)
		}
		values[key] = strings.TrimSpace(value)
	}
	return strings.EqualFold(values["gpg.format"], "ssh") && values["user.signingkey"] != "", nil
}

// nativeGitCredentialHelperKeys discovers helper keys stored in the selected
// repository configuration. The native backend does not expose those helpers
// to the low-privilege account until a host credential broker can provide
// repository-scoped credentials; an empty command-scope value disables each
// matching helper without rewriting the user's Git config.
func nativeGitCredentialHelperKeys(ctx context.Context, executable, worktree string) ([]string, error) {
	output, err := runNativeGit(ctx, executable, "-C", worktree, "-c", "core.fsmonitor=false", "config", "--null", "--name-only", "--get-regexp", `^(credential\.helper|credential\..*\.helper)$`)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	seen := make(map[string]bool)
	var keys []string
	for _, key := range strings.Split(string(output), "\x00") {
		key = strings.TrimSpace(key)
		lower := strings.ToLower(key)
		if key == "" || !(lower == "credential.helper" || (strings.HasPrefix(lower, "credential.") && strings.HasSuffix(lower, ".helper"))) {
			return nil, errors.New("Git returned an invalid credential helper key")
		}
		if !seen[lower] {
			seen[lower] = true
			keys = append(keys, lower)
		}
	}
	return keys, nil
}

func discoverNativeGitSubmodules(ctx context.Context, executable string, root nativeGitMetadata, writeAccess bool) ([]nativeGitMetadata, error) {
	const maxRepositories = 256
	seen := map[string]bool{strings.ToLower(filepath.Clean(root.Workspace)): true}
	var repositories []nativeGitMetadata
	var visit func(nativeGitMetadata, int) error
	visit = func(parent nativeGitMetadata, depth int) error {
		if depth > 32 || len(repositories) >= maxRepositories {
			return errors.New("Git submodule nesting exceeds the supported scope limit")
		}
		paths, err := nativeGitSubmodulePaths(ctx, executable, parent.Workspace)
		if err != nil {
			return err
		}
		for _, relative := range paths {
			if filepath.IsAbs(relative) {
				return fmt.Errorf("Git submodule path must be relative to its worktree: %q", relative)
			}
			candidate, err := filepath.Abs(filepath.Clean(filepath.Join(parent.Workspace, relative)))
			if err != nil || !pathWithin(parent.Workspace, candidate) {
				return fmt.Errorf("Git submodule path escapes its parent worktree: %q", relative)
			}
			key := strings.ToLower(filepath.Clean(candidate))
			if seen[key] {
				continue
			}
			seen[key] = true
			marker := filepath.Join(candidate, ".git")
			if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
				if writeAccess {
					return fmt.Errorf("writable Git scope includes uninitialized submodule %s; initialize it before running writable sandbox commands", candidate)
				}
				continue
			} else if err != nil {
				return fmt.Errorf("inspect Git submodule marker %s: %w", marker, err)
			}
			metadata, err := inspectNativeGitMetadata(ctx, executable, candidate)
			if err != nil {
				if writeAccess {
					return fmt.Errorf("verify writable Git submodule %s: %w", candidate, err)
				}
				continue
			}
			if !strings.EqualFold(filepath.Clean(metadata.Workspace), filepath.Clean(candidate)) {
				return fmt.Errorf("Git submodule path %s resolves to a different worktree %s", candidate, metadata.Workspace)
			}
			if !pathWithin(root.CommonDir, metadata.CommonDir) {
				return fmt.Errorf("Git submodule metadata %s is outside the verified common directory %s", metadata.CommonDir, root.CommonDir)
			}
			repositories = append(repositories, metadata)
			if err := visit(metadata, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(root, 1); err != nil {
		return nil, err
	}
	return repositories, nil
}

func nativeGitSubmodulePaths(ctx context.Context, executable, worktree string) ([]string, error) {
	file := filepath.Join(worktree, ".gitmodules")
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect Git submodule configuration %s: %w", file, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Git submodule configuration %s is not a regular file", file)
	}
	if _, _, err := nativeFileIdentity(file); err != nil {
		return nil, fmt.Errorf("validate Git submodule configuration %s: %w", file, err)
	}
	args := []string{"-C", worktree, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + nativeGitNullPath, "config", "--null", "--file", file, "--get-regexp", `^submodule\..*\.path$`}
	output, err := runNativeGit(ctx, executable, args...)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	seen := map[string]bool{}
	for _, entry := range strings.Split(string(output), "\x00") {
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "\n")
		if !ok || !strings.HasPrefix(strings.ToLower(key), "submodule.") || !strings.HasSuffix(strings.ToLower(key), ".path") || value == "" {
			return nil, errors.New("Git returned a malformed submodule path entry")
		}
		if strings.ContainsRune(value, '\x00') || strings.ContainsRune(value, '\n') || strings.ContainsRune(value, '\r') {
			return nil, errors.New("Git submodule path contains a control character")
		}
		normalized := strings.ToLower(filepath.Clean(value))
		if !seen[normalized] {
			seen[normalized] = true
			paths = append(paths, value)
		}
	}
	return paths, nil
}

func hostGitIdentityValue(ctx context.Context, executable, key string) (string, error) {
	query, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve host Git profile for %s: %w", key, err)
	}
	appData, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve host Git roaming configuration directory for %s: %w", key, err)
	}
	command := exec.CommandContext(query, executable, "config", "--global", "--get", key)
	command.Env = []string{"SYSTEMROOT=" + os.Getenv("SystemRoot"), "WINDIR=" + os.Getenv("SystemRoot"), "PATH=" + filepath.Dir(executable), "USERPROFILE=" + home, "HOME=" + home, "APPDATA=" + appData, "GIT_CONFIG_NOSYSTEM=1"}
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("git config query failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func hasNativeGitMarker(workdir, allowedBase string) (bool, error) {
	current := filepath.Clean(workdir)
	for {
		marker := filepath.Join(current, ".git")
		if _, err := os.Lstat(marker); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect Git repository marker %s: %w", marker, err)
		}
		if strings.EqualFold(current, allowedBase) {
			return false, nil
		}
		parent := filepath.Dir(current)
		if strings.EqualFold(parent, current) || !pathWithin(allowedBase, parent) {
			return false, nil
		}
		current = parent
	}
}

func runNativeGit(ctx context.Context, executable string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = []string{
		"SYSTEMROOT=" + os.Getenv("SystemRoot"),
		"WINDIR=" + os.Getenv("SystemRoot"),
		"PATH=" + filepath.Dir(executable),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + nativeGitNullPath,
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	}
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git metadata query failed: %w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("git metadata query failed: %w", err)
	}
	return output, nil
}

func nativeGitWorktreeListed(ctx context.Context, executable, selectedWorktree string) (bool, error) {
	args := []string{"-C", selectedWorktree, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + nativeGitNullPath, "-c", "core.quotePath=false", "worktree", "list", "--porcelain"}
	output, err := runNativeGit(ctx, executable, args...)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		path := strings.TrimPrefix(line, "worktree ")
		if filepath.IsAbs(path) && strings.EqualFold(filepath.Clean(path), filepath.Clean(selectedWorktree)) {
			return true, nil
		}
	}
	return false, nil
}

func configureNativeGitEnvironment(environment map[string]string, scope *nativeRepositoryScope, writeAccess bool) {
	environment["GIT_CONFIG_NOSYSTEM"] = "1"
	environment["GIT_CONFIG_GLOBAL"] = nativeGitNullPath
	environment["GIT_TERMINAL_PROMPT"] = "0"
	environment["GIT_OPTIONAL_LOCKS"] = "0"
	if !writeAccess {
		environment["GIT_OPTIONAL_LOCKS"] = "0"
	} else {
		delete(environment, "GIT_OPTIONAL_LOCKS")
	}
	keys := make([]string, 0)
	values := make([]string, 0)
	if scope != nil {
		keys = make([]string, 0, len(scope.SafeDirectories)+len(scope.CredentialHelpers)+4)
		values = make([]string, 0, len(scope.SafeDirectories)+len(scope.CredentialHelpers)+4)
		for _, directory := range scope.SafeDirectories {
			keys = append(keys, "safe.directory")
			values = append(values, filepath.Clean(directory))
		}
		if scope.IdentityName != "" {
			keys = append(keys, "user.name")
			values = append(values, scope.IdentityName)
		}
		if scope.IdentityEmail != "" {
			keys = append(keys, "user.email")
			values = append(values, scope.IdentityEmail)
		}
		for _, helperKey := range scope.CredentialHelpers {
			if !strings.EqualFold(helperKey, "credential.helper") {
				keys = append(keys, helperKey)
				values = append(values, "")
			}
		}
	}
	// Reset global and repository-scoped helper lists in command-scope config.
	// This keeps a project's .git/config from silently invoking a credential
	// executable under the shared sandbox account.
	keys = append(keys, "credential.helper")
	values = append(values, "")
	environment["GIT_CONFIG_COUNT"] = fmt.Sprintf("%d", len(keys))
	for i, key := range keys {
		suffix := fmt.Sprintf("%d", i)
		environment["GIT_CONFIG_KEY_"+suffix] = key
		environment["GIT_CONFIG_VALUE_"+suffix] = values[i]
	}
}
