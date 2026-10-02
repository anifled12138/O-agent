//go:build windows

package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	procThreadAttributeSecurityCapabilities = 0x00020009
	maxProcessesPerJob                      = 128
)

var (
	profileAPI        = windows.NewLazySystemDLL("userenv.dll")
	createProfileProc = profileAPI.NewProc("CreateAppContainerProfile")
	deleteProfileProc = profileAPI.NewProc("DeleteAppContainerProfile")
	getFolderProc     = profileAPI.NewProc("GetAppContainerFolderPath")
	coTaskMemProc     = windows.NewLazySystemDLL("ole32.dll").NewProc("CoTaskMemFree")
	aclMutationMu     sync.Mutex
	degradedMu        sync.Mutex
	sandboxDegraded   error
)

type securityCapabilities struct {
	AppContainerSID *windows.SID
	Capabilities    *windows.SIDAndAttributes
	CapabilityCount uint32
	Reserved        uint32
}

type accessGrant struct {
	path             string
	sid              *windows.SID
	write            bool
	oldIntegritySDDL string
	hadOriginalLabel bool
}

type accessPath struct {
	path             string
	write            bool
	deny             bool
	oldIntegritySDDL string
	hadOriginalLabel bool
}

type cleanupJournal struct {
	Version         int            `json:"version"`
	PID             int            `json:"pid"`
	ProcessStart    uint64         `json:"processStart"`
	ProfileName     string         `json:"profileName"`
	AppContainerSID string         `json:"appContainerSid"`
	LoopbackExempt  bool           `json:"loopbackExempt,omitempty"`
	Grants          []journalGrant `json:"grants"`
}

type journalGrant struct {
	Path             string `json:"path"`
	Write            bool   `json:"write,omitempty"` // Legacy mandatory-label journal field.
	ACLWrite         bool   `json:"aclWrite,omitempty"`
	OldIntegritySDDL string `json:"oldIntegritySddl,omitempty"`
	HadIntegrity     bool   `json:"hadIntegrity"`
}

type appContainerProfile struct {
	name string
	sid  *windows.SID
}

type processExitError struct {
	code uint32
}

func (e processExitError) Error() string { return fmt.Sprintf("process exited with code %d", e.code) }
func (e processExitError) ExitCode() int { return int(e.code) }

// Run dispatches to a host-selected backend and fails closed for unknown values.
// Empty keeps the AppContainer migration backend until native acceptance.
func Run(ctx context.Context, command *exec.Cmd, input []byte, policy Policy) (runErr, cleanupErr error) {
	if len(input) > 1<<20 {
		return errors.New("sandbox command input exceeds the 1 MiB limit"), nil
	}
	if policy.PowerShellExitWrapper {
		wrapped, err := wrapPowerShellInput(input)
		if err != nil {
			return err, nil
		}
		input = wrapped
	}
	switch policy.Backend {
	case "", BackendAppContainer:
		return runAppContainer(ctx, command, input, policy)
	case BackendWindowsNative:
		return runNative(ctx, command, input, policy)
	default:
		return fmt.Errorf("unsupported sandbox backend %q", policy.Backend), nil
	}
}

