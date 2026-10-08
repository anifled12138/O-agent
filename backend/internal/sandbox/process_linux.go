//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const maxSandboxInputBytes = 1 << 20
const maxSandboxRuntime = 180 * time.Second
const maxLongLivedSandboxRuntime = 24 * time.Hour

// Let a single scratch tmpfs use the task's full memory budget. The cgroup
// remains the aggregate enforcement boundary across the process and all of its
// private mounts; a smaller per-mount ceiling needlessly rejected large but
// otherwise valid temporary files.
const linuxSandboxScratchMaxBytes = linuxSandboxMemoryMaxBytes

// Run executes a process inside bubblewrap mount, user, PID, IPC, UTS and
// network namespaces. A missing or non-functional bubblewrap installation is
// an error; this path never falls back to an unsandboxed child process.
func Run(ctx context.Context, command *exec.Cmd, input []byte, policy Policy) (runErr, cleanupErr error) {
	return run(ctx, command, input, policy, maxSandboxRuntime)
}

// RunLongLived uses the same isolation and resource controls for trusted
// host-owned sessions whose lifetime is explicitly stopped by their owner.
func RunLongLived(ctx context.Context, command *exec.Cmd, input []byte, policy Policy) (runErr, cleanupErr error) {
	return run(ctx, command, input, policy, maxLongLivedSandboxRuntime)
}

func run(ctx context.Context, command *exec.Cmd, input []byte, policy Policy, maxRuntime time.Duration) (runErr, cleanupErr error) {
	if policy.Backend != "" && policy.Backend != BackendBubblewrap {
		return fmt.Errorf("unsupported Linux sandbox backend %q", policy.Backend), nil
	}
	if command == nil || strings.TrimSpace(command.Path) == "" {
		return errors.New("sandbox command path is required"), nil
	}
	if policy.Timeout <= 0 || policy.Timeout > maxRuntime {
		return fmt.Errorf("sandbox timeout must be between 1 ms and %s", maxRuntime), nil
	}
	if len(input) > maxSandboxInputBytes {
		return errors.New("sandbox command input exceeds the 1 MiB limit"), nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err, nil
	}
	bridge, err := startLinuxGitCredentialBridge(policy.GitCredentials, policy.GitCredentialURLs)
	if err != nil {
		return err, nil
	}
	if bridge != nil {
		defer func() { cleanupErr = errors.Join(cleanupErr, bridge.close()) }()
	}
	if policy.NetworkAccess {
		hostsFile, allowedIPs, cleanup, err := prepareLinuxNetworkAllowlist(ctx, policy.NetworkAllowHosts)
		if err != nil {
			return err, nil
		}
		policy.NetworkHostsFile = hostsFile
		policy.NetworkAllowIPs = allowedIPs
		defer func() { cleanupErr = errors.Join(cleanupErr, cleanup()) }()
	} else if len(policy.NetworkAllowHosts) > 0 {
		return errors.New("network destination hosts require network access"), nil
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return fmt.Errorf("bubblewrap is required for Linux command execution: %w", err), nil
	}
	if err := requireLinuxResourceControllers(); err != nil {
		return err, nil
	}
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		return fmt.Errorf("systemd-run is required to enforce Linux task resource limits: %w", err), nil
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return fmt.Errorf("systemctl is required to stop a Linux task scope on cancellation: %w", err), nil
	}
	if policy.NetworkAccess {
		if err := requireLinuxIPAddressFilter(ctx); err != nil {
			return err, nil
		}
	}
	helperExecutable := ""
	if bridge != nil {
		helperExecutable, err = os.Executable()
		if err != nil {
			return fmt.Errorf("resolve O executable for Git credential helper: %w", err), nil
		}
	}
	bwrapArgs, err := bubblewrapArgs(command, policy, bridge, helperExecutable)
	if err != nil {
		return err, nil
	}
	unit, args, err := systemdScopeArgs(bwrap, bwrapArgs, policy.Timeout, policy.NetworkAccess, policy.NetworkAllowIPs)
	if err != nil {
		return err, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, policy.Timeout)
	defer cancel()
	wrapped := exec.CommandContext(runCtx, systemdRun, args...)
	wrapped.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	wrapped.Dir = "/"
	wrapped.Stdin = command.Stdin
	if input != nil {
		wrapped.Stdin = bytes.NewReader(input)
	}
	wrapped.Stdout, wrapped.Stderr = command.Stdout, command.Stderr
	wrapped.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	err = wrapped.Run()
	command.ProcessState = wrapped.ProcessState
	if runCtx.Err() != nil {
		stopErr := stopLinuxTaskScope(systemctl, unit)
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return context.DeadlineExceeded, stopErr
		}
		return runCtx.Err(), stopErr
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return context.DeadlineExceeded, nil
	}
	return err, nil
}

