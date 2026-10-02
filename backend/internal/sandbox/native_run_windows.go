//go:build windows

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	nativeLogonWithProfile  = 1
	nativePipeBufferSize    = 256 << 10
	nativeRunnerStartLimit  = 30 * time.Second
	nativeRunnerStopLimit   = 10 * time.Second
	nativeWorkspaceLockWait = 100 * time.Millisecond
)

var nativeToolFingerprintCache = struct {
	sync.Mutex
	values map[string]string
}{values: make(map[string]string)}

func runNative(ctx context.Context, command *exec.Cmd, input []byte, policy Policy) (runErr, cleanupErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if command == nil {
		return errors.New("sandbox command is nil"), nil
	}
	if err := ctx.Err(); err != nil {
		return err, nil
	}
	if policy.InstallDir == "" || policy.RunnerPath == "" {
		return ErrNativeNotInstalled, nil
	}
	if command.Err != nil {
		return command.Err, nil
	}
	if command.Path == "" || command.Dir == "" {
		return errors.New("sandbox command path and working directory are required"), nil
	}
	if len(input) > maxNativeInput {
		return errors.New("sandbox command input exceeds the 1 MiB limit"), nil
	}
	if policy.Timeout <= 0 || policy.Timeout > 180*time.Second {
		return errors.New("sandbox command timeout must be between 1 ms and 180 seconds"), nil
	}

	status := NativeStatus(policy.InstallDir, policy.RunnerPath)
	if status.Health != "healthy" {
		return fmt.Errorf("Windows native sandbox health Probe failed: %s", status.Reason), nil
	}
	manifest, err := readNativeManifest(policy.InstallDir)
	if err != nil {
		return fmt.Errorf("read healthy native sandbox installation: %w", err), nil
	}
	if err := verifyInstallationOwner(manifest.OwnerSID); err != nil {
		return err, nil
	}
	credential := manifest.Offline
	if policy.NetworkAccess {
		credential = manifest.Online
	}
	password, err := unprotectSandboxPassword(credential.EncryptedPassword, manifest.OwnerSID)
	if err != nil {
		return fmt.Errorf("unlock %s sandbox account credential: %w", credential.Username, err), nil
	}
	defer zeroString(&password)

	if !filepath.IsAbs(command.Path) {
		resolved, err := exec.LookPath(command.Path)
		if err != nil {
			return fmt.Errorf("resolve sandbox command: %w", err), nil
		}
		command.Path = resolved
	}
	executable, err := canonicalExecutable(command.Path)
	if err != nil {
		return fmt.Errorf("resolve sandbox executable: %w", err), nil
	}
	if _, _, err := nativeFileIdentity(executable); err != nil {
		return fmt.Errorf("validate sandbox executable filesystem: %w", err), nil
	}
	if isSensitiveRuntimePath(filepath.Dir(executable)) || isBroadRuntimePath(filepath.Dir(executable)) {
		return errors.New("refusing to execute a program from a protected or broad user directory"), nil
	}
	workingDirectory, err := canonicalNativeDirectory(command.Dir)
	if err != nil {
		return fmt.Errorf("resolve sandbox working directory: %w", err), nil
	}
	writeAccess := len(policy.WritePaths) > 0
	allowedRoots := append(append([]string(nil), policy.ReadOnlyPaths...), policy.WritePaths...)
	repositoryScope, err := discoverNativeRepositoryScope(ctx, workingDirectory, allowedRoots, writeAccess)
	if err != nil {
		return fmt.Errorf("prepare Git repository scope: %w", err), nil
	}
	readRoots := append([]string(nil), policy.ReadOnlyPaths...)
	writePaths := append([]string(nil), policy.WritePaths...)
	if repositoryScope != nil {
		readRoots = append(readRoots, repositoryScope.ReadRoots...)
		writePaths = append(writePaths, repositoryScope.WriteRoots...)
	}
	for _, path := range writePaths {
		if _, err := canonicalNativeDirectory(path); err != nil {
			return fmt.Errorf("validate writable sandbox root %q: %w", path, err), nil
		}
	}
	if _, ok := os.LookupEnv("SystemRoot"); !ok {
		return errors.New("Windows SystemRoot is unavailable for sandbox command execution"), nil
	}
	if err := ctx.Err(); err != nil {
		return err, nil
	}

	writeLocks, err := acquireWorkspaceWriteLocks(ctx, manifest.OwnerSID, writePaths)
	if err != nil {
		return fmt.Errorf("acquire workspace write lease: %w", err), nil
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, releaseWorkspaceWriteLocks(writeLocks)) }()

	commandID, err := makeCommandID()
	if err != nil {
		return fmt.Errorf("generate sandbox command ID: %w", err), nil
	}
	installDir, err := checkedInstallDirectory(policy.InstallDir)
	if err != nil {
		return err, nil
	}
	if _, err := canonicalNativeDirectory(installDir); err != nil {
		return fmt.Errorf("validate sandbox installation directory: %w", err), nil
	}
	runnerPath, err := canonicalExecutable(policy.RunnerPath)
	if err != nil {
		return fmt.Errorf("resolve installed command runner: %w", err), nil
	}
	if _, _, err := nativeFileIdentity(runnerPath); err != nil {
		return fmt.Errorf("validate installed command runner filesystem: %w", err), nil
	}
	sessionsRoot := filepath.Join(installDir, "sessions")
	if err := os.MkdirAll(sessionsRoot, 0o700); err != nil {
		return fmt.Errorf("prepare sandbox session root: %w", err), nil
	}
	if err := secureNativeDirectory(sessionsRoot, manifest.OwnerSID); err != nil {
		return err, nil
	}
	scratch, err := os.MkdirTemp(sessionsRoot, "command-"+commandID+"-")
	if err != nil {
		return fmt.Errorf("create private command scratch: %w", err), nil
	}
	if err := securePrivateNativeDirectory(scratch, manifest.OwnerSID); err != nil {
		return errors.Join(err, removeScratch(scratch)), nil
	}
	scratch, err = canonicalNativeDirectory(scratch)
	if err != nil {
		return errors.Join(fmt.Errorf("resolve command scratch: %w", err), removeScratch(scratch)), nil
	}
	scratchVolume, scratchIndex, err := nativeFileIdentity(scratch)
	if err != nil {
		return errors.Join(fmt.Errorf("read private command scratch identity: %w", err), removeScratch(scratch)), nil
	}
	for _, name := range []string{"home", "temp", "cache", "localappdata", "appdata"} {
		if err := os.Mkdir(filepath.Join(scratch, name), 0o700); err != nil {
			return errors.Join(fmt.Errorf("create private command %s directory: %w", name, err), removeScratch(scratch)), nil
		}
	}
	tempDir := filepath.Join(scratch, "temp")
	goCache := filepath.Join(scratch, "cache")
	environment, err := commandEnvironment(command.Env, tempDir, goCache, filepath.Join(scratch, "localappdata"), policy.NetworkAccess)
	if err != nil {
		return errors.Join(fmt.Errorf("prepare sandbox command environment: %w", err), removeScratch(scratch)), nil
	}
	environment["HOME"] = filepath.Join(scratch, "home")
	environment["USERPROFILE"] = filepath.Join(scratch, "home")
	environment["APPDATA"] = filepath.Join(scratch, "appdata")
	environment["GOPATH"] = filepath.Join(scratch, "gopath")
	environment["GOMODCACHE"] = filepath.Join(scratch, "gopath", "pkg", "mod")
	toolCacheWriteRoots, err := configureNativeToolCaches(ctx, manifest.OwnerSID, environment)
	if err != nil {
		return errors.Join(fmt.Errorf("prepare persistent tool caches: %w", err), removeScratch(scratch)), nil
	}
	sshAgentEndpoint := ""
	sshAgentPipeName := ""
	var sshAgentKeys [][]byte
	if nativeGitNeedsSSHAgent(repositoryScope, policy.NetworkAccess) {
		sshAgentEndpoint, sshAgentKeys, err = probeHostSSHAgent()
		if err == nil {
			sshAgentPipeName = `\\.\pipe\OAgentSSHAgent-` + commandID
		} else {
			sshAgentEndpoint = ""
		}
	}
	defer clearSSHAgentKeys(sshAgentKeys)
	if policy.NetworkAccess && repositoryScope != nil && hasNativeSSHRemote(repositoryScope.RemoteURLs) && sshAgentEndpoint == "" {
		if err := prepareNativeSSHBlocker(environment, scratch); err != nil {
			return errors.Join(fmt.Errorf("prepare explicit SSH credential boundary: %w", err), removeScratch(scratch)), nil
		}
	}
	configureNativeGitEnvironment(environment, repositoryScope, writeAccess)
	if sshAgentEndpoint == "" && nativeSSHSigningConfigured(repositoryScope) {
		missingAgentPipe := `\\.\pipe\OAgentSSHAgent-` + commandID
		if nativeSSHSigningIdentityConfigured(repositoryScope) {
			if err := prepareNativeSSHSigningEnvironment(environment, scratch, missingAgentPipe,
				repositoryScope.IdentitySigningKey, repositoryScope.IdentityCommitSigning, repositoryScope.IdentityTagSigning); err != nil {
				return errors.Join(fmt.Errorf("preserve Host SSH signing requirements without exposing a private key: %w", err), removeScratch(scratch)), nil
			}
		} else {
			// Keep repository-local signing configuration effective, but ensure it
			// cannot fall back to the Host agent after the command lease is absent.
			environment["SSH_AUTH_SOCK"] = missingAgentPipe
		}
	}
	if sshAgentEndpoint != "" {
		if err := prepareNativeSSHAgentEnvironment(environment, scratch, sshAgentPipeName, sshAgentKeys,
			repositoryScope.IdentitySigningFormat, repositoryScope.IdentitySigningKey,
			repositoryScope.IdentityCommitSigning, repositoryScope.IdentityTagSigning); err != nil {
			return errors.Join(fmt.Errorf("prepare command-scoped Host SSH identity: %w", err), removeScratch(scratch)), nil
		}
	}
	credentialPipeName := ""
	credentialURLs := append([]string(nil), policy.GitCredentialURLs...)
	if repositoryScope != nil {
		credentialURLs = append(credentialURLs, repositoryScope.RemoteURLs...)
	}
	if policy.NetworkAccess && policy.GitCredentials != nil && len(credentialURLs) > 0 {
		credentialPipeName = `\\.\pipe\OAgentCredential-` + commandID
		if err := prepareNativeGitCredentialHelper(environment, scratch, runnerPath, credentialPipeName, commandID); err != nil {
			return errors.Join(fmt.Errorf("prepare command-scoped Git credential helper: %w", err), removeScratch(scratch)), nil
		}
	}
	profileDrive := filepath.VolumeName(environment["USERPROFILE"])
	environment["HOMEDRIVE"] = profileDrive
	environment["HOMEPATH"] = strings.TrimPrefix(environment["USERPROFILE"], profileDrive)
	for _, directory := range []string{environment["HOME"], environment["USERPROFILE"], environment["APPDATA"]} {
		if strings.TrimSpace(directory) != "" && !pathWithin(scratch, directory) {
			return errors.Join(errors.New("sandbox HOME directory escapes command scratch"), removeScratch(scratch)), nil
		}
	}

	readOnly := append([]string(nil), readRoots...)
	readOnly = append(readOnly, runtimeReadPaths(executable)...)
	for _, path := range environmentReadPaths(environment) {
		if !pathWithin(scratch, path) {
			readOnly = append(readOnly, path)
		}
	}
	readOnly = append(readOnly, nativeEnvironmentRuntimePaths(environment)...)
	for _, path := range append(append([]string(nil), readOnly...), writePaths...) {
		if _, err := canonicalNativeDirectory(path); err != nil {
			return errors.Join(fmt.Errorf("validate sandbox access root %q: %w", path, err), removeScratch(scratch)), nil
		}
	}
	allWritePaths := append(append([]string(nil), writePaths...), toolCacheWriteRoots...)
	accessPaths, err := normalizeAccessPaths(readOnly, allWritePaths)
	if err != nil {
		return errors.Join(fmt.Errorf("prepare sandbox filesystem policy: %w", err), removeScratch(scratch)), nil
	}
	accessPaths = append(accessPaths, accessPath{path: scratch, write: true})
	var protectedHostPaths []accessPath
	for _, raw := range policy.ProtectedPaths {
		protectedRoot, err := canonicalNativeDirectory(raw)
		if err != nil {
			return errors.Join(fmt.Errorf("validate protected Host data root %q: %w", raw, err), removeScratch(scratch)), nil
		}
		if filepath.VolumeName(protectedRoot) == protectedRoot || strings.EqualFold(protectedRoot, filepath.VolumeName(protectedRoot)+string(filepath.Separator)) {
			return errors.Join(fmt.Errorf("refusing to protect a drive root as Host data: %s", protectedRoot), removeScratch(scratch)), nil
		}
		for _, allowed := range accessPaths {
			if pathWithin(protectedRoot, allowed.path) && !strings.EqualFold(filepath.Clean(allowed.path), filepath.Clean(scratch)) {
				return errors.Join(fmt.Errorf("sandbox access root %s is inside protected Host data %s", allowed.path, protectedRoot), removeScratch(scratch)), nil
			}
		}
		protectedHostPaths = append(protectedHostPaths, accessPath{path: protectedRoot, deny: true})
	}
	protectedGitPaths := make(map[string]nativeACLRecord)
	var protectedAccessPaths []accessPath
	if repositoryScope != nil {
		for _, record := range repositoryScope.Protected {
			protectedAccessPaths = append(protectedAccessPaths, accessPath{path: record.Path, write: record.Write, deny: record.Deny})
			protectedGitPaths[strings.ToLower(filepath.Clean(record.Path))] = record
		}
	}
	// Place explicit protections before their containing grant roots. Cleanup
	// then removes inherited root grants first and the explicit deny entries last.
	accessPaths = append(protectedHostPaths, append(protectedAccessPaths, accessPaths...)...)
	if !policy.PrivateTempWorkingDirectory && !directoryCovered(workingDirectory, accessPaths) {
		return errors.Join(fmt.Errorf("sandbox policy does not grant access to the working directory %s", workingDirectory), removeScratch(scratch)), nil
	}
	if policy.PrivateTempWorkingDirectory {
		workingDirectory = tempDir
	}

	ownerStart, err := currentProcessCreationTime()
	if err != nil {
		return errors.Join(fmt.Errorf("read sandbox host process identity: %w", err), removeScratch(scratch)), nil
	}
	journalDir, err := nativeJournalDirectory(installDir, manifest.OwnerSID)
	if err != nil {
		return errors.Join(err, removeScratch(scratch)), nil
	}
	journalPath := filepath.Join(journalDir, "command-"+commandID+".json")
	journal := nativeCommandJournal{
		Version: 4, CommandID: commandID, OwnerPID: os.Getpid(),
		OwnerStart: uint64(ownerStart.HighDateTime)<<32 | uint64(ownerStart.LowDateTime),
		AccountSID: credential.SID, ScratchDir: scratch, ScratchVolume: scratchVolume,
		ScratchIndex: scratchIndex, Stage: "prepared", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	journal.ACLs = make([]nativeACLRecord, 0, len(accessPaths))
	for _, path := range accessPaths {
		record, protected := protectedGitPaths[strings.ToLower(filepath.Clean(path.path))]
		if !protected {
			record, err = nativeIdentityRecord(path.path, path.write, path.deny)
		}
		if err != nil {
			return errors.Join(fmt.Errorf("prepare filesystem grant journal: %w", err), removeScratch(scratch)), nil
		}
		journal.ACLs = append(journal.ACLs, record)
	}
	setupMutex, releaseSetup, err := acquireSetupLockContext(ctx, manifest.OwnerSID)
	if err != nil {
		return errors.Join(fmt.Errorf("wait for sandbox maintenance before command start: %w", err), removeScratch(scratch)), nil
	}
	setupHeld := true
	finishSetupLock := func() error {
		if !setupHeld {
			return nil
		}
		setupHeld = false
		return releaseSetup(setupMutex)
	}
	latestManifest, err := readNativeManifest(installDir)
	if err != nil {
		return errors.Join(fmt.Errorf("recheck sandbox installation under maintenance lock: %w", err), finishSetupLock(), removeScratch(scratch)), nil
	}
	if latestManifest.OwnerSID != manifest.OwnerSID || latestManifest.RunnerSHA256 != manifest.RunnerSHA256 || latestManifest.Offline.SID != manifest.Offline.SID || latestManifest.Online.SID != manifest.Online.SID {
		return errors.Join(errors.New("sandbox installation changed while preparing the command"), finishSetupLock(), removeScratch(scratch)), nil
	}
	journalScopeRoots := make([]string, 0, len(accessPaths))
	for _, access := range accessPaths {
		if !equalWindowsPath(access.path, scratch) {
			journalScopeRoots = append(journalScopeRoots, access.path)
		}
	}
	if err := rejectConflictingNativeJournalGrants(installDir, latestManifest, journalScopeRoots); err != nil {
		return errors.Join(err, finishSetupLock(), removeScratch(scratch)), nil
	}
	commandLease, err := acquireNativeCommandLease(manifest.OwnerSID, commandID)
	if err != nil {
		return errors.Join(fmt.Errorf("acquire command lifecycle lease: %w", err), finishSetupLock(), removeScratch(scratch)), nil
	}
	releaseCommandLease := func() error {
		if commandLease == 0 {
			return nil
		}
		handle := commandLease
		commandLease = 0
		return releaseNativeCommandLease(handle)
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, releaseCommandLease()) }()
	if err := writeNativeJournal(journalPath, journal, manifest.OwnerSID); err != nil {
		return errors.Join(err, finishSetupLock(), releaseCommandLease(), removeScratch(scratch)), nil
	}
	journalWritten := true
	if err := finishSetupLock(); err != nil {
		journalWritten = false
		removeErr := removeNativeJournal(journalPath)
		return errors.Join(fmt.Errorf("release sandbox setup lock after command journal: %w", err), removeErr, releaseCommandLease(), removeScratch(scratch)), nil
	}
	var aclAttempts []nativeACLRecord
	var pipeHandle windows.Handle
	var pipeFile *os.File
	var runner windows.ProcessInformation
	var runnerJob windows.Handle
	var stopCredentialBroker func() error
	var sshAgentBridge *nativeSSHAgentBridge
	var runnerStart uint64
	watchErrors := make(chan error, 1)
	runnerExited := true
	runnerTreeStopped := true
	defer func() {
		if stopCredentialBroker != nil {
			cleanupErr = errors.Join(cleanupErr, stopCredentialBroker())
		}
		if sshAgentBridge != nil {
			cleanupErr = errors.Join(cleanupErr, sshAgentBridge.stopAndWait())
		}
		if runnerJob != 0 {
			if !runnerTreeStopped {
				wait, waitErr := windows.WaitForSingleObject(runnerJob, 0)
				var termErr error
				if wait != windows.WAIT_OBJECT_0 {
					termErr = windows.TerminateJobObject(runnerJob, 1)
					wait, waitErr = windows.WaitForSingleObject(runnerJob, uint32(nativeRunnerStopLimit.Milliseconds()))
				}
				if wait != windows.WAIT_OBJECT_0 {
					waitErr = errors.Join(waitErr, fmt.Errorf("runner process tree did not exit during cleanup (wait=%d)", wait))
				}
				cleanupErr = errors.Join(cleanupErr, termErr, waitErr)
				runnerTreeStopped = wait == windows.WAIT_OBJECT_0
			}
			if err := windows.CloseHandle(runnerJob); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close runner Job Object: %w", err))
			}
			runnerJob = 0
		}
		if runner.Process != 0 {
			wait, err := windows.WaitForSingleObject(runner.Process, 0)
			if err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("verify runner process exit: %w", err))
			} else if wait == windows.WAIT_OBJECT_0 {
				runnerExited = true
			} else if !runnerTreeStopped {
				cleanupErr = errors.Join(cleanupErr, errors.New("runner process tree is still active; retain native sandbox journal and grants for recovery"))
			}
		}
		if pipeFile != nil {
			cleanupErr = errors.Join(cleanupErr, pipeFile.Close())
		}
		if pipeHandle != 0 && pipeHandle != windows.InvalidHandle {
			cleanupErr = errors.Join(cleanupErr, windows.CloseHandle(pipeHandle))
		}
		if runner.Thread != 0 {
			cleanupErr = errors.Join(cleanupErr, windows.CloseHandle(runner.Thread))
		}
		if runner.Process != 0 {
			cleanupErr = errors.Join(cleanupErr, windows.CloseHandle(runner.Process))
		}
		var cleanupLogonSID *windows.SID
		if journal.LogonSID != "" {
			logonSID, sidErr := windows.StringToSid(journal.LogonSID)
			if sidErr != nil {
				cleanupErr = errors.Join(cleanupErr, sidErr)
			} else if runnerTreeStopped {
				cleanupLogonSID = logonSID
				if journal.Stage != "cleaning" {
					journal.Stage = "cleaning"
					if err := writeNativeJournal(journalPath, journal, manifest.OwnerSID); err != nil {
						cleanupErr = errors.Join(cleanupErr, fmt.Errorf("persist sandbox cleaning stage: %w", err))
					}
				}
				if err := revokeNativeSIDTree(journalPath, &journal, logonSID, manifest.OwnerSID); err != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("revoke command logon SID from writable trees: %w", err))
				}
				for i := len(aclAttempts) - 1; i >= 0; i-- {
					grant := aclAttempts[i]
					if i < len(journal.ACLs) && journal.ACLs[i].Revoked {
						continue
					}
					if err := setNativeAccess(grant.Path, logonSID, windows.REVOKE_ACCESS, grant.Write, grant.Deny, grant.VolumeSerial, grant.FileIndex); err != nil {
						cleanupErr = errors.Join(cleanupErr, fmt.Errorf("revoke command logon SID from %s: %w", grant.Path, err))
						continue
					}
					if i < len(journal.ACLs) {
						journal.ACLs[i].Revoked = true
						if err := writeNativeJournal(journalPath, journal, manifest.OwnerSID); err != nil {
							cleanupErr = errors.Join(cleanupErr, fmt.Errorf("persist ACL revocation for %s: %w", grant.Path, err))
						}
					}
				}
			} else {
				cleanupErr = errors.Join(cleanupErr, errors.New("runner is still active; retain native sandbox journal and grants for recovery"))
			}
		}
		if runnerTreeStopped && cleanupErr == nil && cleanupLogonSID != nil {
			if err := verifyNativeSIDAbsentRoots(journal.ACLs, cleanupLogonSID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("verify command logon SID removal from writable trees: %w", err))
			}
		}
		if runnerTreeStopped && cleanupErr == nil {
			if err := removeScratchVerified(scratch, scratchVolume, scratchIndex); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		if journalWritten && runnerTreeStopped && cleanupErr == nil && journal.Stage != "finished" {
			journal.Stage = "finished"
			if err := writeNativeJournal(journalPath, journal, manifest.OwnerSID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("persist finished sandbox command state: %w", err))
			}
		}
		if journalWritten && runnerTreeStopped && cleanupErr == nil {
			if err := removeNativeJournal(journalPath); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		select {
		case watcherErr := <-watchErrors:
			cleanupErr = errors.Join(cleanupErr, watcherErr)
		default:
		}
	}()

	pipeName := `\\.\pipe\OAgentSandbox-` + commandID
	runnerJob, err = newJobObjectWithLimit(maxProcessesPerJob + 1)
	if err != nil {
		return fmt.Errorf("create host runner Job Object: %w", err), nil
	}
	runner, err = startNativeRunner(runnerPath, filepath.Dir(runnerPath), pipeName, commandID, credential.Username, password, runnerJob)
	if err != nil {
		return fmt.Errorf("start sandbox runner account %q: %w", credential.Username, err), nil
	}
	runnerExited = false
	runnerTreeStopped = false
	startTime, err := processCreationTime(runner.Process)
	if err != nil {
		return fmt.Errorf("read sandbox runner process start time: %w", err), nil
	}
	runnerStart = uint64(startTime.HighDateTime)<<32 | uint64(startTime.LowDateTime)
	runnerUserSID, runnerLogonSID, err := nativeProcessTokenIdentity(runner.Process)
	if err != nil {
		return fmt.Errorf("inspect new runner logon identity: %w", err), nil
	}
	if runnerUserSID != credential.SID || runnerLogonSID == "" {
		return errors.New("new runner process does not have the selected sandbox account and logon SID"), nil
	}
	pipeHandle, err = createNativeControlPipe(pipeName, manifest.OwnerSID, runnerLogonSID)
	if err != nil {
		return fmt.Errorf("create per-command sandbox control pipe: %w", err), nil
	}
	if _, err := windows.ResumeThread(runner.Thread); err != nil {
		return fmt.Errorf("resume sandbox runner after securing its command pipe: %w", err), nil
	}

	type connectResult struct {
		file *os.File
		err  error
	}
	connected := make(chan connectResult, 1)
	go func(handle windows.Handle) {
		err := windows.ConnectNamedPipe(handle, nil)
		if errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			err = nil
		}
		if err != nil {
			connected <- connectResult{err: err}
			return
		}
		connected <- connectResult{file: os.NewFile(uintptr(handle), "oagent-native-control")}
	}(pipeHandle)
	startTimer := time.NewTimer(nativeRunnerStartLimit)
	defer startTimer.Stop()
	select {
	case result := <-connected:
		if result.err != nil {
			return fmt.Errorf("accept sandbox runner control pipe: %w", result.err), nil
		}
		pipeFile = result.file
		pipeHandle = 0
	case <-ctx.Done():
		return ctx.Err(), nil
	case <-startTimer.C:
		return errors.New("sandbox runner did not connect to its control pipe before timeout"), nil
	}
	hello, err := readNativeFrame(pipeFile)
	if err != nil {
		return fmt.Errorf("read sandbox runner handshake: %w", err), nil
	}
	logonSID, err := authenticateNativeRunner(pipeFile, runner.Process, hello, commandID, credential.SID, runnerStart)
	if err != nil {
		return fmt.Errorf("authenticate sandbox runner: %w", err), nil
	}
	journal.RunnerPID = runner.ProcessId
	journal.RunnerStart = runnerStart
	journal.LogonSID = logonSID.String()
	journal.RestrictedSIDs = append([]string(nil), hello.RestrictedSIDs...)
	journal.EnabledPrivileges = append([]string(nil), hello.EnabledPrivileges...)
	if err := writeNativeJournal(journalPath, journal, manifest.OwnerSID); err != nil {
		return fmt.Errorf("record authenticated runner in journal: %w", err), nil
	}
	if err := restrictNativeControlPipe(pipeFile, manifest.OwnerSID, credential.SID, logonSID); err != nil {
		return fmt.Errorf("restrict authenticated runner control pipe: %w", err), nil
	}
	for _, grant := range journal.ACLs {
		aclAttempts = append(aclAttempts, grant)
		if err := setNativeAccess(grant.Path, logonSID, windows.SET_ACCESS, grant.Write, grant.Deny, grant.VolumeSerial, grant.FileIndex); err != nil {
			return fmt.Errorf("grant command logon SID access to %s: %w", grant.Path, err), nil
		}
	}
	journal.Stage = "granted"
	if err := writeNativeJournal(journalPath, journal, manifest.OwnerSID); err != nil {
		return fmt.Errorf("persist granted sandbox command state: %w", err), nil
	}
	if credentialPipeName != "" {
		stopCredentialBroker, err = startNativeGitCredentialBroker(credentialPipeName, commandID, manifest.OwnerSID, credential.SID, logonSID, policy.GitCredentials, credentialURLs)
		if err != nil {
			return fmt.Errorf("start command-scoped Git credential broker: %w", err), nil
		}
	}
	if sshAgentEndpoint != "" {
		sshAgentBridge, err = startNativeSSHAgentBridge(sshAgentPipeName, sshAgentEndpoint, commandID, manifest.OwnerSID, credential.SID, logonSID, sshAgentKeys)
		if err != nil {
			return fmt.Errorf("start command-scoped Host SSH identity broker: %w", err), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err, nil
	}
	arguments := append([]string(nil), command.Args...)
	if len(arguments) == 0 {
		arguments = []string{executable}
	} else {
		arguments[0] = executable
	}
	plan := &nativeExecutionPlan{
		Version: nativeProtocolVersion, CommandID: commandID, CommandPath: executable,
		InstallationVersion: manifest.RunnerVersion,
		Arguments:           arguments, WorkingDirectory: workingDirectory,
		Environment: environmentEntries(environment), Input: append([]byte(nil), input...),
		ReadRoots: accessReadRootStrings(accessPaths), WriteRoots: accessRootStrings(accessPaths, true),
		NetworkAccess: policy.NetworkAccess, GitCredentialPipe: credentialPipeName, PrivateTempWorkingDirectory: policy.PrivateTempWorkingDirectory,
		TimeoutMS: policy.Timeout.Milliseconds(),
	}
	journal.Stage = "running"
	if err := writeNativeJournal(journalPath, journal, manifest.OwnerSID); err != nil {
		return fmt.Errorf("persist running sandbox command state: %w", err), nil
	}
	if err := writeNativeFrame(pipeFile, nativeFrame{Version: nativeProtocolVersion, Type: "start", CommandID: commandID, Plan: plan}); err != nil {
		return fmt.Errorf("authorize sandbox command start: %w", err), nil
	}

	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	watcherStopped := false
	stopWatching := func() {
		if !watcherStopped {
			close(stopWatcher)
			watcherStopped = true
			<-watcherDone
		}
	}
	go func(job windows.Handle) {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			if err := windows.TerminateJobObject(job, 1); err != nil {
				watchErrors <- fmt.Errorf("terminate runner Job Object after cancellation: %w", err)
			}
		case <-stopWatcher:
		}
	}(runnerJob)
	defer stopWatching()
	var stdout, stderr io.Writer = command.Stdout, command.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	var exitFrame *nativeFrame
	for {
		type frameResult struct {
			frame nativeFrame
			err   error
		}
		frameRead := make(chan frameResult, 1)
		go func() {
			frame, err := readNativeFrame(pipeFile)
			frameRead <- frameResult{frame: frame, err: err}
		}()
		var frame nativeFrame
		select {
		case result := <-frameRead:
			frame = result.frame
			err = result.err
		case <-ctx.Done():
			return ctx.Err(), nil
		}
		readErr := err
		if readErr != nil {
			if ctx.Err() != nil {
				return ctx.Err(), nil
			}
			return fmt.Errorf("read sandbox runner output: %w", readErr), nil
		}
		if frame.Version != nativeProtocolVersion || frame.CommandID != commandID {
			return errors.New("sandbox runner sent a frame for a different protocol or command"), nil
		}
		switch frame.Type {
		case "output":
			if len(frame.Data) == 0 || len(frame.Data) > nativeChunkSize || (frame.Stream != "stdout" && frame.Stream != "stderr") {
				return errors.New("sandbox runner sent an invalid output frame"), nil
			}
			target := stdout
			if frame.Stream == "stderr" {
				target = stderr
			}
			if _, err := target.Write(frame.Data); err != nil {
				return fmt.Errorf("capture sandbox %s: %w", frame.Stream, err), nil
			}
		case "exit":
			if exitFrame != nil || frame.Error != "" {
				return errors.New("sandbox runner sent a malformed exit frame"), nil
			}
			exitFrame = &frame
			goto commandFinished
		case "error":
			return fmt.Errorf("sandbox runner failed: %s", frame.Error), nil
		default:
			return fmt.Errorf("sandbox runner sent unexpected frame type %q", frame.Type), nil
		}
	}