// runAppContainer retains the existing backend as an explicit migration option.
func runAppContainer(ctx context.Context, command *exec.Cmd, input []byte, policy Policy) (runErr, cleanupErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if command == nil {
		return errors.New("sandbox command is nil"), nil
	}
	if err := ctx.Err(); err != nil {
		return err, nil
	}
	if err := currentDegradedError(); err != nil {
		return err, nil
	}
	if command.Err != nil {
		return command.Err, nil
	}
	if command.Path == "" {
		return errors.New("sandbox command path is empty"), nil
	}
	if command.Dir == "" {
		return errors.New("sandbox command working directory is empty"), nil
	}

	if policy.JournalPath == "" {
		return errors.New("sandbox cleanup journal path is required"), nil
	}

	profile, err := createAppContainerProfile()
	if err != nil {
		return nil, err
	}
	var appContainerFolder, tempDir string
	var grants []accessGrant
	var journalCreated bool
	var loopbackExemption bool
	defer func() {
		var tempCleanupErr error
		if tempDir != "" {
			if err := os.RemoveAll(tempDir); err != nil {
				tempCleanupErr = errors.Join(tempCleanupErr, fmt.Errorf("remove AppContainer temporary directory: %w", err))
			}
		}
		var accessRestoreErr error
		for attempt := 0; attempt < 2; attempt++ {
			if len(grants) > 0 {
				accessRestoreErr = revokeAccessGrants(grants)
			}
			if accessRestoreErr == nil {
				break
			}
		}
		var loopbackRestoreErr error
		if loopbackExemption {
			for attempt := 0; attempt < 2; attempt++ {
				loopbackRestoreErr = setAppContainerLoopbackExemption(profile.sid.String(), false)
				if loopbackRestoreErr == nil {
					break
				}
			}
		}
		var profileCleanupErr error
		if accessRestoreErr == nil && loopbackRestoreErr == nil {
			for attempt := 0; attempt < 2; attempt++ {
				profileCleanupErr = profile.close()
				if profileCleanupErr == nil {
					break
				}
			}
		}
		if tempDir != "" {
			if _, err := os.Lstat(tempDir); err == nil {
				tempCleanupErr = errors.Join(tempCleanupErr, errors.New("AppContainer temporary directory still exists after profile cleanup"))
			} else if errors.Is(err, os.ErrNotExist) {
				tempCleanupErr = nil
			} else {
				tempCleanupErr = errors.Join(tempCleanupErr, fmt.Errorf("verify AppContainer temporary directory cleanup: %w", err))
			}
		}
		if accessRestoreErr != nil || loopbackRestoreErr != nil {
			degradedMu.Lock()
			sandboxDegraded = errors.Join(sandboxDegraded, accessRestoreErr, loopbackRestoreErr)
			degradedMu.Unlock()
		}
		restoreErr := errors.Join(accessRestoreErr, loopbackRestoreErr, profileCleanupErr)
		if journalCreated && restoreErr == nil {
			if err := removeCleanupJournal(policy.JournalPath); err != nil {
				restoreErr = errors.Join(restoreErr, err)
			}
		}
		cleanupErr = errors.Join(cleanupErr, tempCleanupErr, restoreErr)
	}()
	journal, err := newCleanupJournal(profile, nil)
	if err != nil {
		return fmt.Errorf("prepare sandbox profile journal: %w", err), nil
	}
	journalCreated = true
	if err := writeCleanupJournal(policy.JournalPath, journal); err != nil {
		return fmt.Errorf("persist sandbox profile journal: %w", err), nil
	}
	appContainerFolder, err = getAppContainerFolderPath(profile.sid)
	if err != nil {
		return fmt.Errorf("resolve AppContainer profile folder: %w", err), nil
	}
	appContainerFolder, err = canonicalDirectory(appContainerFolder)
	if err != nil {
		return fmt.Errorf("resolve canonical AppContainer profile folder: %w", err), nil
	}
	tempRoot := filepath.Join(appContainerFolder, "Temp")
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		return fmt.Errorf("prepare AppContainer temporary folder: %w", err), nil
	}
	tempRoot, err = canonicalDirectory(tempRoot)
	if err != nil {
		return fmt.Errorf("resolve AppContainer temporary folder: %w", err), nil
	}
	if !pathWithin(appContainerFolder, tempRoot) {
		return errors.New("AppContainer temporary folder resolves outside its profile"), nil
	}
	tempDir, err = os.MkdirTemp(tempRoot, "axiom-command-")
	if err != nil {
		return fmt.Errorf("create private AppContainer temporary directory: %w", err), nil
	}
	candidateTempDir := tempDir
	resolvedTempDir, err := canonicalDirectory(candidateTempDir)
	if err != nil {
		return fmt.Errorf("resolve AppContainer temporary directory: %w", err), nil
	}
	if !pathWithin(tempRoot, resolvedTempDir) {
		return errors.New("AppContainer command directory resolves outside its temporary folder"), nil
	}
	tempDir = resolvedTempDir
	goCache := filepath.Join(tempDir, "go-cache")
	if err := os.MkdirAll(goCache, 0o700); err != nil {
		return fmt.Errorf("create private Go build cache: %w", err), nil
	}
	environment, err := commandEnvironment(command.Env, tempDir, goCache, appContainerFolder, policy.NetworkAccess)
	if err != nil {
		return fmt.Errorf("prepare sandbox command environment: %w", err), nil
	}
	environmentBlock, err := encodeEnvironment(environment)
	if err != nil {
		return fmt.Errorf("prepare sandbox environment: %w", err), nil
	}

	readOnly := append([]string(nil), policy.ReadOnlyPaths...)
	readOnly = append(readOnly, runtimeReadPaths(command.Path)...)
	readOnly = append(readOnly, environmentReadPaths(environment)...)
	accessPaths, err := normalizeAccessPaths(readOnly, policy.WritePaths)
	if err != nil {
		return fmt.Errorf("prepare sandbox filesystem policy: %w", err), nil
	}
	if policy.PrivateTempWorkingDirectory {
		command.Dir = tempDir
	}
	workingDirectory, err := canonicalDirectory(command.Dir)
	if err != nil {
		return fmt.Errorf("resolve sandbox working directory: %w", err), nil
	}
	if !directoryCovered(workingDirectory, accessPaths) && !pathWithin(tempDir, workingDirectory) {
		return fmt.Errorf("sandbox policy does not grant access to the working directory %s", workingDirectory), nil
	}
	journal, err = newCleanupJournal(profile, accessPaths)
	if err != nil {
		return fmt.Errorf("prepare sandbox cleanup journal: %w", err), nil
	}
	loopbackExemption = policy.NetworkAccess && proxyNeedsLoopbackExemption(environment)
	journal.LoopbackExempt = loopbackExemption
	if err := writeCleanupJournal(policy.JournalPath, journal); err != nil {
		return fmt.Errorf("persist sandbox cleanup journal: %w", err), nil
	}
	journalCreated = true
	grants, err = applyAccessGrants(accessPaths, profile.sid)
	if err != nil {
		return err, nil
	}
	if loopbackExemption {
		if err := setAppContainerLoopbackExemption(profile.sid.String(), true); err != nil {
			return fmt.Errorf("enable configured local proxy for sandbox command: %w", err), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err, nil
	}
	processCtx := ctx
	var timeoutCancel context.CancelFunc
	if policy.Timeout > 0 {
		processCtx, timeoutCancel = context.WithTimeout(ctx, policy.Timeout)
		defer timeoutCancel()
	}
	input, err = stageAppContainerPowerShellInput(command, input, tempDir)
	if err != nil {
		return fmt.Errorf("stage private PowerShell input: %w", err), nil
	}
	runErr, cleanupErr = runAppContainerProcess(processCtx, command, input, environmentBlock, profile.sid, policy.NetworkAccess)
	return runErr, cleanupErr
}

// AppContainer PowerShell hosts do not reliably consume redirected stdin on
// every supported Windows build, and Restricted execution policy can reject
// -File. Keep the source in the private command directory and use a short
// -EncodedCommand loader to evaluate it in memory. Other commands keep stdin.
func stageAppContainerPowerShellInput(command *exec.Cmd, input []byte, tempDir string) ([]byte, error) {
	if command == nil || len(input) == 0 || len(command.Args) < 3 {
		return input, nil
	}
	if !strings.EqualFold(filepath.Base(command.Path), "powershell.exe") && !strings.EqualFold(filepath.Base(command.Path), "pwsh.exe") {
		return input, nil
	}
	last := len(command.Args)
	if !strings.EqualFold(command.Args[last-2], "-Command") || command.Args[last-1] != "-" {
		return input, nil
	}
	if strings.TrimSpace(tempDir) == "" || !filepath.IsAbs(tempDir) {
		return input, errors.New("private PowerShell temporary directory must be absolute")
	}
	path := filepath.Join(tempDir, "command-input.ps1")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return input, fmt.Errorf("create private PowerShell input: %w", err)
	}
	written, writeErr := file.Write(input)
	if writeErr == nil && written != len(input) {
		writeErr = io.ErrShortWrite
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return input, errors.Join(err, os.Remove(path))
	}
	quotedPath := strings.ReplaceAll(path, "'", "''")
	quotedTempDir := strings.ReplaceAll(tempDir, "'", "''")
	loader := "$env:TEMP = '" + quotedTempDir + "'\n" +
		"$env:TMP = '" + quotedTempDir + "'\n" +
		"$__oagentSource = [System.IO.File]::ReadAllText('" + quotedPath + "')\n" +
		"& ([ScriptBlock]::Create($__oagentSource))\n"
	encodedUnits := utf16.Encode([]rune(loader))
	encodedBytes := make([]byte, len(encodedUnits)*2)
	for index, unit := range encodedUnits {
		binary.LittleEndian.PutUint16(encodedBytes[index*2:], unit)
	}
	encodedLoader := base64.StdEncoding.EncodeToString(encodedBytes)
	command.Args = append(append([]string(nil), command.Args[:last-2]...), "-EncodedCommand", encodedLoader)
	return nil, nil
}

