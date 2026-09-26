//go:build windows

package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	path string
	sid  *windows.SID
}

type accessPath struct {
	path string
}

type cleanupJournal struct {
	Version         int            `json:"version"`
	PID             int            `json:"pid"`
	ProcessStart    uint64         `json:"processStart"`
	ProfileName     string         `json:"profileName"`
	AppContainerSID string         `json:"appContainerSid"`
	Grants          []journalGrant `json:"grants"`
}

type journalGrant struct {
	Path             string `json:"path"`
	Write            bool   `json:"write"`
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

// Run creates a per-invocation AppContainer identity, grants it read access
// only to requested paths and runtime directories, and confines writes to the
// profile's private Temp directory. It starts the process suspended inside a
// kill-on-close Job Object. No network capabilities are attached.
func Run(ctx context.Context, command *exec.Cmd, input []byte, policy Policy) (runErr, cleanupErr error) {
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
		var profileCleanupErr error
		if accessRestoreErr == nil {
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
		if accessRestoreErr != nil {
			degradedMu.Lock()
			sandboxDegraded = errors.Join(sandboxDegraded, accessRestoreErr)
			degradedMu.Unlock()
		}
		restoreErr := errors.Join(accessRestoreErr, profileCleanupErr)
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
	environment := commandEnvironment(command.Env, tempDir, goCache, appContainerFolder)
	environmentBlock, err := encodeEnvironment(environment)
	if err != nil {
		return fmt.Errorf("prepare sandbox environment: %w", err), nil
	}

	readOnly := append([]string(nil), policy.ReadOnlyPaths...)
	readOnly = append(readOnly, runtimeReadPaths(command.Path, environment)...)
	readOnly = append(readOnly, environmentReadPaths(environment)...)
	accessPaths, err := normalizeAccessPaths(readOnly)
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
	if err := writeCleanupJournal(policy.JournalPath, journal); err != nil {
		return fmt.Errorf("persist sandbox cleanup journal: %w", err), nil
	}
	journalCreated = true
	grants, err = applyAccessGrants(accessPaths, profile.sid)
	if err != nil {
		return err, nil
	}
	if err := ctx.Err(); err != nil {
		return err, nil
	}
	runErr, cleanupErr = runAppContainerProcess(ctx, command, input, environmentBlock, profile.sid)
	return runErr, cleanupErr
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
	displayName, err := windows.UTF16PtrFromString("Axiom agent command")
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

func normalizeAccessPaths(readOnly []string) ([]accessPath, error) {
	byPath := map[string]accessPath{}
	for _, raw := range readOnly {
		if strings.TrimSpace(raw) == "" {
			return nil, errors.New("sandbox path is empty")
		}
		path, err := canonicalDirectory(raw)
		if err != nil {
			return nil, fmt.Errorf("resolve allowed path %q: %w", raw, err)
		}
		if filepath.VolumeName(path) == path || strings.EqualFold(path, filepath.VolumeName(path)+string(filepath.Separator)) {
			return nil, fmt.Errorf("refusing to grant access to a drive root: %s", path)
		}
		byPath[strings.ToLower(path)] = accessPath{path: path}
	}
	paths := make([]accessPath, 0, len(byPath))
	for _, path := range byPath {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i].path) < len(paths[j].path) })
	return paths, nil
}

func applyAccessGrants(paths []accessPath, sid *windows.SID) ([]accessGrant, error) {
	grants := make([]accessGrant, 0, len(paths))
	for _, path := range paths {
		if err := setAccess(path.path, sid, windows.SET_ACCESS); err != nil {
			revokeErr := setAccess(path.path, sid, windows.REVOKE_ACCESS)
			if revokeErr != nil {
				grants = append(grants, accessGrant{path: path.path, sid: sid})
			}
			return grants, errors.Join(fmt.Errorf("grant sandbox access to %s: %w", path.path, err), revokeErr)
		}
		grants = append(grants, accessGrant{path: path.path, sid: sid})
	}
	return grants, nil
}

func newCleanupJournal(profile *appContainerProfile, paths []accessPath) (cleanupJournal, error) {
	sid := profile.sid.String()
	var created windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &created, nil, nil, nil); err != nil {
		return cleanupJournal{}, fmt.Errorf("read host process creation time: %w", err)
	}
	processStart := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	journal := cleanupJournal{Version: 1, PID: os.Getpid(), ProcessStart: processStart, ProfileName: profile.name, AppContainerSID: sid}
	for _, path := range paths {
		journal.Grants = append(journal.Grants, journalGrant{
			Path: path.path,
		})
	}
	return journal, nil
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
func RecoverRunfiles(root string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("resolve sandbox recovery root: %w", err)
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return fmt.Errorf("inspect sandbox recovery root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sandbox recovery root %q is not a directory", resolvedRoot)
	}
	entries, err := os.ReadDir(resolvedRoot)
	if err != nil {
		return fmt.Errorf("list sandbox recovery records: %w", err)
	}
	var joined error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".axiom-sandbox-") || filepath.Ext(name) != ".json" || entry.IsDir() {
			continue
		}
		path := filepath.Join(resolvedRoot, name)
		info, err := os.Lstat(path)
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("inspect sandbox recovery record %s: %w", name, err))
			continue
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			joined = errors.Join(joined, fmt.Errorf("sandbox recovery record %s is not a regular file within the size limit", name))
			continue
		}
		if err := recoverCleanupJournal(path, resolvedRoot); err != nil {
			joined = errors.Join(joined, fmt.Errorf("recover sandbox journal %s: %w", name, err))
		}
	}
	return joined
}