commandFinished:
	stopWatching()
	if pipeFile != nil {
		if err := pipeFile.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close sandbox control pipe: %w", err))
		}
		pipeFile = nil
	}
	wait, waitErr := windows.WaitForSingleObject(runner.Process, uint32(nativeRunnerStopLimit.Milliseconds()))
	if waitErr != nil || wait != windows.WAIT_OBJECT_0 {
		if waitErr == nil {
			waitErr = fmt.Errorf("runner did not exit after command result (wait=%d)", wait)
		}
		termErr := windows.TerminateProcess(runner.Process, 1)
		wait, waitErr = windows.WaitForSingleObject(runner.Process, uint32(nativeRunnerStopLimit.Milliseconds()))
		if wait != windows.WAIT_OBJECT_0 {
			waitErr = errors.Join(waitErr, fmt.Errorf("runner did not exit after termination (wait=%d)", wait))
		}
		cleanupErr = errors.Join(cleanupErr, termErr, waitErr)
	}
	runnerExited = wait == windows.WAIT_OBJECT_0
	if runnerExited {
		jobWait, jobWaitErr := windows.WaitForSingleObject(runnerJob, uint32(nativeRunnerStopLimit.Milliseconds()))
		if jobWaitErr != nil || jobWait != windows.WAIT_OBJECT_0 {
			if jobWaitErr == nil {
				jobWaitErr = fmt.Errorf("runner Job Object remained active after runner exit (wait=%d)", jobWait)
			}
			cleanupErr = errors.Join(cleanupErr, jobWaitErr, windows.TerminateJobObject(runnerJob, 1))
		} else {
			runnerTreeStopped = true
		}
	}
	if ctx.Err() != nil {
		runErr = errors.Join(ctx.Err(), processExitError{code: exitFrame.ExitCode})
	} else if exitFrame.TimedOut {
		runErr = errors.Join(context.DeadlineExceeded, processExitError{code: exitFrame.ExitCode})
	} else if exitFrame.ExitCode != 0 {
		runErr = processExitError{code: exitFrame.ExitCode}
	}
	if runnerExited {
		var runnerExit uint32
		if err := windows.GetExitCodeProcess(runner.Process, &runnerExit); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("read runner exit status: %w", err))
		} else if runnerExit != 0 {
			runErr = errors.Join(runErr, fmt.Errorf("sandbox runner exited with code %d after reporting the command result", runnerExit))
		}
	}
	return runErr, cleanupErr
}