func createAppContainerProfile() (*appContainerProfile, error) {
	if err := profileAPI.Load(); err != nil {
		return nil, fmt.Errorf("load Windows AppContainer APIs: %w", err)
	}
	if err := createProfileProc.Find(); err != nil {
		return nil, fmt.Errorf("Windows AppContainer creation API is unavailable: %w", err)
	}
	var entropy [12]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return nil, fmt.Errorf("generate sandbox identity: %w", err)
	}
	name := "AxiomAgent-" + hex.EncodeToString(entropy[:])
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	displayName, err := windows.UTF16PtrFromString("O agent command")
	if err != nil {
		return nil, fmt.Errorf("encode sandbox display name: %w", err)
	}
	description, err := windows.UTF16PtrFromString("Per-command filesystem and network isolation")
	if err != nil {
		return nil, fmt.Errorf("encode sandbox description: %w", err)
	}
	var sid *windows.SID
	hresult, _, _ := createProfileProc.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(displayName)),
		uintptr(unsafe.Pointer(description)),
		0,
		0,
		uintptr(unsafe.Pointer(&sid)),
	)
	if hresult != 0 {
		var cleanupErr error
		if sid != nil {
			cleanupErr = errors.Join(cleanupErr, windows.FreeSid(sid))
		}
		cleanupErr = errors.Join(cleanupErr, deleteAppContainerProfile(name))
		return nil, errors.Join(fmt.Errorf("create Windows AppContainer profile failed (HRESULT 0x%08X)", uint32(hresult)), cleanupErr)
	}
	if sid == nil {
		return nil, errors.Join(errors.New("create Windows AppContainer profile returned no SID"), deleteAppContainerProfile(name))
	}
	return &appContainerProfile{name: name, sid: sid}, nil
}

func getAppContainerFolderPath(sid *windows.SID) (string, error) {
	if sid == nil {
		return "", errors.New("AppContainer SID is nil")
	}
	if err := getFolderProc.Find(); err != nil {
		return "", fmt.Errorf("Windows AppContainer folder API is unavailable: %w", err)
	}
	if err := coTaskMemProc.Find(); err != nil {
		return "", fmt.Errorf("Windows task memory cleanup API is unavailable: %w", err)
	}
	sidPtr, err := windows.UTF16PtrFromString(sid.String())
	if err != nil {
		return "", fmt.Errorf("encode AppContainer SID: %w", err)
	}
	var folderPtr *uint16
	hresult, _, _ := getFolderProc.Call(uintptr(unsafe.Pointer(sidPtr)), uintptr(unsafe.Pointer(&folderPtr)))
	if hresult != 0 {
		if folderPtr != nil {
			coTaskMemProc.Call(uintptr(unsafe.Pointer(folderPtr)))
		}
		return "", fmt.Errorf("get Windows AppContainer folder failed (HRESULT 0x%08X)", uint32(hresult))
	}
	if folderPtr == nil {
		return "", errors.New("get Windows AppContainer folder returned no path")
	}
	folder := windows.UTF16PtrToString(folderPtr)
	coTaskMemProc.Call(uintptr(unsafe.Pointer(folderPtr)))
	if folder == "" {
		return "", errors.New("get Windows AppContainer folder returned an empty path")
	}
	return filepath.Clean(folder), nil
}

func currentDegradedError() error {
	degradedMu.Lock()
	defer degradedMu.Unlock()
	if sandboxDegraded == nil {
		return nil
	}
	return fmt.Errorf("sandbox cleanup previously failed; restart the Agent service to recover permissions: %w", sandboxDegraded)
}

func (p *appContainerProfile) close() error {
	if p == nil {
		return nil
	}
	var cleanupErr error
	if p.name != "" {
		if err := deleteAppContainerProfile(p.name); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			p.name = ""
		}
	}
	if p.sid != nil {
		if err := windows.FreeSid(p.sid); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("free Windows AppContainer SID: %w", err))
		} else {
			p.sid = nil
		}
	}
	return cleanupErr
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return filepath.Clean(resolved), nil
}

func normalizeAccessPaths(readOnly, writable []string) ([]accessPath, error) {
	byPath := map[string]accessPath{}
	add := func(raw string, write bool) error {
		if strings.TrimSpace(raw) == "" {
			return errors.New("sandbox path is empty")
		}
		path, err := canonicalDirectory(raw)
		if err != nil {
			return fmt.Errorf("resolve allowed path %q: %w", raw, err)
		}
		if filepath.VolumeName(path) == path || strings.EqualFold(path, filepath.VolumeName(path)+string(filepath.Separator)) {
			return fmt.Errorf("refusing to grant access to a drive root: %s", path)
		}
		if write && (isBroadRuntimePath(path) || isSensitiveRuntimePath(path) || isSystemRuntimePath(path)) {
			return fmt.Errorf("refusing to grant workspace write access to a protected or broad path: %s", path)
		}
		key := strings.ToLower(path)
		prior := byPath[key]
		prior.path = path
		prior.write = prior.write || write
		byPath[key] = prior
		return nil
	}
	for _, raw := range readOnly {
		if err := add(raw, false); err != nil {
			return nil, err
		}
	}
	for _, raw := range writable {
		if err := add(raw, true); err != nil {
			return nil, err
		}
	}
	paths := make([]accessPath, 0, len(byPath))
	for _, path := range byPath {
		if path.write {
			label, hadLabel, err := readIntegrityLabel(path.path)
			if err != nil {
				return nil, fmt.Errorf("read original workspace integrity label for %s: %w", path.path, err)
			}
			path.oldIntegritySDDL, path.hadOriginalLabel = label, hadLabel
		}
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i].path) < len(paths[j].path) })
	return paths, nil
}