func requireLinuxResourceControllers() error {
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return fmt.Errorf("cgroup v2 resource controllers are required for Linux sandbox execution: %w", err)
	}
	available := map[string]bool{}
	for _, name := range strings.Fields(string(controllers)) {
		available[name] = true
	}
	for _, required := range []string{"cpu", "memory", "pids"} {
		if !available[required] {
			return fmt.Errorf("cgroup v2 %s controller is unavailable; refusing unbounded Linux sandbox execution", required)
		}
	}
	return nil
}

func stopLinuxTaskScope(systemctl, unit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stop := exec.CommandContext(ctx, systemctl, "--user", "stop", unit)
	if err := stop.Run(); err == nil {
		return verifyLinuxTaskScopeStopped(ctx, systemctl, unit)
	} else {
		stopErr := err
		verifyErr := verifyLinuxTaskScopeStopped(ctx, systemctl, unit)
		if verifyErr == nil {
			return nil
		}
		return errors.Join(fmt.Errorf("stop Linux sandbox scope %s: %w", unit, stopErr), verifyErr)
	}
}

func verifyLinuxTaskScopeStopped(ctx context.Context, systemctl, unit string) error {
	showLoad := exec.CommandContext(ctx, systemctl, "--user", "show", "--property=LoadState", "--value", unit)
	loadState, err := showLoad.Output()
	if err != nil {
		return fmt.Errorf("verify Linux sandbox scope load state: %w", err)
	}
	load := strings.TrimSpace(string(loadState))
	if load == "" || load == "not-found" {
		return nil
	}
	showActive := exec.CommandContext(ctx, systemctl, "--user", "show", "--property=ActiveState", "--value", unit)
	activeState, err := showActive.Output()
	if err != nil {
		return fmt.Errorf("verify Linux sandbox scope active state: %w", err)
	}
	active := strings.TrimSpace(string(activeState))
	if active != "inactive" && active != "failed" {
		return fmt.Errorf("Linux sandbox scope %s remains %s (load state %s)", unit, active, load)
	}
	return nil
}