func verifyInstallationOwner(ownerSID string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current Windows user SID: %w", err)
	}
	if user.User.Sid.String() != ownerSID {
		return fmt.Errorf("native sandbox belongs to Windows user %s, current user is %s", ownerSID, user.User.Sid.String())
	}
	return nil
}

func restrictNativeControlPipe(pipe *os.File, ownerSID, accountSID string, logonSID *windows.SID) error {
	sddl := fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)(A;;GRGW;;;%s)", ownerSID, logonSID.String())
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	handle := windows.Handle(pipe.Fd())
	if err := windows.SetKernelObjectSecurity(handle, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, descriptor); err != nil {
		return err
	}
	readBack, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read back control pipe security descriptor: %w", err)
	}
	text := strings.ToUpper(readBack.String())
	if !strings.Contains(text, strings.ToUpper(logonSID.String())) || strings.Contains(text, strings.ToUpper(accountSID)) {
		return errors.New("control pipe ACL read-back does not restrict access to the authenticated logon SID")
	}
	return nil
}

func securePrivateNativeDirectory(path, ownerSID string) error {
	sddl := fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", ownerSID)
	return setNativePathDACL(path, sddl)
}

func createNativeControlPipe(name, ownerSID, logonSID string) (windows.Handle, error) {
	sddl := fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)(A;;GRGW;;;%s)", ownerSID, logonSID)
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, fmt.Errorf("build private runner control pipe ACL: %w", err)
	}
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateNamedPipe(name16, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, nativePipeBufferSize, nativePipeBufferSize, 0, &security)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return 0, err
	}
	return handle, nil
}