func applyAccessGrants(paths []accessPath, sid *windows.SID) ([]accessGrant, error) {
	grants := make([]accessGrant, 0, len(paths))
	for _, path := range paths {
		if err := setAccess(path.path, sid, windows.SET_ACCESS, path.write); err != nil {
			// Only successfully applied grants belong in the rollback list. The failed
			// ACL operation is returned directly, without a speculative revoke.
			return grants, fmt.Errorf("grant AppContainer read/execute access to %s: %w", path.path, err)
		}
		grant := accessGrant{path: path.path, sid: sid, write: path.write, oldIntegritySDDL: path.oldIntegritySDDL, hadOriginalLabel: path.hadOriginalLabel}
		grants = append(grants, grant)
		if path.write {
			if err := setLowIntegrityLabel(path.path); err != nil {
				return grants, fmt.Errorf("allow the AppContainer to write workspace path %s: %w", path.path, err)
			}
		}
	}
	return grants, nil
}

func readIntegrityLabel(path string) (string, bool, error) {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return "", false, err
	}
	if descriptor == nil {
		return "", false, errors.New("workspace path has no security descriptor")
	}
	_, _, err = descriptor.SACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	sddl := descriptor.String()
	if strings.TrimSpace(sddl) == "" {
		return "", false, errors.New("encode original workspace integrity label")
	}
	return sddl, true, nil
}

func setLowIntegrityLabel(path string) error {
	descriptor, err := windows.SecurityDescriptorFromString("S:(ML;OICI;NW;;;LW)")
	if err != nil {
		return fmt.Errorf("prepare temporary low-integrity workspace label: %w", err)
	}
	label, _, err := descriptor.SACL()
	if err != nil {
		return fmt.Errorf("read temporary workspace integrity label: %w", err)
	}
	if label == nil {
		return errors.New("temporary workspace integrity label is missing")
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, label); err != nil {
		return fmt.Errorf("set temporary workspace integrity label: %w", err)
	}
	return nil
}

func restoreIntegrityLabel(path, oldSDDL string, hadLabel bool) error {
	var oldLabel *windows.ACL
	if hadLabel {
		descriptor, err := windows.SecurityDescriptorFromString(oldSDDL)
		if err != nil {
			return fmt.Errorf("decode original workspace integrity label: %w", err)
		}
		oldLabel, _, err = descriptor.SACL()
		if err != nil {
			return fmt.Errorf("read original workspace integrity label: %w", err)
		}
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, oldLabel); err != nil {
		return fmt.Errorf("restore original workspace integrity label: %w", err)
	}
	return nil
}

func newCleanupJournal(profile *appContainerProfile, paths []accessPath) (cleanupJournal, error) {
	sid := profile.sid.String()
	created, err := currentProcessCreationTime()
	if err != nil {
		return cleanupJournal{}, fmt.Errorf("read host process creation time: %w", err)
	}
	processStart := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	journal := cleanupJournal{Version: 1, PID: os.Getpid(), ProcessStart: processStart, ProfileName: profile.name, AppContainerSID: sid}
	for _, path := range paths {
		journal.Grants = append(journal.Grants, journalGrant{
			Path:             path.path,
			ACLWrite:         path.write,
			OldIntegritySDDL: path.oldIntegritySDDL,
			HadIntegrity:     path.hadOriginalLabel,
		})
	}
	return journal, nil
}

func currentProcessCreationTime() (windows.Filetime, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getpid()))
	if err != nil {
		return windows.Filetime{}, err
	}
	created, timeErr := processCreationTime(process)
	closeErr := windows.CloseHandle(process)
	if timeErr != nil || closeErr != nil {
		return windows.Filetime{}, errors.Join(timeErr, closeErr)
	}
	return created, nil
}

func writeCleanupJournal(path string, journal cleanupJournal) (resultErr error) {
	if !strings.HasPrefix(filepath.Base(path), ".axiom-sandbox-") || filepath.Ext(path) != ".json" {
		return errors.New("sandbox cleanup journal path is invalid")
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".axiom-sandbox-pending-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove temporary sandbox journal: %w", err))
		}
		if _, err := os.Lstat(temporary); err == nil {
			resultErr = errors.Join(resultErr, errors.New("temporary sandbox journal still exists after cleanup"))
		} else if !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("verify temporary sandbox journal cleanup: %w", err))
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return errors.Join(err, file.Close())
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	temporaryPtr, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(temporaryPtr, pathPtr, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return err
	}
	readback, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back sandbox cleanup journal: %w", err)
	}
	if !bytes.Equal(data, readback) {
		return errors.New("sandbox cleanup journal did not match its persisted contents")
	}
	return nil
}

func removeCleanupJournal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove sandbox cleanup journal: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("sandbox cleanup journal still exists after removal")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify sandbox cleanup journal removal: %w", err)
	}
	return nil
}

// RecoverRunfiles restores temporary ACL grants (and legacy integrity labels)
// left by a host process that exited before command cleanup completed. It runs
// before a new Agent service accepts turns.
func RecoverRunfiles(root string) (RecoveryReport, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RecoveryReport{}, nil
		}
		return RecoveryReport{}, fmt.Errorf("resolve sandbox recovery root: %w", err)
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return RecoveryReport{}, fmt.Errorf("inspect sandbox recovery root: %w", err)
	}
	if !info.IsDir() {
		return RecoveryReport{}, fmt.Errorf("sandbox recovery root %q is not a directory", resolvedRoot)
	}
	entries, err := os.ReadDir(resolvedRoot)
	if err != nil {
		return RecoveryReport{}, fmt.Errorf("list sandbox recovery records: %w", err)
	}
	var report RecoveryReport
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".axiom-sandbox-") || filepath.Ext(name) != ".json" || entry.IsDir() {
			continue
		}
		path := filepath.Join(resolvedRoot, name)
		info, err := os.Lstat(path)
		if err != nil {
			return report, fmt.Errorf("inspect sandbox recovery record %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return report, fmt.Errorf("sandbox recovery record %s is not a regular file within the size limit", name)
		}
		journalReport, err := recoverCleanupJournal(path, resolvedRoot)
		report.UnresolvedGrants += journalReport.UnresolvedGrants
		report.PermissionDenied += journalReport.PermissionDenied
		report.OtherFailures += journalReport.OtherFailures
		if err != nil {
			return report, fmt.Errorf("recover sandbox journal %s: %w", name, err)
		}
	}
	return report, nil
}