func bubblewrapArgs(command *exec.Cmd, policy Policy, bridge *linuxGitCredentialBridge, helperExecutable string) ([]string, error) {
	workdir, err := canonicalSandboxPath(command.Dir, true)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox working directory: %w", err)
	}
	readRoots, err := canonicalMountRoots(policy.ReadOnlyPaths)
	if err != nil {
		return nil, err
	}
	writeRoots, err := canonicalMountRoots(policy.WritePaths)
	if err != nil {
		return nil, err
	}
	for _, root := range writeRoots {
		if !rootIsCovered(root, readRoots) {
			readRoots = append(readRoots, root)
		}
	}
	if workdir != "/" && !isSystemPath(workdir) && !rootIsCovered(workdir, readRoots) {
		readRoots = append(readRoots, workdir)
	}
	readRoots = uniquePaths(readRoots)
	writeRoots = uniquePaths(writeRoots)
	guestPaths := guestMountPaths(append(append([]string(nil), readRoots...), writeRoots...))
	guestRoot := "/workspace"

	args := []string{"--unshare-all", "--die-with-parent", "--new-session"}
	if policy.NetworkAccess {
		args = append(args, "--share-net")
		if policy.NetworkHostsFile == "" || len(policy.NetworkAllowIPs) == 0 {
			return nil, errors.New("network access requires a resolved destination allow-list")
		}
	}
	args = append(args,
		"--ro-bind-try", "/usr", "/usr",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/sbin", "/sbin",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--ro-bind-try", "/etc", "/etc",
		"--dev", "/dev",
		"--proc", "/proc",
	)
	args = append(args, "--dir", guestRoot)
	directories := map[string]bool{"/": true, guestRoot: true, "/usr": true, "/bin": true, "/sbin": true, "/lib": true, "/lib64": true, "/etc": true, "/dev": true, "/proc": true}
	if policy.NetworkAccess {
		if err := appendBind(&args, "--ro-bind", policy.NetworkHostsFile, "/etc/hosts", directories); err != nil {
			return nil, fmt.Errorf("mount pinned network destination hosts: %w", err)
		}
	}
	for _, root := range readRoots {
		if rootIsCovered(root, writeRoots) {
			continue
		}
		if err := appendBind(&args, "--ro-bind", root, guestPaths[root], directories); err != nil {
			return nil, err
		}
	}
	for _, root := range writeRoots {
		if err := appendBind(&args, "--bind", root, guestPaths[root], directories); err != nil {
			return nil, err
		}
	}
	// Reapply narrower read-only roots after writable parents so nested policy
	// paths never inherit broader write access.
	for _, root := range readRoots {
		if rootIsCovered(root, writeRoots) && !pathInRoots(root, writeRoots) {
			if err := appendBind(&args, "--ro-bind", root, guestPaths[root], directories); err != nil {
				return nil, err
			}
		}
	}
	executablePath, err := canonicalSandboxPath(command.Path, false)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox executable: %w", err)
	}
	innerArgs := command.Args
	if !isSystemPath(executablePath) {
		guestExecutable := guestRoot + "/.o-agent-executable"
		if err := appendBind(&args, "--ro-bind", executablePath, guestExecutable, directories); err != nil {
			return nil, err
		}
		innerArgs = append([]string(nil), command.Args...)
		if len(innerArgs) == 0 {
			innerArgs = []string{guestExecutable}
		} else {
			innerArgs[0] = guestExecutable
		}
	}
	if err := appendLinuxHostMasks(&args, len(readRoots) > 0); err != nil {
		return nil, err
	}
	if bridge != nil {
		if !policy.NetworkAccess || policy.GitCredentials == nil || len(policy.GitCredentialURLs) == 0 {
			return nil, errors.New("Git credential bridge requires network access, a broker, and a repository scope")
		}
		canonicalHelper, err := canonicalSandboxPath(helperExecutable, false)
		if err != nil {
			return nil, fmt.Errorf("resolve O executable for Git credential helper: %w", err)
		}
		if _, statErr := os.Stat("/run"); errors.Is(statErr, os.ErrNotExist) {
			args = append(args, "--dir", "/run")
		} else if statErr != nil {
			return nil, fmt.Errorf("inspect sandbox runtime mount root: %w", statErr)
		}
		directories["/run"] = true // appendLinuxHostMasks has hidden host /run above.
		if err := appendBind(&args, "--bind", bridge.dir, linuxCredentialGuestDir, directories); err != nil {
			return nil, fmt.Errorf("mount private Git credential bridge: %w", err)
		}
		if err := appendBind(&args, "--ro-bind", canonicalHelper, linuxCredentialGuestDir+"/axiom", directories); err != nil {
			return nil, fmt.Errorf("mount Git credential helper executable: %w", err)
		}
	}
	if policy.PrivateTempWorkingDirectory {
		args = append(args, "--dir", "/tmp/o-agent-run", "--chdir", "/tmp/o-agent-run")
	} else {
		guestWorkdir := workdir
		if mapped, ok := mapGuestPath(workdir, guestPaths); ok {
			guestWorkdir = mapped
		}
		args = append(args, "--chdir", guestWorkdir)
	}
	args = append(args,
		"--clearenv",
		"--setenv", "PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--setenv", "HOME", "/tmp",
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "USER", "sandbox",
		"--setenv", "LOGNAME", "sandbox",
		"--setenv", "LANG", "C.UTF-8",
		"--setenv", "LC_ALL", "C.UTF-8",
	)
	if bridge != nil {
		args = append(args,
			"--setenv", "GIT_TERMINAL_PROMPT", "0",
			"--setenv", "GIT_CONFIG_COUNT", "3",
			"--setenv", "GIT_CONFIG_KEY_0", "credential.helper",
			"--setenv", "GIT_CONFIG_VALUE_0", "",
			"--setenv", "GIT_CONFIG_KEY_1", "credential.helper",
			"--setenv", "GIT_CONFIG_VALUE_1", "!/run/o-agent-git/axiom --git-credential-helper --socket /run/o-agent-git/credential.sock",
			"--setenv", "GIT_CONFIG_KEY_2", "credential.useHttpPath",
			"--setenv", "GIT_CONFIG_VALUE_2", "true",
		)
	}
	args = append(args, "--")
	if len(innerArgs) == 0 {
		innerArgs = []string{executablePath}
	}
	args = append(args, innerArgs...)
	return args, nil
}