func startNativeRunner(runnerPath, workingDirectory, pipeName, commandID, username, password string, job windows.Handle) (windows.ProcessInformation, error) {
	user16, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	domain16, err := windows.UTF16PtrFromString(".")
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	password16, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	defer clearUTF16(password16)
	app16, err := windows.UTF16PtrFromString(runnerPath)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	cmd16, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{runnerPath, "--pipe", pipeName, "--command-id", commandID}))
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	working16, err := windows.UTF16PtrFromString(workingDirectory)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	systemRoot := os.Getenv("SystemRoot")
	runnerEnv := map[string]string{"SYSTEMROOT": systemRoot, "WINDIR": systemRoot, "PATH": filepath.Join(systemRoot, "System32"), "PATHEXT": ".COM;.EXE;.BAT;.CMD"}
	envBlock, err := encodeEnvironment(runnerEnv)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Flags: windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE}
	var process windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW | windows.CREATE_SUSPENDED)
	result, _, callErr := procCreateProcessWithLogon.Call(
		uintptr(unsafe.Pointer(user16)), uintptr(unsafe.Pointer(domain16)), uintptr(unsafe.Pointer(password16)), nativeLogonWithProfile,
		uintptr(unsafe.Pointer(app16)), uintptr(unsafe.Pointer(cmd16)), uintptr(flags),
		uintptr(unsafe.Pointer(&envBlock[0])), uintptr(unsafe.Pointer(working16)), uintptr(unsafe.Pointer(&startup)), uintptr(unsafe.Pointer(&process)),
	)
	runtime.KeepAlive(password16)
	runtime.KeepAlive(envBlock)
	if result == 0 {
		return windows.ProcessInformation{}, fmt.Errorf("CreateProcessWithLogonW: %w", callErr)
	}
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		termErr := windows.TerminateProcess(process.Process, 1)
		_, waitErr := windows.WaitForSingleObject(process.Process, windows.INFINITE)
		closeErr := errors.Join(windows.CloseHandle(process.Thread), windows.CloseHandle(process.Process))
		return windows.ProcessInformation{}, errors.Join(fmt.Errorf("assign runner to host Job Object: %w", err), termErr, waitErr, closeErr)
	}
	return process, nil
}