func recoverCleanupJournal(path, root string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var journal cleanupJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return fmt.Errorf("decode journal: %w", err)
	}
	if journal.Version != 1 || journal.PID <= 0 || journal.ProcessStart == 0 || !strings.HasPrefix(journal.ProfileName, "AxiomAgent-") {
		return errors.New("journal fields are invalid")
	}
	if len(journal.Grants) > 512 {
		return errors.New("journal has too many access grants")
	}
	journalPath, err := filepath.Abs(path)
	if err != nil || !pathWithin(root, journalPath) {
		return errors.New("journal is outside the recovery root")
	}
	alive, err := journalOwnerAlive(journal.PID, journal.ProcessStart)
	if err != nil {
		return fmt.Errorf("check journal owner process: %w", err)
	}
	if alive {
		return nil
	}
	sid, err := windows.StringToSid(journal.AppContainerSID)
	if err != nil {
		return fmt.Errorf("decode AppContainer SID: %w", err)
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
			continue
		}
		if !equalWindowsPath(canonical, grant.Path) {
			joined = errors.Join(joined, fmt.Errorf("granted path %s now resolves to %s", grant.Path, canonical))
			continue
		}
		if grant.Write {
			var oldLabel *windows.ACL
			var descriptor *windows.SECURITY_DESCRIPTOR
			var descriptorErr error
			if grant.HadIntegrity {
				descriptor, descriptorErr = windows.SecurityDescriptorFromString(grant.OldIntegritySDDL)
				if descriptorErr != nil {
					joined = errors.Join(joined, fmt.Errorf("decode original integrity label for %s: %w", grant.Path, descriptorErr))
					continue
				}
				oldLabel, _, descriptorErr = descriptor.SACL()
				if descriptorErr != nil {
					joined = errors.Join(joined, fmt.Errorf("read original integrity label for %s: %w", grant.Path, descriptorErr), freeSecurityDescriptor(descriptor))
					continue
				}
			}
			setErr := windows.SetNamedSecurityInfo(grant.Path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, oldLabel)
			freeErr := freeSecurityDescriptor(descriptor)
			if setErr != nil || freeErr != nil {
				joined = errors.Join(joined, fmt.Errorf("restore integrity label on %s: %w", grant.Path, errors.Join(setErr, freeErr)))
				continue
			}
		}
		if err := setAccess(grant.Path, sid, windows.REVOKE_ACCESS); err != nil {
			joined = errors.Join(joined, fmt.Errorf("revoke access from %s: %w", grant.Path, err))
		}
	}
	if joined != nil {
		return joined
	}
	if err := deleteAppContainerProfile(journal.ProfileName); err != nil {
		return fmt.Errorf("delete stale AppContainer profile: %w", err)
	}
	return removeCleanupJournal(path)
}

func journalOwnerAlive(pid int, expectedStart uint64) (bool, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, nil
		}
		return false, err
	}
	var created windows.Filetime
	timeErr := windows.GetProcessTimes(process, &created, nil, nil, nil)
	closeErr := windows.CloseHandle(process)
	if timeErr != nil || closeErr != nil {
		return false, errors.Join(timeErr, closeErr)
	}
	actualStart := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	return actualStart == expectedStart, nil
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
		if err := setAccess(grant.path, grant.sid, windows.REVOKE_ACCESS); err != nil {
			joined = errors.Join(joined, fmt.Errorf("revoke sandbox access from %s: %w", grant.path, err))
		}
	}
	return joined
}

func freeSecurityDescriptor(descriptor *windows.SECURITY_DESCRIPTOR) error {
	if descriptor == nil {
		return nil
	}
	handle, err := windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(descriptor))))
	if err != nil {
		return err
	}
	if handle != 0 {
		return errors.New("LocalFree did not release the security descriptor")
	}
	return nil
}

func setAccess(path string, sid *windows.SID, accessMode windows.ACCESS_MODE) (resultErr error) {
	aclMutationMu.Lock()
	defer aclMutationMu.Unlock()

	current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if current == nil {
		return errors.New("filesystem object has no security descriptor")
	}
	defer func() {
		resultErr = errors.Join(resultErr, freeSecurityDescriptor(current))
	}()
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE),
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
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, freeSecurityDescriptor(updated))
	}()
	dacl, _, err := updated.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func commandEnvironment(input []string, tempDir, goCache, appContainerFolder string) map[string]string {
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
	return values
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

func runtimeReadPaths(executable string, environment map[string]string) []string {
	paths := []string{}
	if executableDir, err := canonicalDirectory(filepath.Dir(executable)); err == nil && !isSensitiveRuntimePath(executableDir) && !isBroadRuntimePath(executableDir) && !isSystemRuntimePath(executableDir) {
		paths = append(paths, executableDir)
	}
	for _, item := range filepath.SplitList(environment["PATH"]) {
		item = strings.TrimSpace(strings.Trim(item, `"`))
		if item == "" || !filepath.IsAbs(item) {
			continue
		}
		resolved, err := canonicalDirectory(item)
		if err != nil || isSensitiveRuntimePath(resolved) || isBroadRuntimePath(resolved) || isSystemRuntimePath(resolved) {
			continue
		}
		paths = append(paths, resolved)
	}
	return paths
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

func runAppContainerProcess(ctx context.Context, command *exec.Cmd, input []byte, environmentBlock []uint16, sid *windows.SID) (runErr, cleanupErr error) {
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
	if err := windows.CreateProcess(executable16, commandLine, nil, nil, true, flags, &environmentBlock[0], workingDirectory16, &startup.StartupInfo, &processInfo); err != nil {
		return nil, fmt.Errorf("start command in Windows AppContainer: %w", err)
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
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	info.BasicLimitInformation.ActiveProcessLimit = maxProcessesPerJob
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, errors.Join(err, windows.CloseHandle(job))
	}
	return job, nil
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