func recoverCleanupJournal(path, root string) (RecoveryReport, error) {
	var report RecoveryReport
	data, err := os.ReadFile(path)
	if err != nil {
		return report, err
	}
	var journal cleanupJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return report, fmt.Errorf("decode journal: %w", err)
	}
	if journal.Version != 1 || journal.PID <= 0 || journal.ProcessStart == 0 || !strings.HasPrefix(journal.ProfileName, "AxiomAgent-") {
		return report, errors.New("journal fields are invalid")
	}
	if len(journal.Grants) > 512 {
		return report, errors.New("journal has too many access grants")
	}
	journalPath, err := filepath.Abs(path)
	if err != nil || !pathWithin(root, journalPath) {
		return report, errors.New("journal is outside the recovery root")
	}
	alive, err := journalOwnerAlive(journal.PID, journal.ProcessStart)
	if err != nil {
		return report, fmt.Errorf("check journal owner process: %w", err)
	}
	if alive {
		return report, nil
	}
	sid, err := windows.StringToSid(journal.AppContainerSID)
	if err != nil {
		return report, fmt.Errorf("decode AppContainer SID: %w", err)
	}
	if journal.LoopbackExempt {
		if err := setAppContainerLoopbackExemption(journal.AppContainerSID, false); err != nil {
			return report, fmt.Errorf("remove stale AppContainer loopback exemption: %w", err)
		}
	}
	var joined error
	for index := len(journal.Grants) - 1; index >= 0; index-- {
		grant := journal.Grants[index]
		canonical, pathErr := canonicalDirectory(grant.Path)
		if pathErr != nil {
			if errors.Is(pathErr, os.ErrNotExist) {
				continue
			}
			joined = errors.Join(joined, fmt.Errorf("resolve granted path %s: %w", grant.Path, pathErr))
			report.UnresolvedGrants++
			report.OtherFailures++
			continue
		}
		if !equalWindowsPath(canonical, grant.Path) {
			joined = errors.Join(joined, fmt.Errorf("granted path %s now resolves to %s", grant.Path, canonical))
			report.UnresolvedGrants++
			report.OtherFailures++
			continue
		}
		if grant.Write || grant.ACLWrite {
			var oldLabel *windows.ACL
			var descriptor *windows.SECURITY_DESCRIPTOR
			var descriptorErr error
			if grant.HadIntegrity {
				descriptor, descriptorErr = windows.SecurityDescriptorFromString(grant.OldIntegritySDDL)
				if descriptorErr != nil {
					joined = errors.Join(joined, fmt.Errorf("decode original integrity label for %s: %w", grant.Path, descriptorErr))
					report.UnresolvedGrants++
					report.OtherFailures++
					continue
				}
				oldLabel, _, descriptorErr = descriptor.SACL()
				if descriptorErr != nil {
					joined = errors.Join(joined, fmt.Errorf("read original integrity label for %s: %w", grant.Path, descriptorErr))
					report.UnresolvedGrants++
					report.OtherFailures++
					continue
				}
			}
			setErr := windows.SetNamedSecurityInfo(grant.Path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, oldLabel)
			if setErr != nil {
				joined = errors.Join(joined, fmt.Errorf("restore integrity label on %s: %w", grant.Path, setErr))
				report.UnresolvedGrants++
				if errors.Is(setErr, windows.ERROR_ACCESS_DENIED) {
					report.PermissionDenied++
				} else {
					report.OtherFailures++
				}
				continue
			}
		}
		if err := setAccess(grant.Path, sid, windows.REVOKE_ACCESS, grant.ACLWrite); err != nil {
			joined = errors.Join(joined, fmt.Errorf("revoke access from %s: %w", grant.Path, err))
			report.UnresolvedGrants++
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				report.PermissionDenied++
			} else {
				report.OtherFailures++
			}
		}
	}
	if err := deleteAppContainerProfile(journal.ProfileName); err != nil {
		return report, errors.Join(joined, fmt.Errorf("delete stale AppContainer profile: %w", err))
	}
	if err := removeCleanupJournal(path); err != nil {
		return report, errors.Join(joined, err)
	}
	return report, nil
}

func journalOwnerAlive(pid int, expectedStart uint64) (bool, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		return false, err
	}
	created, timeErr := processCreationTime(process)
	closeErr := windows.CloseHandle(process)
	if timeErr != nil || closeErr != nil {
		return false, errors.Join(timeErr, closeErr)
	}
	actualStart := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	return actualStart == expectedStart, nil
}

func processCreationTime(process windows.Handle) (windows.Filetime, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return windows.Filetime{}, err
	}
	return created, nil
}