func canonicalMountRoots(paths []string) ([]string, error) {
	roots := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved, err := canonicalSandboxPath(path, false)
		if err != nil {
			return nil, fmt.Errorf("resolve sandbox access root %q: %w", path, err)
		}
		roots = append(roots, resolved)
	}
	return uniquePaths(roots), nil
}

func canonicalSandboxPath(path string, directory bool) (string, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return "", errors.New("sandbox paths must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if directory && !info.IsDir() {
		return "", errors.New("sandbox working directory is not a directory")
	}
	if !directory && !info.IsDir() && !info.Mode().IsRegular() {
		return "", errors.New("sandbox mount must be a regular file or directory")
	}
	return resolved, nil
}

func appendBind(args *[]string, operation, source, target string, directories map[string]bool) error {
	if source == "/" || target == "/" {
		return errors.New("binding the host root into an Agent sandbox is forbidden")
	}
	parent := filepath.Dir(target)
	var parents []string
	for current := parent; current != "/" && current != "."; current = filepath.Dir(current) {
		parents = append(parents, current)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		if !directories[parents[i]] {
			*args = append(*args, "--dir", parents[i])
			directories[parents[i]] = true
		}
	}
	*args = append(*args, operation, source, target)
	// Bind mounts also make their destination available as a parent for later
	// nested grants. This prevents duplicate --dir operations in the namespace.
	directories[target] = true
	return nil
}

func guestMountPaths(roots []string) map[string]string {
	roots = uniquePaths(roots)
	result := make(map[string]string, len(roots))
	nextGrant := 0
	for _, root := range roots {
		parent := ""
		for _, candidate := range roots {
			if candidate == root || !pathWithin(candidate, root) {
				continue
			}
			if len(candidate) > len(parent) {
				parent = candidate
			}
		}
		if parent != "" {
			relative := strings.TrimPrefix(strings.TrimPrefix(root, parent), string(filepath.Separator))
			result[root] = filepath.Join(result[parent], relative)
			continue
		}
		if nextGrant == 0 {
			result[root] = "/workspace"
		} else {
			result[root] = fmt.Sprintf("/workspace/.grant-%d", nextGrant)
		}
		nextGrant++
	}
	return result
}

func mapGuestPath(path string, roots map[string]string) (string, bool) {
	bestRoot := ""
	for root := range roots {
		if pathWithin(root, path) && len(root) > len(bestRoot) {
			bestRoot = root
		}
	}
	if bestRoot == "" {
		return "", false
	}
	relative, err := filepath.Rel(bestRoot, path)
	if err != nil || relative == "." {
		return roots[bestRoot], err == nil
	}
	return filepath.Join(roots[bestRoot], relative), true
}

func isSystemPath(path string) bool {
	for _, root := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64"} {
		if pathWithin(root, path) {
			return true
		}
	}
	return false
}

func appendLinuxHostMasks(args *[]string, workspaceMounted bool) error {
	entries, err := os.ReadDir("/")
	if err != nil {
		return fmt.Errorf("list host root paths to mask: %w", err)
	}
	allowed := map[string]bool{"usr": true, "bin": true, "sbin": true, "lib": true, "lib64": true, "etc": true, "dev": true, "proc": true}
	if workspaceMounted {
		allowed["workspace"] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		if allowed[name] {
			continue
		}
		info, statErr := os.Stat(filepath.Join("/", name))
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspect host root path %q before masking: %w", name, statErr)
		}
		if info.IsDir() {
			*args = appendSizedTmpfs(*args, filepath.Join("/", name))
		}
	}
	for _, path := range []string{"/etc/o-agent", "/etc/ssh", "/etc/ssl/private", "/etc/NetworkManager/system-connections", "/etc/letsencrypt", "/etc/sudoers.d"} {
		var info os.FileInfo
		var statErr error
		if path == "/etc/o-agent" {
			info, statErr = os.Lstat(path)
			if statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
				return fmt.Errorf("host O service configuration path %q must be a real directory before sandboxing", path)
			}
		} else {
			info, statErr = os.Stat(path)
		}
		if statErr == nil && info.IsDir() {
			*args = appendSizedTmpfs(*args, path)
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect host credential directory %q before masking: %w", path, statErr)
		}
	}
	for _, path := range []string{"/etc/shadow", "/etc/gshadow", "/etc/sudoers", "/etc/krb5.keytab"} {
		*args = append(*args, "--ro-bind-try", "/dev/null", path)
	}
	return nil
}