func nativeProcessTokenIdentity(process windows.Handle) (resultUserSID, resultLogonSID string, resultErr error) {
	token, err := openProcessToken(process)
	if err != nil {
		return "", "", err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(windows.Handle(token))) }()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", "", fmt.Errorf("read runner process user SID: %w", err)
	}
	groups, err := token.GetTokenGroups()
	if err != nil {
		return "", "", fmt.Errorf("read runner process logon SID: %w", err)
	}
	logonSID := tokenLogonSID(groups)
	if logonSID == nil || !logonSID.IsValid() {
		return "", "", errors.New("runner process token has no valid logon SID")
	}
	return user.User.Sid.String(), logonSID.String(), nil
}

func authenticateNativeRunner(pipe *os.File, runnerProcess windows.Handle, hello nativeFrame, commandID, expectedAccountSID string, expectedStart uint64) (resultSID *windows.SID, resultErr error) {
	if hello.Version != nativeProtocolVersion || hello.Type != "hello" || hello.CommandID != commandID || hello.ProcessID == 0 || hello.UserSID == "" || hello.LogonSID == "" {
		return nil, errors.New("runner handshake is missing identity fields or uses an unsupported protocol")
	}
	if !equalStringLists(hello.RestrictedSIDs, []string{expectedAccountSID, hello.LogonSID, "S-1-1-0"}) || !equalStringLists(hello.EnabledPrivileges, []string{"SeChangeNotifyPrivilege"}) {
		return nil, errors.New("runner handshake does not report the approved restricted token baseline")
	}
	var clientPID uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(pipe.Fd()), &clientPID); err != nil {
		return nil, fmt.Errorf("read control pipe client process ID: %w", err)
	}
	if clientPID != hello.ProcessID {
		return nil, errors.New("runner handshake process ID does not match the named pipe client")
	}
	expectedPID, err := windows.GetProcessId(runnerProcess)
	if err != nil {
		return nil, fmt.Errorf("read host-started runner process ID: %w", err)
	}
	if clientPID != expectedPID {
		return nil, errors.New("named pipe client is not the process started by the host")
	}
	created, err := processCreationTime(runnerProcess)
	if err != nil {
		return nil, err
	}
	actualStart := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if actualStart != expectedStart || hello.ProcessStart != actualStart {
		return nil, errors.New("runner process start time does not match the authenticated host process")
	}
	token, err := openProcessToken(runnerProcess)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(windows.Handle(token))) }()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user.User.Sid.String() != expectedAccountSID || hello.UserSID != expectedAccountSID {
		return nil, fmt.Errorf("runner account SID mismatch: expected %s, got %s", expectedAccountSID, user.User.Sid.String())
	}
	if token.IsElevated() {
		return nil, errors.New("sandbox runner unexpectedly has an elevated token")
	}
	groups, err := token.GetTokenGroups()
	if err != nil {
		return nil, err
	}
	logonSID := tokenLogonSID(groups)
	if logonSID == nil || logonSID.String() != hello.LogonSID {
		return nil, errors.New("runner handshake logon SID does not match its process token")
	}
	return windows.StringToSid(logonSID.String())
}