func deleteAppContainerProfile(name string) error {
	if err := deleteProfileProc.Find(); err != nil {
		return fmt.Errorf("Windows AppContainer cleanup API is unavailable: %w", err)
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	hresult, _, _ := deleteProfileProc.Call(uintptr(unsafe.Pointer(namePtr)))
	if hresult == 0 || uint32(hresult) == 0x80070002 || uint32(hresult) == 0x80070490 {
		return nil
	}
	return fmt.Errorf("delete Windows AppContainer profile failed (HRESULT 0x%08X)", uint32(hresult))
}

func revokeAccessGrants(grants []accessGrant) error {
	var joined error
	for i := len(grants) - 1; i >= 0; i-- {
		grant := grants[i]
		if grant.write {
			if err := restoreIntegrityLabel(grant.path, grant.oldIntegritySDDL, grant.hadOriginalLabel); err != nil {
				joined = errors.Join(joined, fmt.Errorf("restore workspace integrity label on %s: %w", grant.path, err))
			}
		}
		if err := setAccess(grant.path, grant.sid, windows.REVOKE_ACCESS, grant.write); err != nil {
			joined = errors.Join(joined, fmt.Errorf("revoke sandbox access from %s: %w", grant.path, err))
		}
	}
	return joined
}

func setAccess(path string, sid *windows.SID, accessMode windows.ACCESS_MODE, write bool) error {
	aclMutationMu.Lock()
	defer aclMutationMu.Unlock()

	current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read directory DACL: %w", err)
	}
	if current == nil {
		return errors.New("filesystem object has no security descriptor")
	}
	permissions := windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE)
	if write {
		permissions |= windows.ACCESS_MASK(windows.FILE_GENERIC_WRITE | 0x40) // FILE_DELETE_CHILD for workspace-local cleanup.
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: permissions,
		AccessMode:        accessMode,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	updated, err := windows.BuildSecurityDescriptor(nil, nil, []windows.EXPLICIT_ACCESS{entry}, nil, current)
	if err != nil {
		return fmt.Errorf("build updated directory DACL: %w", err)
	}
	dacl, _, err := updated.DACL()
	if err != nil {
		return fmt.Errorf("read updated directory DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("write directory DACL: %w", err)
	}
	return nil
}

func commandEnvironment(input []string, tempDir, goCache, appContainerFolder string, networkAccess bool) (map[string]string, error) {
	allowed := map[string]struct{}{
		"COMSPEC": {}, "DOTNET_ROOT": {}, "GOMODCACHE": {}, "GOPATH": {}, "GOROOT": {},
		"JAVA_HOME": {}, "LANG": {}, "LC_ALL": {}, "LOCALAPPDATA": {}, "NUMBER_OF_PROCESSORS": {}, "OS": {},
		"PATH": {}, "PATHEXT": {}, "PROCESSOR_ARCHITECTURE": {}, "PROCESSOR_IDENTIFIER": {},
		"PSMODULEPATH": {}, "SYSTEMROOT": {}, "VIRTUAL_ENV": {}, "WINDIR": {},
	}
	if input == nil {
		input = os.Environ()
	}
	values := make(map[string]string)
	for _, entry := range input {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
			continue
		}
		upper := strings.ToUpper(key)
		if _, ok := allowed[upper]; ok {
			values[upper] = value
		}
	}
	values["TEMP"] = tempDir
	values["TMP"] = tempDir
	values["TMPDIR"] = tempDir
	values["GOCACHE"] = goCache
	values["LOCALAPPDATA"] = appContainerFolder
	if networkAccess {
		proxyValues, err := commandProxyEnvironment(input)
		if err != nil {
			return nil, err
		}
		for key, value := range proxyValues {
			values[key] = value
		}
	}
	return values, nil
}

func encodeEnvironment(environment map[string]string) ([]uint16, error) {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, key+"="+environment[key])
	}
	block := strings.Join(entries, "\x00") + "\x00\x00"
	return utf16.Encode([]rune(block)), nil
}

func environmentReadPaths(environment map[string]string) []string {
	var paths []string
	for _, key := range []string{"DOTNET_ROOT", "GOMODCACHE", "GOROOT", "JAVA_HOME", "VIRTUAL_ENV"} {
		if value := strings.TrimSpace(environment[key]); value != "" {
			if info, err := os.Stat(value); err == nil && info.IsDir() && !isSensitiveRuntimePath(value) && !isBroadRuntimePath(value) && !isSystemRuntimePath(value) {
				paths = append(paths, value)
			}
		}
	}
	if environment["GOMODCACHE"] == "" {
		gopath := strings.TrimSpace(environment["GOPATH"])
		if gopath == "" {
			if home, err := os.UserHomeDir(); err == nil {
				gopath = filepath.Join(home, "go")
			}
		}
		for _, root := range filepath.SplitList(gopath) {
			moduleCache := filepath.Join(root, "pkg", "mod")
			if info, err := os.Stat(moduleCache); err == nil && info.IsDir() && !isSensitiveRuntimePath(moduleCache) && !isSystemRuntimePath(moduleCache) {
				paths = append(paths, moduleCache)
				break
			}
		}
	}
	return paths
}

func runtimeReadPaths(executable string) []string {
	paths := []string{}
	appendRuntimeReadPath := func(path string) {
		resolved, err := canonicalDirectory(path)
		if err != nil || isSensitiveRuntimePath(resolved) || isBroadRuntimePath(resolved) || isSystemRuntimePath(resolved) {
			return
		}
		paths = append(paths, resolved)
	}

	executableDir, err := canonicalDirectory(filepath.Dir(executable))
	if err == nil {
		appendRuntimeReadPath(executableDir)
		// Windows virtual environments keep Python in Scripts while the standard
		// library and site-packages live under the environment root.
		if strings.EqualFold(filepath.Base(executableDir), "Scripts") && isPythonExecutable(executable) {
			appendRuntimeReadPath(filepath.Dir(executableDir))
		}
	}
	return paths
}

func isPythonExecutable(path string) bool {
	switch strings.ToLower(filepath.Base(path)) {
	case "python.exe", "python3.exe", "pythonw.exe":
		return true
	default:
		return false
	}
}

func isBroadRuntimePath(path string) bool {
	clean := filepath.Clean(path)
	if strings.EqualFold(clean, filepath.VolumeName(clean)+string(filepath.Separator)) {
		return true
	}
	if relative, err := filepath.Rel(filepath.VolumeName(clean)+string(filepath.Separator), clean); err == nil && relative != "." && !strings.Contains(relative, string(filepath.Separator)) {
		return true
	}
	for _, root := range []string{os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramW6432")} {
		if root != "" && equalWindowsPath(clean, root) {
			return true
		}
	}
	home, err := os.UserHomeDir()
	return err == nil && equalWindowsPath(clean, home)
}

func isSensitiveRuntimePath(path string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		resolvedHome = home
	}
	if equalWindowsPath(path, resolvedHome) {
		return true
	}
	for _, part := range strings.FieldsFunc(strings.ToLower(filepath.Clean(path)), func(r rune) bool { return r == '\\' || r == '/' }) {
		switch part {
		case ".ssh", ".aws", ".azure", ".docker", ".kube", ".gnupg", ".config", ".npm", "credentials", "secrets":
			return true
		}
	}
	return false
}