func appendSizedTmpfs(args []string, path string) []string {
	return append(args, "--size", fmt.Sprintf("%d", linuxSandboxScratchMaxBytes), "--tmpfs", path)
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func rootIsCovered(path string, roots []string) bool {
	for _, root := range roots {
		if pathWithin(root, path) {
			return true
		}
	}
	return false
}

func pathInRoots(path string, roots []string) bool {
	for _, root := range roots {
		if root == path {
			return true
		}
	}
	return false
}

func uniquePaths(paths []string) []string {
	sort.Slice(paths, func(i, j int) bool {
		if len(paths[i]) == len(paths[j]) {
			return paths[i] < paths[j]
		}
		return len(paths[i]) < len(paths[j])
	})
	result := paths[:0]
	for _, path := range paths {
		if len(result) == 0 || result[len(result)-1] != path {
			result = append(result, path)
		}
	}
	return result
}

func LinuxStatus() NativeHealth {
	status := NativeHealth{Installation: "absent", Health: "unhealthy", Backend: string(BackendBubblewrap), CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		status.Reason = "bubblewrap is not installed or not on PATH"
		return status
	}
	status.Installation, status.Runner = "installed", bwrap
	testPath, err := exec.LookPath("test")
	if err != nil {
		status.Reason = "the Linux test executable is unavailable for sandbox isolation check"
		return status
	}
	probeDir, err := os.MkdirTemp(os.TempDir(), "o-agent-sandbox-probe-")
	if err != nil {
		status.Reason = fmt.Sprintf("create Linux sandbox isolation probe: %v", err)
		return status
	}
	probePath := filepath.Join(probeDir, "host-only-canary")
	if err := os.WriteFile(probePath, []byte("host-only"), 0o600); err != nil {
		status.Reason = fmt.Sprintf("write Linux sandbox isolation probe: %v", errors.Join(err, removeLinuxProbeFiles(probePath, probeDir)))
		return status
	}
	command := exec.Command(testPath, "-e", probePath)
	command.Dir = "/usr"
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr, cleanupErr := Run(context.Background(), command, nil, Policy{Backend: BackendBubblewrap, Timeout: 10 * time.Second})
	fileCleanupErr := removeLinuxProbeFiles(probePath, probeDir)
	if cleanupErr != nil || fileCleanupErr != nil {
		status.Reason = fmt.Sprintf("clean up Linux sandbox isolation probe: %v", errors.Join(cleanupErr, fileCleanupErr))
		return status
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1 || stderr.Len() != 0 || stdout.Len() != 0 {
		if runErr == nil {
			status.Reason = "sandboxed process can read a host-only canary outside its granted workspace"
		} else {
			status.Reason = fmt.Sprintf("bubblewrap isolation probe failed: %v %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return status
	}
	status.Health, status.Reason = "healthy", ""
	if err := requireLinuxIPAddressFilter(context.Background()); err != nil {
		status.NetworkEgressReason = err.Error()
	} else {
		status.NetworkEgressFiltering = true
	}
	return status
}

func removeLinuxProbeFiles(probePath, probeDir string) error {
	fileErr := os.Remove(probePath)
	if errors.Is(fileErr, os.ErrNotExist) {
		fileErr = nil
	}
	dirErr := os.Remove(probeDir)
	if errors.Is(dirErr, os.ErrNotExist) {
		dirErr = nil
	}
	if _, err := os.Lstat(probePath); !errors.Is(err, os.ErrNotExist) {
		fileErr = errors.Join(fileErr, err, errors.New("Linux sandbox probe canary remains on host"))
	}
	if _, err := os.Lstat(probeDir); !errors.Is(err, os.ErrNotExist) {
		dirErr = errors.Join(dirErr, err, errors.New("Linux sandbox probe directory remains on host"))
	}
	return errors.Join(fileErr, dirErr)
}

func NativeStatus(installDir, runnerPath string) NativeHealth {
	return NativeHealth{Installation: "absent", Health: "unhealthy", Backend: string(BackendWindowsNative), Reason: "Windows native sandbox is unavailable on Linux", Runner: runnerPath, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

func RecoverRunfiles(string) (RecoveryReport, error) { return RecoveryReport{}, nil }

func RecoverNativeJournals(string) (RecoveryReport, error) { return RecoveryReport{}, nil }

func WindowsSandboxSetupAvailable(string, string) bool { return false }

func WindowsSandboxSetupHelperAvailable(string) bool { return false }

func RunWindowsSandboxSetup(context.Context, string, string, string, string) error {
	return ErrUnavailable
}