func openProcessToken(process windows.Handle) (windows.Token, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return 0, fmt.Errorf("open runner process token: %w", err)
	}
	return token, nil
}

func acquireWorkspaceWriteLocks(ctx context.Context, ownerSID string, paths []string) ([]windows.Handle, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	// Windows mutex ownership belongs to an OS thread. Keep the goroutine on
	// this thread until all workspace leases acquired below have been released.
	runtime.LockOSThread()
	threadPinned := true
	unlockThread := func() {
		if threadPinned {
			threadPinned = false
			runtime.UnlockOSThread()
		}
	}
	canonical := make([]string, 0, len(paths))
	for _, path := range paths {
		resolved, err := canonicalDirectory(path)
		if err != nil {
			unlockThread()
			return nil, fmt.Errorf("resolve writable workspace lock root %q: %w", path, err)
		}
		canonical = append(canonical, resolved)
	}
	sort.Slice(canonical, func(i, j int) bool { return strings.ToLower(canonical[i]) < strings.ToLower(canonical[j]) })
	unique := canonical[:0]
	for _, path := range canonical {
		if len(unique) == 0 || !strings.EqualFold(unique[len(unique)-1], path) {
			unique = append(unique, path)
		}
	}
	locks := make([]windows.Handle, 0, len(unique))
	rollback := func(err error) ([]windows.Handle, error) {
		cleanupErr := releaseWorkspaceWriteLockHandles(locks)
		unlockThread()
		return nil, errors.Join(err, cleanupErr)
	}
	for _, path := range unique {
		name := `Global\OAgentSandboxWrite_` + wfpDigest(ownerSID+"\x00"+strings.ToLower(path))
		name16, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return rollback(err)
		}
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return rollback(err)
		}
		descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + user.User.Sid.String() + ")")
		if err != nil {
			return rollback(err)
		}
		security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
		mutex, _, callErr := procCreateMutex.Call(uintptr(unsafe.Pointer(&security)), 0, uintptr(unsafe.Pointer(name16)))
		runtime.KeepAlive(descriptor)
		if mutex == 0 {
			return rollback(fmt.Errorf("create workspace write mutex: %w", callErr))
		}
		handle := windows.Handle(mutex)
		for {
			if err := ctx.Err(); err != nil {
				return rollback(errors.Join(err, windows.CloseHandle(handle)))
			}
			wait, err := windows.WaitForSingleObject(handle, uint32(nativeWorkspaceLockWait.Milliseconds()))
			if err != nil {
				return rollback(errors.Join(fmt.Errorf("wait for workspace write mutex: %w", err), windows.CloseHandle(handle)))
			}
			switch wait {
			case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
				locks = append(locks, handle)
				goto acquired
			case uint32(windows.WAIT_TIMEOUT):
			default:
				return rollback(errors.Join(fmt.Errorf("unexpected workspace write lock result %d", wait), windows.CloseHandle(handle)))
			}
		}
	acquired:
	}
	return locks, nil
}