func isSystemRuntimePath(path string) bool {
	roots := []string{os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramW6432")}
	for _, root := range roots {
		if root == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		if pathWithin(root, path) {
			return true
		}
	}
	return false
}

func equalWindowsPath(a, b string) bool {
	aAbs, errA := filepath.Abs(filepath.Clean(a))
	bAbs, errB := filepath.Abs(filepath.Clean(b))
	return errA == nil && errB == nil && strings.EqualFold(aAbs, bAbs)
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func directoryCovered(target string, paths []accessPath) bool {
	for _, path := range paths {
		if pathWithin(path.path, target) {
			return true
		}
	}
	return false
}

func runAppContainerProcess(ctx context.Context, command *exec.Cmd, input []byte, environmentBlock []uint16, sid *windows.SID, networkAccess bool) (runErr, cleanupErr error) {
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
	if isSensitiveRuntimePath(filepath.Dir(executable)) || isBroadRuntimePath(filepath.Dir(executable)) {
		return errors.New("refusing to execute a program from a protected or broad user directory"), nil
	}
	workingDirectory, err := canonicalDirectory(command.Dir)
	if err != nil {
		return fmt.Errorf("resolve sandbox working directory: %w", err), nil
	}

	job, err := newJobObject()
	if err != nil {
		return nil, fmt.Errorf("create sandbox process job: %w", err)
	}
	defer func() {
		if job != 0 {
			if err := windows.CloseHandle(job); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close sandbox process job: %w", err))
			}
		}
	}()

	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var childStdin, parentStdin windows.Handle
	var parentStdout, childStdout windows.Handle
	var parentStderr, childStderr windows.Handle
	if err := windows.CreatePipe(&childStdin, &parentStdin, sa, 0); err != nil {
		return nil, fmt.Errorf("create sandbox stdin pipe: %w", err)
	}
	if err := windows.CreatePipe(&parentStdout, &childStdout, sa, 0); err != nil {
		return nil, errors.Join(fmt.Errorf("create sandbox stdout pipe: %w", err), closeWindowsHandles(childStdin, parentStdin))
	}
	if err := windows.CreatePipe(&parentStderr, &childStderr, sa, 0); err != nil {
		return nil, errors.Join(fmt.Errorf("create sandbox stderr pipe: %w", err), closeWindowsHandles(childStdin, parentStdin, parentStdout, childStdout))
	}
	defer func() {
		if err := closeWindowsHandles(childStdin, parentStdin, parentStdout, childStdout, parentStderr, childStderr); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}()
	for _, handle := range []windows.Handle{parentStdin, parentStdout, parentStderr} {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			return nil, fmt.Errorf("restrict sandbox pipe inheritance: %w", err)
		}
	}

	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, fmt.Errorf("create sandbox process attributes: %w", err)
	}
	defer attributes.Delete()
	inheritedHandles := []windows.Handle{childStdin, childStdout, childStderr}
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&inheritedHandles[0]), uintptr(len(inheritedHandles))*unsafe.Sizeof(inheritedHandles[0])); err != nil {
		return nil, fmt.Errorf("limit sandbox inherited handles: %w", err)
	}
	caps := securityCapabilities{AppContainerSID: sid}
	var internetClientSID *windows.SID
	var capabilityAttrs []windows.SIDAndAttributes
	if networkAccess {
		internetClientSID, err = windows.StringToSid("S-1-15-3-1") // SECURITY_CAPABILITY_INTERNET_CLIENT
		if err != nil {
			return nil, fmt.Errorf("resolve InternetClient AppContainer capability: %w", err)
		}
		// StringToSid returns a Go-owned copy (it frees the native SID internally).
		// Do not pass it to FreeSid: that API only releases SIDs created by
		// AllocateAndInitializeSid and would corrupt the Go heap here.
		capabilityAttrs = []windows.SIDAndAttributes{{Sid: internetClientSID, Attributes: windows.SE_GROUP_ENABLED}}
		caps.Capabilities = &capabilityAttrs[0]
		caps.CapabilityCount = uint32(len(capabilityAttrs))
	}
	if err := attributes.Update(procThreadAttributeSecurityCapabilities, unsafe.Pointer(&caps), unsafe.Sizeof(caps)); err != nil {
		return nil, fmt.Errorf("configure Windows AppContainer isolation: %w", err)
	}

	args := command.Args
	if len(args) == 0 {
		args = []string{executable}
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return nil, fmt.Errorf("encode sandbox command line: %w", err)
	}
	executable16, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, fmt.Errorf("encode sandbox executable path: %w", err)
	}
	workingDirectory16, err := windows.UTF16PtrFromString(workingDirectory)
	if err != nil {
		return nil, fmt.Errorf("encode sandbox working directory: %w", err)
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:         uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:      windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW,
			ShowWindow: windows.SW_HIDE,
			StdInput:   childStdin,
			StdOutput:  childStdout,
			StdErr:     childStderr,
		},
		ProcThreadAttributeList: attributes.List(),
	}
	processInfo := windows.ProcessInformation{}
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW)
	createErr := windows.CreateProcess(executable16, commandLine, nil, nil, true, flags, &environmentBlock[0], workingDirectory16, &startup.StartupInfo, &processInfo)
	// UpdateProcThreadAttribute stores pointers to these Go-owned values for
	// CreateProcess to read. Keep both the handle list and security capability
	// graph alive until the native call has finished consuming the attributes.
	runtime.KeepAlive(inheritedHandles)
	runtime.KeepAlive(caps)
	runtime.KeepAlive(capabilityAttrs)
	runtime.KeepAlive(internetClientSID)
	if createErr != nil {
		return nil, fmt.Errorf("start command in Windows AppContainer: %w", createErr)
	}
	defer func() {
		if processInfo.Thread != 0 {
			if err := windows.CloseHandle(processInfo.Thread); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close sandbox process thread: %w", err))
			}
		}
		if processInfo.Process != 0 {
			if err := windows.CloseHandle(processInfo.Process); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close sandbox process handle: %w", err))
			}
		}
	}()
	if err := windows.AssignProcessToJobObject(job, processInfo.Process); err != nil {
		killErr := windows.TerminateProcess(processInfo.Process, 1)
		_, waitErr := windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		return nil, errors.Join(fmt.Errorf("assign sandbox process to kill-on-close job: %w", err), killErr, waitErr)
	}
	var childHandleErr error
	for _, handle := range []*windows.Handle{&childStdin, &childStdout, &childStderr} {
		if *handle == 0 {
			continue
		}
		if err := windows.CloseHandle(*handle); err != nil {
			childHandleErr = errors.Join(childHandleErr, err)
		} else {
			*handle = 0
		}
	}
	if childHandleErr != nil {
		killErr := windows.TerminateJobObject(job, 1)
		_, waitErr := windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		return nil, errors.Join(fmt.Errorf("close host copies of sandbox child pipes: %w", childHandleErr), killErr, waitErr)
	}
	if _, err := windows.ResumeThread(processInfo.Thread); err != nil {
		killErr := windows.TerminateJobObject(job, 1)
		_, waitErr := windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		return nil, errors.Join(fmt.Errorf("resume sandbox process: %w", err), killErr, waitErr)
	}

	stdinFile := os.NewFile(uintptr(parentStdin), "sandbox-stdin")
	stdoutFile := os.NewFile(uintptr(parentStdout), "sandbox-stdout")
	stderrFile := os.NewFile(uintptr(parentStderr), "sandbox-stderr")
	parentStdin, parentStdout, parentStderr = 0, 0, 0
	stdoutTarget := command.Stdout
	if stdoutTarget == nil {
		stdoutTarget = io.Discard
	}
	stderrTarget := command.Stderr
	if stderrTarget == nil {
		stderrTarget = io.Discard
	}
	stdoutDone := make(chan error, 1)
	stderrDone := make(chan error, 1)
	stdinDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stdoutTarget, stdoutFile)
		stdoutDone <- errors.Join(copyErr, stdoutFile.Close())
	}()
	go func() {
		_, copyErr := io.Copy(stderrTarget, stderrFile)
		stderrDone <- errors.Join(copyErr, stderrFile.Close())
	}()
	go func() {
		var writeErr error
		if len(input) > 0 {
			_, writeErr = stdinFile.Write(input)
		}
		stdinDone <- errors.Join(writeErr, stdinFile.Close())
	}()
	waitErr := waitProcess(ctx, processInfo.Process, job)
	if waitErr != nil {
		runErr = waitErr
	}
	if err := windows.CloseHandle(job); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close sandbox process job: %w", err))
		terminateErr := windows.TerminateJobObject(job, 1)
		result, jobWaitErr := windows.WaitForSingleObject(job, 5_000)
		if jobWaitErr != nil || result != windows.WAIT_OBJECT_0 {
			terminationErr := errors.Join(terminateErr, jobWaitErr)
			if result != windows.WAIT_OBJECT_0 {
				terminationErr = errors.Join(terminationErr, fmt.Errorf("sandbox job wait returned %d", result))
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("terminate remaining sandbox processes: %w", terminationErr))
			cleanupErr = errors.Join(cleanupErr, closeWindowsFiles(stdinFile, stdoutFile, stderrFile))
		} else if terminateErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("terminate remaining sandbox processes: %w", terminateErr))
		}
	} else {
		job = 0
	}
	cleanupErr = errors.Join(cleanupErr, <-stdinDone, <-stdoutDone, <-stderrDone)
	if runErr != nil {
		return runErr, cleanupErr
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(processInfo.Process, &exitCode); err != nil {
		return nil, errors.Join(cleanupErr, fmt.Errorf("read sandbox process exit code: %w", err))
	}
	if exitCode != 0 {
		return processExitError{code: exitCode}, cleanupErr
	}
	return nil, cleanupErr
}