func releaseWorkspaceWriteLocks(locks []windows.Handle) error {
	if len(locks) == 0 {
		return nil
	}
	defer runtime.UnlockOSThread()
	return releaseWorkspaceWriteLockHandles(locks)
}

func releaseWorkspaceWriteLockHandles(locks []windows.Handle) error {
	var joined error
	for i := len(locks) - 1; i >= 0; i-- {
		if result, _, err := procReleaseMutex.Call(uintptr(locks[i])); result == 0 {
			joined = errors.Join(joined, fmt.Errorf("release workspace write mutex: %w", err))
		}
		joined = errors.Join(joined, windows.CloseHandle(locks[i]))
	}
	return joined
}
func environmentEntries(environment map[string]string) []string {
	values := make([]string, 0, len(environment))
	for key, value := range environment {
		values = append(values, key+"="+value)
	}
	sort.Strings(values)
	return values
}

func accessRootStrings(paths []accessPath, writeOnly bool) []string {
	var result []string
	for _, path := range paths {
		if !path.deny && path.write == writeOnly {
			result = append(result, path.path)
		}
	}
	sort.Strings(result)
	return result
}

func accessReadRootStrings(paths []accessPath) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if !path.deny {
			result = append(result, path.path)
		}
	}
	sort.Strings(result)
	return result
}

func configureNativeToolCaches(ctx context.Context, ownerSID string, environment map[string]string) (roots []string, resultErr error) {
	setupMutex, releaseSetup, err := acquireSetupLockContext(ctx, ownerSID)
	if err != nil {
		return nil, fmt.Errorf("wait for sandbox maintenance before preparing tool caches: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, releaseSetup(setupMutex)) }()
	goExecutable := resolveNativePathExecutable(environment, "go.exe", "GOROOT")
	if goExecutable != "" {
		version, err := nativeToolCacheFingerprint(goExecutable)
		if err != nil {
			return nil, fmt.Errorf("identify Go toolchain for cache isolation: %w", err)
		}
		buildCache, err := nativePersistentToolCache(ownerSID, "go", version, "build")
		if err != nil {
			return nil, err
		}
		moduleCache, err := nativePersistentToolCache(ownerSID, "go", version, "modules")
		if err != nil {
			return nil, err
		}
		environment["GOCACHE"] = buildCache
		environment["GOMODCACHE"] = moduleCache
		roots = append(roots, buildCache, moduleCache)
	}
	if nodeExecutable := resolveNativePathExecutable(environment, "node.exe", ""); nodeExecutable != "" {
		version, err := nativeToolCacheFingerprint(nodeExecutable)
		if err != nil {
			return nil, fmt.Errorf("identify Node toolchain for cache isolation: %w", err)
		}
		npmCache, err := nativePersistentToolCache(ownerSID, "node", version, "npm")
		if err != nil {
			return nil, err
		}
		yarnCache, err := nativePersistentToolCache(ownerSID, "node", version, "yarn")
		if err != nil {
			return nil, err
		}
		environment["NPM_CONFIG_CACHE"] = npmCache
		environment["YARN_CACHE_FOLDER"] = yarnCache
		roots = append(roots, npmCache, yarnCache)
	}
	if pythonExecutable := resolveNativePathExecutable(environment, "python.exe", ""); pythonExecutable != "" {
		version, err := nativeToolCacheFingerprint(pythonExecutable)
		if err != nil {
			return nil, fmt.Errorf("identify Python toolchain for cache isolation: %w", err)
		}
		pipCache, err := nativePersistentToolCache(ownerSID, "python", version, "pip")
		if err != nil {
			return nil, err
		}
		environment["PIP_CACHE_DIR"] = pipCache
		roots = append(roots, pipCache)
	}
	return roots, nil
}

func nativePersistentToolCache(ownerSID, tool, version, cacheName string) (string, error) {
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve current Windows user's local cache directory: %w", err)
	}
	ownerDigest := sha256.Sum256([]byte(ownerSID))
	cacheRoot := filepath.Join(cacheBase, "OAgentSandboxCache")
	ownerRoot := filepath.Join(cacheRoot, hex.EncodeToString(ownerDigest[:]))
	toolRoot := filepath.Join(ownerRoot, tool)
	versionRoot := filepath.Join(toolRoot, version)
	directory := filepath.Join(versionRoot, cacheName)
	for _, path := range []string{cacheRoot, ownerRoot, toolRoot, versionRoot, directory} {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create %s tool cache directory %s: %w", tool, path, err)
		}
		if _, err := canonicalNativeDirectory(path); err != nil {
			return "", fmt.Errorf("validate %s tool cache directory %s: %w", tool, path, err)
		}
		beforeVolume, beforeIndex, err := nativeFileIdentity(path)
		if err != nil {
			return "", fmt.Errorf("read %s tool cache directory identity %s: %w", tool, path, err)
		}
		if err := securePrivateNativeDirectory(path, ownerSID); err != nil {
			return "", fmt.Errorf("secure %s tool cache directory %s: %w", tool, path, err)
		}
		afterVolume, afterIndex, err := nativeFileIdentity(path)
		if err != nil {
			return "", fmt.Errorf("verify secured %s tool cache directory %s: %w", tool, path, err)
		}
		if beforeVolume != afterVolume || beforeIndex != afterIndex {
			return "", fmt.Errorf("%s tool cache directory changed while securing %s", tool, path)
		}
	}
	return directory, nil
}

func nativeToolCacheFingerprint(executable string) (string, error) {
	info, err := os.Stat(executable)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 256<<20 {
		return "", fmt.Errorf("tool binary size %d is outside the supported cache identity limit", info.Size())
	}
	if _, _, err := nativeFileIdentity(executable); err != nil {
		return "", fmt.Errorf("validate tool binary for cache identity: %w", err)
	}
	key := strings.ToLower(filepath.Clean(executable)) + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	nativeToolFingerprintCache.Lock()
	defer nativeToolFingerprintCache.Unlock()
	if fingerprint := nativeToolFingerprintCache.values[key]; fingerprint != "" {
		return fingerprint, nil
	}
	fingerprint, err := sha256File(executable)
	if err != nil {
		return "", fmt.Errorf("hash tool binary for cache identity: %w", err)
	}
	nativeToolFingerprintCache.values[key] = fingerprint
	return fingerprint, nil
}

func resolveNativePathExecutable(environment map[string]string, executableName, rootEnvironment string) string {
	var candidates []string
	if root := strings.TrimSpace(environment[rootEnvironment]); rootEnvironment != "" && root != "" {
		candidates = append(candidates, filepath.Join(root, "bin", executableName))
	}
	for _, directory := range filepath.SplitList(environment["PATH"]) {
		if strings.TrimSpace(directory) != "" {
			candidates = append(candidates, filepath.Join(strings.Trim(directory, `"`), executableName))
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			if info.Size() <= 0 || info.Size() > 256<<20 {
				continue
			}
			if _, _, err := nativeFileIdentity(candidate); err != nil {
				continue
			}
			if resolved, err := canonicalExecutable(candidate); err == nil {
				return resolved
			}
		}
	}
	return ""
}

func removeScratch(path string) error {
	if path == "" {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("private command scratch remains after cleanup: %s", path)
	}
	return nil
}

func removeScratchVerified(path string, volume uint32, index uint64) error {
	actualVolume, actualIndex, err := nativeFileIdentity(path)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify command scratch before cleanup: %w", err)
	}
	if actualVolume != volume || actualIndex != index {
		return errors.New("refuse to remove command scratch after its filesystem identity changed")
	}
	return removeScratch(path)
}

func removeNativeJournal(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove completed sandbox command journal: %w", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("completed sandbox command journal remains after cleanup: %s", path)
	}
	return nil
}

func verifySIDPresent(path string, sid *windows.SID) error {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read granted DACL %s: %w", path, err)
	}
	if descriptor == nil || !strings.Contains(strings.ToUpper(descriptor.String()), strings.ToUpper(sid.String())) {
		return fmt.Errorf("command logon SID was not present in the DACL for %s", path)
	}
	return nil
}