func newJobObject() (windows.Handle, error) {
	return newJobObjectWithLimit(maxProcessesPerJob)
}

func newJobObjectWithLimit(maxProcesses uint32) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	info.BasicLimitInformation.ActiveProcessLimit = maxProcesses
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, errors.Join(err, windows.CloseHandle(job))
	}
	return job, nil
}

type nativeJobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaults           uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func terminateAndWaitJob(job windows.Handle, exitCode uint32) error {
	var accounting nativeJobAccounting
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
		if killErr := windows.TerminateJobObject(job, exitCode); killErr != nil {
			return errors.Join(fmt.Errorf("query Job Object processes: %w", err), fmt.Errorf("terminate Job Object: %w", killErr))
		}
	} else if accounting.ActiveProcesses > 0 {
		if err := windows.TerminateJobObject(job, exitCode); err != nil {
			return fmt.Errorf("terminate %d command processes: %w", accounting.ActiveProcesses, err)
		}
	}
	wait, err := windows.WaitForSingleObject(job, 30_000)
	if err != nil {
		return fmt.Errorf("wait for command process tree to exit: %w", err)
	}
	if wait != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("command Job Object remained active after termination (wait=%d)", wait)
	}
	return nil
}

func waitProcess(ctx context.Context, process, job windows.Handle) error {
	for {
		result, err := windows.WaitForSingleObject(process, 100)
		if err != nil {
			return fmt.Errorf("wait for sandbox process: %w", err)
		}
		switch result {
		case windows.WAIT_OBJECT_0:
			return nil
		case uint32(windows.WAIT_TIMEOUT):
			if ctx.Err() == nil {
				continue
			}
			killErr := windows.TerminateJobObject(job, 1)
			if killErr != nil {
				killErr = errors.Join(killErr, windows.TerminateProcess(process, 1))
			}
			_, waitErr := windows.WaitForSingleObject(process, windows.INFINITE)
			return errors.Join(ctx.Err(), killErr, waitErr)
		default:
			return fmt.Errorf("unexpected sandbox process wait result: %d", result)
		}
	}
}

func canonicalExecutable(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular executable file", path)
	}
	return filepath.Clean(resolved), nil
}

func closeWindowsHandles(handles ...windows.Handle) error {
	var joined error
	for _, handle := range handles {
		if handle == 0 || handle == windows.InvalidHandle {
			continue
		}
		if err := windows.CloseHandle(handle); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func closeWindowsFiles(files ...*os.File) error {
	var joined error
	for _, file := range files {
		if file == nil {
			continue
		}
		if err := file.Close(); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}
