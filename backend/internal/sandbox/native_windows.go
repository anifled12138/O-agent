//go:build windows

package sandbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const nativeManifestVersion = 2
const nativeRunnerVersion = "2"

type nativeCredential struct {
	Username          string `json:"username"`
	SID               string `json:"sid"`
	EncryptedPassword string `json:"encryptedPassword"`
}

type nativeManifest struct {
	Version               int              `json:"version"`
	Lifecycle             string           `json:"lifecycle,omitempty"`
	OwnerSID              string           `json:"ownerSid"`
	RunnerVersion         string           `json:"runnerVersion"`
	RunnerSHA256          string           `json:"runnerSha256"`
	PreviousRunnerVersion string           `json:"previousRunnerVersion,omitempty"`
	PreviousRunnerSHA256  string           `json:"previousRunnerSha256,omitempty"`
	CandidateRunnerSHA256 string           `json:"candidateRunnerSha256,omitempty"`
	Offline               nativeCredential `json:"offline"`
	Online                nativeCredential `json:"online"`
	WFPKeys               []string         `json:"wfpKeys"`
	CreatedAt             string           `json:"createdAt"`
}

type nativeExecutionPlan struct {
	Version                     int      `json:"version"`
	CommandID                   string   `json:"commandId"`
	InstallationVersion         string   `json:"installationVersion"`
	CommandPath                 string   `json:"commandPath"`
	Arguments                   []string `json:"arguments"`
	WorkingDirectory            string   `json:"workingDirectory"`
	Environment                 []string `json:"environment"`
	Input                       []byte   `json:"input,omitempty"`
	ReadRoots                   []string `json:"readRoots"`
	WriteRoots                  []string `json:"writeRoots"`
	NetworkAccess               bool     `json:"networkAccess"`
	GitCredentialPipe           string   `json:"gitCredentialPipe,omitempty"`
	PrivateTempWorkingDirectory bool     `json:"privateTempWorkingDirectory"`
	TimeoutMS                   int64    `json:"timeoutMs"`
}

var (
	procCreateProcessWithLogon = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateProcessWithLogonW")
	procLogonUser              = windows.NewLazySystemDLL("advapi32.dll").NewProc("LogonUserW")
	procCreateMutex            = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateMutexW")
	procReleaseMutex           = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReleaseMutex")
	procGetCurrentProcessID    = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCurrentProcessId")
	procGetExitCodeProcess     = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetExitCodeProcess")
	procGetNamedPipeClientPID  = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetNamedPipeClientProcessId")
)

func NativeStatus(installDir string, runnerPath string) NativeHealth {
	status := NativeHealth{Installation: "absent", Health: "unhealthy", Backend: string(BackendWindowsNative), CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	manifest, err := readNativeManifest(installDir)
	if errors.Is(err, os.ErrNotExist) {
		status.Reason = "Windows native sandbox has not been installed"
		return status
	}
	status.Installation = "installed"
	if err != nil {
		status.Reason = err.Error()
		return status
	}
	if manifest.Lifecycle == "installing" {
		status.Installation = "installing"
		status.OwnerSID, status.OfflineUser, status.OnlineUser = manifest.OwnerSID, manifest.Offline.Username, manifest.Online.Username
		status.Reason = "native sandbox setup was interrupted; repair or uninstall it"
		return status
	}
	if manifest.Lifecycle == "uninstalling" {
		status.Installation = "uninstalling"
		status.OwnerSID, status.OfflineUser, status.OnlineUser = manifest.OwnerSID, manifest.Offline.Username, manifest.Online.Username
		status.Reason = "native sandbox uninstall was interrupted; resume uninstall"
		return status
	}
	status.OwnerSID, status.OfflineUser, status.OnlineUser = manifest.OwnerSID, manifest.Offline.Username, manifest.Online.Username
	if err := verifyInstallationOwner(manifest.OwnerSID); err != nil {
		status.Reason = err.Error()
		return status
	}
	if filepath.Clean(runnerPath) == "." || runnerPath == "" {
		runnerPath = filepath.Join(installDir, "axiom-command-runner.exe")
	}
	status.Runner = runnerPath
	if manifest.PreviousRunnerSHA256 != "" || manifest.CandidateRunnerSHA256 != "" {
		status.Reason = "sandbox runner maintenance has pending recovery; run repair"
		return status
	}
	if err := probeNativeInstallation(manifest, runnerPath); err != nil {
		status.Reason = err.Error()
		return status
	}
	status.Health = "healthy"
	status.Reason = ""
	return status
}

func LinuxStatus() NativeHealth {
	return NativeHealth{Installation: "absent", Health: "unhealthy", Backend: string(BackendBubblewrap), Reason: "Linux bubblewrap sandbox is unavailable on Windows", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

// InstallWindowsNative creates or repairs only resources recorded as owned by
// this per-user installation. It requires an elevated setup process.
func InstallWindowsNative(installDir, sourceRunner, ownerSID string) (resultErr error) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("sandbox setup requires an elevated administrator process")
	}
	if ownerSID == "" {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return fmt.Errorf("resolve installation owner SID: %w", err)
		}
		ownerSID = user.User.Sid.String()
	}
	owner, err := windows.StringToSid(ownerSID)
	if err != nil || !owner.IsValid() {
		return fmt.Errorf("invalid installation owner SID %q", ownerSID)
	}
	installDir, err = checkedInstallDirectory(installDir)
	if err != nil {
		return err
	}
	mutex, release, err := acquireSetupLock(ownerSID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, release(mutex)) }()
	if err := drainNativeJournalsLocked(installDir, 2*time.Minute); err != nil {
		return fmt.Errorf("wait for native sandbox commands before setup: %w", err)
	}
	if sourceRunner == "" {
		return errors.New("trusted command runner source is required for installation and repair")
	}
	if _, err := os.Stat(sourceRunner); err != nil {
		return fmt.Errorf("inspect trusted command runner source: %w", err)
	}
	manifestPath := filepath.Join(installDir, "installation.json")
	manifest, readErr := readNativeManifest(installDir)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read existing native sandbox installation: %w", readErr)
	}
	newInstall := errors.Is(readErr, os.ErrNotExist)
	if newInstall {
		if _, err := os.Lstat(installDir); err == nil {
			entries, readErr := os.ReadDir(installDir)
			if readErr != nil {
				return fmt.Errorf("inspect sandbox directory without a valid manifest: %w", readErr)
			}
			if len(entries) != 0 {
				return fmt.Errorf("refusing to take ownership of a non-empty sandbox directory without a valid manifest: %s", installDir)
			}
			if err := os.Remove(installDir); err != nil {
				return fmt.Errorf("remove empty incomplete sandbox directory: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect sandbox installation directory: %w", err)
		}
		if err := os.MkdirAll(installDir, 0o700); err != nil {
			return fmt.Errorf("create sandbox installation directory: %w", err)
		}
		if err := secureNativeDirectory(installDir, ownerSID); err != nil {
			return err
		}
		offlineName, onlineName := sandboxAccountNames(ownerSID)
		keys, err := ownedWFPKeyIdentifiers(ownerSID)
		if err != nil {
			return err
		}
		manifest = nativeManifest{
			Version: nativeManifestVersion, Lifecycle: "installing", OwnerSID: ownerSID,
			Offline: nativeCredential{Username: offlineName}, Online: nativeCredential{Username: onlineName},
			WFPKeys: wfpKeyStrings(keys), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := writeNativeManifest(manifestPath, manifest, ownerSID); err != nil {
			return fmt.Errorf("persist native sandbox installation journal: %w", err)
		}
	} else if manifest.OwnerSID != ownerSID {
		return fmt.Errorf("sandbox manifest owner SID mismatch: recorded %q, requested %q", manifest.OwnerSID, ownerSID)
	} else if manifest.Lifecycle == "uninstalling" {
		return errors.New("native sandbox uninstall is incomplete; resume uninstall before repair")
	} else if err := recoverNativeRunnerTransaction(installDir, &manifest, ownerSID); err != nil {
		return fmt.Errorf("recover interrupted sandbox runner maintenance: %w", err)
	}
	offlineName, onlineName := sandboxAccountNames(ownerSID)
	keys, err := ownedWFPKeyIdentifiers(ownerSID)
	if err != nil {
		return err
	}
	expectedWFPKeys := wfpKeyStrings(keys)
	if manifest.Offline.Username != offlineName || manifest.Online.Username != onlineName {
		return errors.New("sandbox manifest account names do not match this installation owner")
	}
	if !newInstall && len(manifest.WFPKeys) > 0 && !equalStringLists(manifest.WFPKeys, expectedWFPKeys) {
		return errors.New("sandbox manifest WFP keys do not match this installation owner")
	}
	originalManifest := manifest
	committed := false
	transactionStarted := false
	repairStatePersisted := false
	defer func() {
		if resultErr == nil || committed || newInstall || !repairStatePersisted {
			return
		}
		var rollbackErr error
		if transactionStarted {
			rollbackErr = recoverNativeRunnerTransaction(installDir, &manifest, ownerSID)
		}
		if rollbackErr == nil && originalManifest.RunnerSHA256 != "" {
			stableHash, hashErr := sha256File(filepath.Join(installDir, "axiom-command-runner.exe"))
			if hashErr != nil || stableHash != originalManifest.RunnerSHA256 {
				rollbackErr = errors.Join(errors.New("previous sandbox runner cannot be verified after repair failure"), hashErr)
			}
		}
		if rollbackErr == nil {
			rollbackErr = writeNativeManifest(manifestPath, originalManifest, ownerSID)
		}
		if rollbackErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restore previous sandbox installation state: %w", rollbackErr))
		}
	}()
	manifest.WFPKeys = expectedWFPKeys
	manifest.Lifecycle = "installing"
	repairStatePersisted = true
	if err := writeNativeManifest(manifestPath, manifest, ownerSID); err != nil {
		return fmt.Errorf("persist native sandbox repair state: %w", err)
	}
	createdOffline, createdOnline := false, false
	wfpTouched := false
	if newInstall {
		defer func() {
			if resultErr == nil || committed {
				return
			}
			var rollbackErr error
			if createdOnline {
				rollbackErr = errors.Join(rollbackErr, deleteSandboxAccount(manifest.Online.Username))
			}
			if createdOffline {
				rollbackErr = errors.Join(rollbackErr, deleteSandboxAccount(manifest.Offline.Username))
			}
			if wfpTouched && rollbackErr == nil {
				rollbackErr = removeOwnedWFP(ownerSID)
			}
			if rollbackErr == nil {
				rollbackErr = removeNewInstallDirectory(installDir)
			}
			if rollbackErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("sandbox setup rollback failed; installation journal retained: %w", rollbackErr))
			}
		}()
	}
	if err := ensureNativeManifestCredential(&manifest, &manifest.Offline, ownerSID, sourceRunner, &createdOffline, manifestPath); err != nil {
		return fmt.Errorf("prepare Offline sandbox account: %w", err)
	}
	if err := ensureNativeManifestCredential(&manifest, &manifest.Online, ownerSID, sourceRunner, &createdOnline, manifestPath); err != nil {
		return fmt.Errorf("prepare Online sandbox account: %w", err)
	}
	wfpTouched = true
	keys, err = installOfflineWFP(ownerSID)
	if err != nil {
		return fmt.Errorf("install or repair persistent sandbox WFP policy: %w", err)
	}
	manifest.WFPKeys = wfpKeyStrings(keys)

	runnerDest := filepath.Join(installDir, "axiom-command-runner.exe")
	runnerHash, err := sha256File(sourceRunner)
	if err != nil {
		return fmt.Errorf("verify trusted sandbox runner source: %w", err)
	}
	previousVersion, previousHash := manifest.RunnerVersion, manifest.RunnerSHA256
	if _, err := os.Lstat(runnerDest + ".new"); err == nil {
		return fmt.Errorf("sandbox runner staging path already exists: %s", runnerDest+".new")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(runnerDest + ".previous"); err == nil {
		return fmt.Errorf("sandbox runner backup path already exists: %s", runnerDest+".previous")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	manifest.PreviousRunnerVersion = previousVersion
	manifest.PreviousRunnerSHA256 = previousHash
	manifest.CandidateRunnerSHA256 = runnerHash
	if err := writeNativeManifest(manifestPath, manifest, ownerSID); err != nil {
		return fmt.Errorf("persist sandbox runner replacement journal: %w", err)
	}
	transactionStarted = true
	stagedHash, err := stageProtectedRunner(sourceRunner, runnerDest, ownerSID)
	if err != nil {
		return err
	}
	if stagedHash != runnerHash {
		return errors.New("staged sandbox runner does not match its trusted source")
	}
	if err := probeManifestAccountRunner(runnerDest+".new", manifest.Offline, ownerSID); err != nil {
		return fmt.Errorf("staged runner Probe for %q failed: %w", manifest.Offline.Username, err)
	}
	if err := probeManifestAccountRunner(runnerDest+".new", manifest.Online, ownerSID); err != nil {
		return fmt.Errorf("staged runner Probe for %q failed: %w", manifest.Online.Username, err)
	}
	if err := activateStagedRunner(runnerDest, previousHash, runnerHash); err != nil {
		return err
	}
	manifest.RunnerVersion = nativeRunnerVersion
	manifest.RunnerSHA256 = runnerHash
	manifest.Version = nativeManifestVersion
	if err := writeNativeManifest(manifestPath, manifest, ownerSID); err != nil {
		return err
	}
	readBack, err := readNativeManifest(installDir)
	if err != nil {
		return fmt.Errorf("read back native sandbox installation manifest: %w", err)
	}
	if readBack.OwnerSID != manifest.OwnerSID || readBack.Offline.SID != manifest.Offline.SID || readBack.Online.SID != manifest.Online.SID || readBack.RunnerSHA256 != manifest.RunnerSHA256 || readBack.Lifecycle != "installing" {
		return errors.New("native sandbox installation manifest read-back mismatch")
	}
	if err := probeNativeInstallation(readBack, runnerDest); err != nil {
		return fmt.Errorf("native sandbox installation Probe failed: %w", err)
	}
	manifest.Lifecycle = "installed"
	if err := writeNativeManifest(manifestPath, manifest, ownerSID); err != nil {
		return err
	}
	finalReadBack, err := readNativeManifest(installDir)
	if err != nil {
		return fmt.Errorf("read back installed sandbox lifecycle: %w", err)
	}
	if finalReadBack.Lifecycle != "installed" || finalReadBack.RunnerSHA256 != runnerHash || finalReadBack.WFPKeys == nil {
		return errors.New("installed sandbox lifecycle read-back mismatch")
	}
	if err := probeNativeInstallation(finalReadBack, runnerDest); err != nil {
		return fmt.Errorf("final native sandbox Probe failed: %w", err)
	}
	committed = true
	if err := cleanupNativeRunnerTransaction(installDir, &manifest, ownerSID); err != nil {
		return fmt.Errorf("sandbox runner is installed but maintenance cleanup is pending: %w", err)
	}
	return nil
}

// RemoveWindowsNative removes only the account, WFP, runner, and state resources
// named by this installation's protected manifest. It never adopts or removes
// same-named resources when that ownership record is absent.
func RemoveWindowsNative(installDir, ownerSID string) (resultErr error) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("sandbox uninstall requires an elevated administrator process")
	}
	if ownerSID == "" {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return fmt.Errorf("resolve sandbox uninstall owner SID: %w", err)
		}
		ownerSID = user.User.Sid.String()
	}
	owner, err := windows.StringToSid(ownerSID)
	if err != nil || !owner.IsValid() {
		return fmt.Errorf("invalid sandbox uninstall owner SID %q", ownerSID)
	}
	installDir, err = checkedInstallDirectory(installDir)
	if err != nil {
		return err
	}
	mutex, release, err := acquireSetupLock(ownerSID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, release(mutex)) }()
	if err := drainNativeJournalsLocked(installDir, 2*time.Minute); err != nil {
		return fmt.Errorf("wait for native sandbox commands before uninstall: %w", err)
	}
	manifest, err := readNativeManifest(installDir)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(installDir); errors.Is(statErr, os.ErrNotExist) {
			return nil
		} else if statErr != nil {
			return fmt.Errorf("inspect sandbox directory without an installation manifest: %w", statErr)
		}
		entries, readErr := os.ReadDir(installDir)
		if readErr != nil {
			return fmt.Errorf("inspect sandbox directory without an installation manifest: %w", readErr)
		}
		if len(entries) != 0 {
			return fmt.Errorf("refusing to remove non-empty sandbox directory without an owned installation manifest: %s", installDir)
		}
		if err := os.Remove(installDir); err != nil {
			return fmt.Errorf("remove empty sandbox directory without an installation manifest: %w", err)
		}
		if _, err := os.Lstat(installDir); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, errors.New("empty sandbox directory remains after removal"))
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read owned sandbox installation before uninstall: %w", err)
	}
	if manifest.OwnerSID != ownerSID {
		return fmt.Errorf("sandbox manifest owner SID mismatch: recorded %q, requested %q", manifest.OwnerSID, ownerSID)
	}
	keys, err := ownedWFPKeyIdentifiers(ownerSID)
	if err != nil {
		return err
	}
	if !equalStringLists(manifest.WFPKeys, wfpKeyStrings(keys)) {
		return errors.New("refusing to uninstall because manifest WFP keys do not match this installation's owned keys")
	}
	offlineName, onlineName := sandboxAccountNames(ownerSID)
	if manifest.Offline.Username != offlineName || manifest.Online.Username != onlineName {
		return errors.New("refusing to uninstall because manifest account names do not match this installation owner")
	}
	for _, account := range []*nativeCredential{&manifest.Offline, &manifest.Online} {
		sid, sidErr := lookupLocalAccountSID(account.Username)
		if errors.Is(sidErr, syscall.Errno(nerrUserNotFound)) {
			continue
		}
		if sidErr != nil {
			return fmt.Errorf("verify owned sandbox account %q before uninstall: %w", account.Username, sidErr)
		}
		if account.SID == "" {
			password, err := unprotectSandboxPassword(account.EncryptedPassword, ownerSID)
			if err != nil {
				return fmt.Errorf("cannot verify interrupted setup account %q ownership: %w", account.Username, err)
			}
			logonErr := probeAccountLogon(account.Username, password)
			zeroString(&password)
			if logonErr != nil {
				return fmt.Errorf("cannot verify interrupted setup account %q ownership: %w", account.Username, logonErr)
			}
			account.SID = sid.String()
			if err := writeNativeManifest(filepath.Join(installDir, "installation.json"), manifest, ownerSID); err != nil {
				return fmt.Errorf("persist verified sandbox account ownership before uninstall: %w", err)
			}
		} else if sid.String() != account.SID {
			return fmt.Errorf("refusing to delete sandbox account %q because its SID differs from the installation manifest", account.Username)
		}
	}
	manifest.Lifecycle = "uninstalling"
	if err := writeNativeManifest(filepath.Join(installDir, "installation.json"), manifest, ownerSID); err != nil {
		return fmt.Errorf("persist native sandbox uninstall state: %w", err)
	}
	for _, account := range []nativeCredential{manifest.Online, manifest.Offline} {
		if err := deleteSandboxAccount(account.Username); err != nil {
			return fmt.Errorf("delete owned sandbox account %q: %w", account.Username, err)
		}
	}
	if err := removeOwnedWFP(ownerSID); err != nil {
		return fmt.Errorf("remove owned persistent sandbox network policy: %w", err)
	}
	if err := verifyOwnedWFPAbsent(ownerSID); err != nil {
		return err
	}
	if err := removeOwnedRunnerFiles(installDir, manifest); err != nil {
		return fmt.Errorf("remove owned sandbox runner files: %w", err)
	}
	for _, path := range []string{filepath.Join(installDir, "journals"), filepath.Join(installDir, "sessions")} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty sandbox state directory %s: %w", path, err)
		}
	}
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return fmt.Errorf("inspect sandbox directory before removing ownership manifest: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != "installation.json" || entries[0].IsDir() {
		return errors.New("refusing to remove sandbox ownership manifest while unrecognized installation files remain")
	}
	if err := os.Remove(filepath.Join(installDir, "installation.json")); err != nil {
		return fmt.Errorf("remove owned sandbox manifest: %w", err)
	}
	if err := os.Remove(installDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty sandbox installation directory: %w", err)
	}
	if _, err := os.Lstat(installDir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandbox installation directory remains after uninstall: %s", installDir)
	}
	return nil
}

func createAndProtectAccount(name, ownerSID string) (sid, encrypted string, err error) {
	sid, password, err := createSandboxAccount(name)
	if err != nil {
		return "", "", err
	}
	defer zeroString(&password)
	protected, err := protectSandboxPassword(password, ownerSID)
	if err != nil {
		return "", "", errors.Join(err, deleteSandboxAccount(name))
	}
	return sid, protected, nil
}

func removeNewInstallDirectory(installDir string) error {
	entries, err := os.ReadDir(installDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		"axiom-command-runner.exe":     true,
		"axiom-command-runner.exe.new": true,
		"installation.json":            true,
		"journals":                     true,
		"sessions":                     true,
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("refusing setup rollback because unrecognized file %q is present", entry.Name())
		}
		path := filepath.Join(installDir, entry.Name())
		if _, _, err := nativeFileIdentity(path); err != nil {
			return fmt.Errorf("verify setup rollback path %q: %w", entry.Name(), err)
		}
		if entry.IsDir() {
			children, err := os.ReadDir(path)
			if err != nil {
				return fmt.Errorf("inspect setup rollback directory %q: %w", entry.Name(), err)
			}
			if len(children) != 0 {
				return fmt.Errorf("refusing setup rollback because directory %q is not empty", entry.Name())
			}
		}
	}
	for _, name := range []string{"axiom-command-runner.exe", "axiom-command-runner.exe.new", "journals", "sessions"} {
		path := filepath.Join(installDir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove setup rollback resource %q: %w", name, err)
		}
	}
	manifestPath := filepath.Join(installDir, "installation.json")
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove setup rollback ownership journal: %w", err)
	}
	if err := os.Remove(installDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty setup rollback directory: %w", err)
	}
	if _, err := os.Lstat(installDir); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, errors.New("setup rollback directory remains after removal"))
	}
	return nil
}

// ensureNativeManifestCredential creates or recovers an account only when the
// random password already stored in the protected manifest proves ownership.
func ensureNativeManifestCredential(manifest *nativeManifest, account *nativeCredential, ownerSID, runnerPath string, created *bool, manifestPath string) error {
	if account.Username == "" {
		return errors.New("sandbox account name is missing from the installation manifest")
	}
	if account.EncryptedPassword == "" {
		password, err := randomPassword()
		if err != nil {
			return err
		}
		protected, protectErr := protectSandboxPassword(password, ownerSID)
		zeroString(&password)
		if protectErr != nil {
			return protectErr
		}
		account.EncryptedPassword = protected
		if err := writeNativeManifest(manifestPath, *manifest, ownerSID); err != nil {
			return fmt.Errorf("persist protected credential before account creation: %w", err)
		}
	}
	password, err := unprotectSandboxPassword(account.EncryptedPassword, ownerSID)
	if err != nil {
		return err
	}
	defer zeroString(&password)

	sid, lookupErr := lookupLocalAccountSID(account.Username)
	if errors.Is(lookupErr, syscall.Errno(nerrUserNotFound)) {
		if account.SID != "" {
			return fmt.Errorf("owned sandbox account %q is missing; refusing to recreate it with a different SID", account.Username)
		}
		sidText, createErr := createSandboxAccountWithPassword(account.Username, password)
		if createErr != nil {
			return createErr
		}
		sid, err = windows.StringToSid(sidText)
		if err != nil {
			return fmt.Errorf("parse newly created sandbox account SID: %w", err)
		}
		*created = true
	} else if lookupErr != nil {
		return fmt.Errorf("inspect sandbox account %q: %w", account.Username, lookupErr)
	} else if account.SID != "" && sid.String() != account.SID {
		return fmt.Errorf("sandbox account %q SID does not match its installation manifest", account.Username)
	} else if err := probeAccountLogon(account.Username, password); err != nil {
		return fmt.Errorf("sandbox account %q does not authenticate with the protected installation credential: %w", account.Username, err)
	}
	if account.SID != "" && sid.String() != account.SID {
		return fmt.Errorf("sandbox account %q SID does not match its installation manifest", account.Username)
	}
	if err := ensureSandboxAccountBaseline(account.Username, sid.String()); err != nil {
		return err
	}
	if err := probeAccountRunner(runnerPath, account.Username, password); err != nil {
		return fmt.Errorf("sandbox runner Probe for %q: %w", account.Username, err)
	}
	if account.SID == "" {
		account.SID = sid.String()
		if err := writeNativeManifest(manifestPath, *manifest, ownerSID); err != nil {
			return fmt.Errorf("persist owned sandbox account SID: %w", err)
		}
	}
	return nil
}

func probeAccountLogon(username, password string) error {
	user16, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return err
	}
	domain16, _ := windows.UTF16PtrFromString(".")
	password16, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	defer clearUTF16(password16)
	var token windows.Token
	r1, _, callErr := procLogonUser.Call(
		uintptr(unsafe.Pointer(user16)), uintptr(unsafe.Pointer(domain16)), uintptr(unsafe.Pointer(password16)),
		2, 0, uintptr(unsafe.Pointer(&token)),
	)
	runtime.KeepAlive(password16)
	if r1 == 0 {
		return fmt.Errorf("LogonUserW: %w", callErr)
	}
	return token.Close()
}

func protectSandboxPassword(password, ownerSID string) (string, error) {
	plain := []byte(password)
	defer clear(plain)
	input := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	entropyRaw := []byte(ownerSID + "\x00O Agent Windows sandbox v2")
	defer clear(entropyRaw)
	entropy := windows.DataBlob{Size: uint32(len(entropyRaw)), Data: &entropyRaw[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, &entropy, 0, nil, 0x4, &output); err != nil {
		return "", fmt.Errorf("protect sandbox password with DPAPI machine scope: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return base64.StdEncoding.EncodeToString(unsafe.Slice(output.Data, output.Size)), nil
}

func unprotectSandboxPassword(encoded, ownerSID string) (string, error) {
	protected, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(protected) == 0 {
		return "", errors.New("sandbox credential ciphertext is invalid")
	}
	defer clear(protected)
	input := windows.DataBlob{Size: uint32(len(protected)), Data: &protected[0]}
	entropyRaw := []byte(ownerSID + "\x00O Agent Windows sandbox v2")
	defer clear(entropyRaw)
	entropy := windows.DataBlob{Size: uint32(len(entropyRaw)), Data: &entropyRaw[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, &entropy, 0, nil, 0, &output); err != nil {
		return "", fmt.Errorf("unprotect sandbox password with DPAPI machine scope: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return string(unsafe.Slice(output.Data, output.Size)), nil
}

func zeroString(value *string) {
	if value == nil {
		return
	}
	bytes := unsafe.Slice(unsafe.StringData(*value), len(*value))
	clear(bytes)
	*value = ""
}

func readNativeManifest(installDir string) (nativeManifest, error) {
	path := filepath.Join(installDir, "installation.json")
	info, err := os.Lstat(path)
	if err != nil {
		return nativeManifest{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 {
		return nativeManifest{}, errors.New("native sandbox manifest is not a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nativeManifest{}, err
	}
	var manifest nativeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nativeManifest{}, fmt.Errorf("decode native sandbox manifest: %w", err)
	}
	if manifest.Version != nativeManifestVersion || manifest.OwnerSID == "" {
		return nativeManifest{}, errors.New("native sandbox manifest is missing required ownership or resource fields")
	}
	if (manifest.PreviousRunnerSHA256 == "") != (manifest.PreviousRunnerVersion == "") || (manifest.PreviousRunnerSHA256 != "" && !validSHA256(manifest.PreviousRunnerSHA256)) || (manifest.CandidateRunnerSHA256 != "" && !validSHA256(manifest.CandidateRunnerSHA256)) {
		return nativeManifest{}, errors.New("native sandbox runner maintenance journal is invalid")
	}
	if manifest.Lifecycle == "installed" && manifest.CandidateRunnerSHA256 != "" && manifest.RunnerSHA256 != manifest.CandidateRunnerSHA256 {
		return nativeManifest{}, errors.New("installed sandbox runner does not match its maintenance journal")
	}
	if manifest.Lifecycle == "installing" || manifest.Lifecycle == "uninstalling" {
		if manifest.Offline.Username == "" || manifest.Online.Username == "" {
			return nativeManifest{}, errors.New("transitional sandbox manifest is missing account names")
		}
		return manifest, nil
	}
	if manifest.Lifecycle != "" && manifest.Lifecycle != "installed" {
		return nativeManifest{}, fmt.Errorf("unsupported native sandbox lifecycle %q", manifest.Lifecycle)
	}
	if manifest.Offline.Username == "" || manifest.Online.Username == "" || manifest.Offline.SID == "" || manifest.Online.SID == "" || manifest.Offline.EncryptedPassword == "" || manifest.Online.EncryptedPassword == "" || manifest.RunnerSHA256 == "" || len(manifest.WFPKeys) == 0 {
		return nativeManifest{}, errors.New("native sandbox manifest is missing required ownership or resource fields")
	}
	return manifest, nil
}

func writeNativeManifest(path string, manifest nativeManifest, ownerSID string) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode native sandbox installation manifest: %w", err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate manifest temporary name: %w", err)
	}
	tmp := path + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary sandbox manifest: %w", err)
	}
	writeErr := func() error {
		if _, err := f.Write(data); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		return f.Close()
	}()
	if writeErr != nil {
		return errors.Join(fmt.Errorf("persist temporary sandbox manifest: %w", writeErr), f.Close(), removeNativeTemporaryFile(tmp))
	}
	if err := secureNativeFile(tmp, ownerSID, false); err != nil {
		return errors.Join(err, removeNativeTemporaryFile(tmp))
	}
	if err := windows.MoveFileEx(windows.StringToUTF16Ptr(tmp), windows.StringToUTF16Ptr(path), windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return errors.Join(fmt.Errorf("atomically replace native sandbox manifest: %w", err), removeNativeTemporaryFile(tmp))
	}
	readBack, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back native sandbox manifest: %w", err)
	}
	if !equalBytes(data, readBack) {
		return errors.New("native sandbox manifest read-back mismatch")
	}
	return nil
}

func removeNativeTemporaryFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove non-regular temporary sandbox file: %s", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("temporary sandbox file remains: %s", path))
	}
	return nil
}

func validateManifestAccounts(manifest nativeManifest) error {
	for _, account := range []nativeCredential{manifest.Offline, manifest.Online} {
		sid, err := lookupLocalAccountSID(account.Username)
		if err != nil {
			return fmt.Errorf("sandbox account %q is missing: %w", account.Username, err)
		}
		if sid.String() != account.SID {
			return fmt.Errorf("sandbox account %q SID changed from its owned manifest", account.Username)
		}
		if err := validateSandboxAccount(account.Username, account.SID); err != nil {
			return err
		}
		password, err := unprotectSandboxPassword(account.EncryptedPassword, manifest.OwnerSID)
		if err != nil {
			return fmt.Errorf("sandbox account %q credential is unavailable: %w", account.Username, err)
		}
		zeroString(&password)
	}
	return nil
}

func probeNativeInstallation(manifest nativeManifest, runnerPath string) (resultErr error) {
	if manifest.OwnerSID == "" {
		return errors.New("installation owner SID is empty")
	}
	if manifest.RunnerVersion != nativeRunnerVersion {
		return fmt.Errorf("sandbox runner version %q requires repair to version %q", manifest.RunnerVersion, nativeRunnerVersion)
	}
	if err := validateManifestAccounts(manifest); err != nil {
		return err
	}
	hash, err := sha256File(runnerPath)
	if err != nil {
		return err
	}
	if hash != manifest.RunnerSHA256 {
		return errors.New("installed runner hash does not match the owned manifest")
	}
	keys, err := ownedWFPKeys(manifest.OwnerSID)
	if err != nil {
		return err
	}
	if !equalStringLists(manifest.WFPKeys, wfpKeyStrings(keys)) {
		return errors.New("manifest does not contain this installation's exact owned WFP rule set")
	}
	engine, err := openWFP()
	if err != nil {
		return fmt.Errorf("open WFP for health Probe: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, closeWFP(engine)) }()
	if err := verifyWFPFilters(engine, manifest.OwnerSID, keys); err != nil {
		return err
	}
	for _, account := range []nativeCredential{manifest.Offline, manifest.Online} {
		password, err := unprotectSandboxPassword(account.EncryptedPassword, manifest.OwnerSID)
		if err != nil {
			return err
		}
		probeErr := probeAccountRunner(runnerPath, account.Username, password)
		zeroString(&password)
		if probeErr != nil {
			return fmt.Errorf("logon Probe for %q failed: %w", account.Username, probeErr)
		}
	}
	return nil
}

func probeAccountRunner(runnerPath, username, password string) (resultErr error) {
	user16, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return err
	}
	domain16, _ := windows.UTF16PtrFromString(".")
	password16, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	defer clearUTF16(password16)
	app16, err := windows.UTF16PtrFromString(runnerPath)
	if err != nil {
		return err
	}
	cmd16, err := windows.UTF16PtrFromString("\"" + runnerPath + "\" --probe")
	if err != nil {
		return err
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var processInfo windows.ProcessInformation
	r1, _, callErr := procCreateProcessWithLogon.Call(uintptr(unsafe.Pointer(user16)), uintptr(unsafe.Pointer(domain16)), uintptr(unsafe.Pointer(password16)), 1, uintptr(unsafe.Pointer(app16)), uintptr(unsafe.Pointer(cmd16)), windows.CREATE_NO_WINDOW, 0, 0, uintptr(unsafe.Pointer(&startup)), uintptr(unsafe.Pointer(&processInfo)))
	runtime.KeepAlive(password16)
	if r1 == 0 {
		return fmt.Errorf("CreateProcessWithLogonW: %w", callErr)
	}
	defer func() {
		resultErr = errors.Join(resultErr, windows.CloseHandle(processInfo.Thread), windows.CloseHandle(processInfo.Process))
	}()
	wait, err := windows.WaitForSingleObject(processInfo.Process, 30_000)
	if err != nil {
		return fmt.Errorf("wait for runner Probe: %w", err)
	}
	if wait != windows.WAIT_OBJECT_0 {
		terminateErr := windows.TerminateProcess(processInfo.Process, 1)
		wait, waitErr := windows.WaitForSingleObject(processInfo.Process, 30_000)
		if waitErr == nil && wait != windows.WAIT_OBJECT_0 {
			waitErr = fmt.Errorf("runner Probe remained active after termination (wait=%d)", wait)
		}
		return errors.Join(errors.New("runner Probe timed out"), terminateErr, waitErr)
	}
	var exitCode uint32
	r1, _, callErr = procGetExitCodeProcess.Call(uintptr(processInfo.Process), uintptr(unsafe.Pointer(&exitCode)))
	if r1 == 0 {
		return callErr
	}
	if exitCode != 0 {
		return fmt.Errorf("runner Probe exited with code %d", exitCode)
	}
	return nil
}

func stageProtectedRunner(source, destination, ownerSID string) (string, error) {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("inspect sandbox runner source: %w", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return "", errors.New("sandbox runner source is not a regular file")
	}
	if filepath.Clean(source) == filepath.Clean(destination) {
		return "", errors.New("sandbox runner source cannot be the staging destination")
	}
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tmp := destination + ".new"
	if _, err := os.Lstat(tmp); err == nil {
		return "", fmt.Errorf("sandbox runner staging path already exists: %s", tmp)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500)
	if err != nil {
		return "", fmt.Errorf("create staged sandbox runner: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", errors.Join(fmt.Errorf("stage trusted sandbox runner: %w", err), removeStagedRunner(tmp))
	}
	if err := secureNativeFile(tmp, ownerSID, true); err != nil {
		return "", errors.Join(err, removeStagedRunner(tmp))
	}
	hash, err := sha256File(tmp)
	if err != nil {
		return "", errors.Join(fmt.Errorf("verify staged sandbox runner: %w", err), removeStagedRunner(tmp))
	}
	return hash, nil
}

func activateStagedRunner(destination, previousHash, candidateHash string) error {
	staged := destination + ".new"
	backup := destination + ".previous"
	stagedHash, err := sha256File(staged)
	if err != nil || stagedHash != candidateHash {
		return errors.Join(errors.New("staged sandbox runner hash changed before activation"), err)
	}
	if previousHash != "" {
		currentHash, err := sha256File(destination)
		if err != nil || currentHash != previousHash {
			return errors.Join(errors.New("installed sandbox runner does not match the manifest before upgrade"), err)
		}
		if _, err := os.Lstat(backup); err == nil {
			return fmt.Errorf("sandbox runner backup path already exists: %s", backup)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := windows.MoveFileEx(windows.StringToUTF16Ptr(destination), windows.StringToUTF16Ptr(backup), windows.MOVEFILE_WRITE_THROUGH); err != nil {
			return fmt.Errorf("preserve previous sandbox runner: %w", err)
		}
	} else if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("refusing to replace an unowned sandbox runner: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := windows.MoveFileEx(windows.StringToUTF16Ptr(staged), windows.StringToUTF16Ptr(destination), windows.MOVEFILE_WRITE_THROUGH); err != nil {
		if previousHash != "" {
			restoreErr := windows.MoveFileEx(windows.StringToUTF16Ptr(backup), windows.StringToUTF16Ptr(destination), windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
			return errors.Join(fmt.Errorf("activate staged sandbox runner: %w", err), restoreErr)
		}
		return fmt.Errorf("activate staged sandbox runner: %w", err)
	}
	installedHash, err := sha256File(destination)
	if err != nil || installedHash != candidateHash {
		var restoreErr error
		if previousHash != "" {
			restoreErr = windows.MoveFileEx(windows.StringToUTF16Ptr(backup), windows.StringToUTF16Ptr(destination), windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		} else {
			restoreErr = os.Remove(destination)
		}
		return errors.Join(errors.New("activated sandbox runner hash verification failed"), err, restoreErr)
	}
	return nil
}

func probeManifestAccountRunner(runnerPath string, account nativeCredential, ownerSID string) error {
	password, err := unprotectSandboxPassword(account.EncryptedPassword, ownerSID)
	if err != nil {
		return fmt.Errorf("unlock sandbox account %q for runner Probe: %w", account.Username, err)
	}
	err = probeAccountRunner(runnerPath, account.Username, password)
	zeroString(&password)
	return err
}

func removeStagedRunner(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove non-regular sandbox runner staging path: %s", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("sandbox runner staging file remains: %s", path))
	}
	return nil
}

func recoverNativeRunnerTransaction(installDir string, manifest *nativeManifest, ownerSID string) error {
	if manifest == nil || (manifest.PreviousRunnerSHA256 == "" && manifest.PreviousRunnerVersion == "" && manifest.CandidateRunnerSHA256 == "") {
		return nil
	}
	if manifest.CandidateRunnerSHA256 == "" || !validSHA256(manifest.CandidateRunnerSHA256) || (manifest.PreviousRunnerSHA256 == "") != (manifest.PreviousRunnerVersion == "") || (manifest.PreviousRunnerSHA256 != "" && !validSHA256(manifest.PreviousRunnerSHA256)) {
		return errors.New("sandbox runner maintenance journal has an invalid hash or version")
	}
	runner := filepath.Join(installDir, "axiom-command-runner.exe")
	backup := runner + ".previous"
	stage := runner + ".new"
	manifestPath := filepath.Join(installDir, "installation.json")
	if manifest.Lifecycle == "installed" {
		if manifest.RunnerSHA256 != manifest.CandidateRunnerSHA256 {
			return errors.New("committed sandbox runner does not match its maintenance journal")
		}
		activeHash, err := sha256File(runner)
		if err != nil || activeHash != manifest.CandidateRunnerSHA256 {
			return errors.Join(errors.New("committed sandbox runner hash does not match its manifest"), err)
		}
		if manifest.PreviousRunnerSHA256 != "" {
			if err := removeRunnerBackup(backup, manifest.PreviousRunnerSHA256); err != nil {
				return err
			}
		} else if _, err := os.Lstat(backup); err == nil {
			return errors.New("unowned sandbox runner backup exists without a previous runner hash")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := removeStagedRunner(stage); err != nil {
			return err
		}
		clearNativeRunnerTransaction(manifest)
		if err := writeNativeManifest(manifestPath, *manifest, ownerSID); err != nil {
			return fmt.Errorf("persist recovered sandbox runner maintenance state: %w", err)
		}
		return verifyNativeRunnerManifestReadback(installDir, *manifest)
	}
	if manifest.Lifecycle != "installing" {
		return fmt.Errorf("cannot recover sandbox runner maintenance in lifecycle %q", manifest.Lifecycle)
	}
	if manifest.PreviousRunnerSHA256 == "" {
		if activeHash, err := sha256File(runner); err == nil {
			if activeHash != manifest.CandidateRunnerSHA256 {
				return errors.New("refusing to remove an unrecognized runner from interrupted first installation")
			}
			if err := os.Remove(runner); err != nil {
				return fmt.Errorf("remove incomplete first-install runner: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := os.Lstat(backup); err == nil {
			return errors.New("unexpected backup exists during first sandbox installation")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := removeStagedRunner(stage); err != nil {
			return err
		}
		manifest.CandidateRunnerSHA256 = ""
		if err := writeNativeManifest(manifestPath, *manifest, ownerSID); err != nil {
			return fmt.Errorf("persist interrupted first-install recovery: %w", err)
		}
		return nil
	}
	if backupHash, err := sha256File(backup); err == nil {
		if backupHash != manifest.PreviousRunnerSHA256 {
			return errors.New("sandbox runner backup does not match the previous manifest hash")
		}
		if err := windows.MoveFileEx(windows.StringToUTF16Ptr(backup), windows.StringToUTF16Ptr(runner), windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
			return fmt.Errorf("restore previous sandbox runner after interrupted upgrade: %w", err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		activeHash, activeErr := sha256File(runner)
		if activeErr != nil || activeHash != manifest.PreviousRunnerSHA256 {
			return errors.Join(errors.New("previous sandbox runner is unavailable after interrupted upgrade"), activeErr)
		}
	} else {
		return fmt.Errorf("inspect sandbox runner backup: %w", err)
	}
	activeHash, err := sha256File(runner)
	if err != nil || activeHash != manifest.PreviousRunnerSHA256 {
		return errors.Join(errors.New("restored sandbox runner does not match its previous hash"), err)
	}
	if err := removeStagedRunner(stage); err != nil {
		return err
	}
	manifest.RunnerVersion = manifest.PreviousRunnerVersion
	manifest.RunnerSHA256 = manifest.PreviousRunnerSHA256
	manifest.Lifecycle = "installed"
	clearNativeRunnerTransaction(manifest)
	if err := writeNativeManifest(manifestPath, *manifest, ownerSID); err != nil {
		return fmt.Errorf("persist previous sandbox runner recovery: %w", err)
	}
	return verifyNativeRunnerManifestReadback(installDir, *manifest)
}

func cleanupNativeRunnerTransaction(installDir string, manifest *nativeManifest, ownerSID string) error {
	if manifest == nil || manifest.Lifecycle != "installed" || manifest.CandidateRunnerSHA256 == "" || manifest.RunnerSHA256 != manifest.CandidateRunnerSHA256 {
		return errors.New("sandbox runner cleanup has no committed candidate")
	}
	runner := filepath.Join(installDir, "axiom-command-runner.exe")
	activeHash, err := sha256File(runner)
	if err != nil || activeHash != manifest.CandidateRunnerSHA256 {
		return errors.Join(errors.New("sandbox runner changed before maintenance cleanup"), err)
	}
	if manifest.PreviousRunnerSHA256 != "" {
		if err := removeRunnerBackup(runner+".previous", manifest.PreviousRunnerSHA256); err != nil {
			return err
		}
	} else if _, err := os.Lstat(runner + ".previous"); err == nil {
		return errors.New("unexpected sandbox runner backup exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := removeStagedRunner(runner + ".new"); err != nil {
		return err
	}
	clearNativeRunnerTransaction(manifest)
	manifestPath := filepath.Join(installDir, "installation.json")
	if err := writeNativeManifest(manifestPath, *manifest, ownerSID); err != nil {
		return fmt.Errorf("persist sandbox runner maintenance cleanup: %w", err)
	}
	if err := verifyNativeRunnerManifestReadback(installDir, *manifest); err != nil {
		return err
	}
	return probeNativeInstallation(*manifest, runner)
}

func removeRunnerBackup(path, expectedHash string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove non-regular sandbox runner backup: %s", path)
	}
	hash, err := sha256File(path)
	if err != nil || hash != expectedHash {
		return errors.Join(fmt.Errorf("sandbox runner backup hash mismatch: %s", path), err)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("sandbox runner backup remains: %s", path))
	}
	return nil
}

func removeOwnedRunnerFiles(installDir string, manifest nativeManifest) error {
	runner := filepath.Join(installDir, "axiom-command-runner.exe")
	if err := removeOwnedRunner(runner, manifest.RunnerSHA256, manifest.CandidateRunnerSHA256, manifest.PreviousRunnerSHA256); err != nil {
		return err
	}
	if manifest.PreviousRunnerSHA256 != "" {
		if err := removeRunnerBackup(runner+".previous", manifest.PreviousRunnerSHA256); err != nil {
			return err
		}
	} else if _, err := os.Lstat(runner + ".previous"); err == nil {
		return errors.New("refusing to remove unowned sandbox runner backup")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(runner + ".new"); err == nil {
		if manifest.CandidateRunnerSHA256 == "" {
			return errors.New("refusing to remove unowned sandbox runner staging file")
		}
		if err := removeStagedRunner(runner + ".new"); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeOwnedRunner(path string, acceptedHashes ...string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove non-regular sandbox runner: %s", path)
	}
	hash, err := sha256File(path)
	if err != nil {
		return err
	}
	owned := false
	for _, accepted := range acceptedHashes {
		if accepted != "" && hash == accepted {
			owned = true
			break
		}
	}
	if !owned {
		return fmt.Errorf("refusing to remove sandbox runner with an unrecognized hash: %s", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("sandbox runner remains after removal: %s", path))
	}
	return nil
}

func clearNativeRunnerTransaction(manifest *nativeManifest) {
	manifest.PreviousRunnerVersion = ""
	manifest.PreviousRunnerSHA256 = ""
	manifest.CandidateRunnerSHA256 = ""
}

func verifyNativeRunnerManifestReadback(installDir string, expected nativeManifest) error {
	readBack, err := readNativeManifest(installDir)
	if err != nil {
		return err
	}
	if readBack.Lifecycle != expected.Lifecycle || readBack.RunnerVersion != expected.RunnerVersion || readBack.RunnerSHA256 != expected.RunnerSHA256 || readBack.PreviousRunnerSHA256 != expected.PreviousRunnerSHA256 || readBack.PreviousRunnerVersion != expected.PreviousRunnerVersion || readBack.CandidateRunnerSHA256 != expected.CandidateRunnerSHA256 {
		return errors.New("sandbox runner maintenance manifest read-back mismatch")
	}
	return nil
}

func validSHA256(hash string) bool {
	if len(hash) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func secureNativeDirectory(path, ownerSID string) error {
	sddl := fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)(A;;0x20;;;BU)", ownerSID)
	return setNativePathDACL(path, sddl)
}

func secureNativeFile(path, ownerSID string, runner bool) error {
	sddl := fmt.Sprintf("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)", ownerSID)
	if runner {
		sddl += "(A;;GRGX;;;BU)"
	}
	return setNativePathDACL(path, sddl)
}

func setNativePathDACL(path, sddl string) error {
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("build protected sandbox ACL: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read protected sandbox ACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("apply protected sandbox ACL to %s: %w", path, err)
	}
	readBack, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read back protected sandbox ACL for %s: %w", path, err)
	}
	if readBack == nil {
		return fmt.Errorf("protected sandbox ACL read-back returned no descriptor for %s", path)
	}
	expectedDACL, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read requested protected DACL for %s: %w", path, err)
	}
	actualDACL, _, err := readBack.DACL()
	if err != nil {
		return fmt.Errorf("read back protected DACL for %s: %w", path, err)
	}
	if !nativeDACLProtected(readBack.String()) || !nativeDACLMatches(expectedDACL, actualDACL) {
		actual := ""
		actual = nativeDACLSDDL(readBack.String())
		return fmt.Errorf("protected sandbox ACL read-back does not match the requested DACL for %s (requested=%q actual=%q)", path, nativeDACLSDDL(descriptor.String()), actual)
	}
	return nil
}

func nativeDACLProtected(securityDescriptor string) bool {
	dacl := nativeDACLSDDL(securityDescriptor)
	firstACE := strings.Index(dacl, "(")
	return firstACE >= 0 && strings.Contains(dacl[len("D:"):firstACE], "P")
}

func nativeDACLMatches(expected, actual *windows.ACL) bool {
	expectedEntries, err := nativeDACLACEKeys(expected)
	if err != nil {
		return false
	}
	actualEntries, err := nativeDACLACEKeys(actual)
	if err != nil || len(expectedEntries) != len(actualEntries) {
		return false
	}
	sort.Strings(expectedEntries)
	sort.Strings(actualEntries)
	for index := range expectedEntries {
		if expectedEntries[index] != actualEntries[index] {
			return false
		}
	}
	return true
}

func nativeDACLACEKeys(acl *windows.ACL) ([]string, error) {
	if acl == nil {
		return nil, errors.New("DACL is missing")
	}
	entries := make([]string, 0, acl.AceCount)
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return nil, err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return nil, errors.New("DACL contains an unsupported ACE type")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return nil, errors.New("DACL contains an invalid trustee SID")
		}
		mask := uint32(ace.Mask)
		if mask&windows.GENERIC_READ != 0 {
			mask = mask&^windows.GENERIC_READ | uint32(windows.FILE_GENERIC_READ)
		}
		if mask&windows.GENERIC_WRITE != 0 {
			mask = mask&^windows.GENERIC_WRITE | uint32(windows.FILE_GENERIC_WRITE)
		}
		if mask&windows.GENERIC_EXECUTE != 0 {
			mask = mask&^windows.GENERIC_EXECUTE | uint32(windows.FILE_GENERIC_EXECUTE)
		}
		if mask&windows.GENERIC_ALL != 0 {
			mask = mask&^windows.GENERIC_ALL | 0x001F01FF
		}
		entries = append(entries, fmt.Sprintf("%02X:%02X:%08X:%s", ace.Header.AceType, ace.Header.AceFlags, mask, sid.String()))
	}
	return entries, nil
}

func nativeDACLSDDL(securityDescriptor string) string {
	upper := strings.ToUpper(securityDescriptor)
	start := strings.Index(upper, "D:")
	if start < 0 {
		return ""
	}
	end := len(upper)
	if sacl := strings.Index(upper[start+2:], "S:"); sacl >= 0 {
		end = start + 2 + sacl
	}
	dacl := upper[start:end]
	if firstACE := strings.Index(dacl, "("); firstACE >= 0 {
		// Windows may mark the resulting protected DACL as auto-inherited even
		// though the requested SDDL omitted that informational control bit.
		control := strings.ReplaceAll(dacl[:firstACE], "AI", "")
		dacl = control + dacl[firstACE:]
	}
	return dacl
}

func checkedInstallDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("sandbox installation directory is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(absolute)
	volumeRoot := filepath.VolumeName(clean) + string(filepath.Separator)
	if strings.EqualFold(clean, volumeRoot) || len(clean) < len(volumeRoot)+4 {
		return "", errors.New("sandbox installation directory is too broad")
	}
	if _, _, err := nativeFileIdentity(filepath.Dir(clean)); err != nil {
		return "", fmt.Errorf("validate sandbox installation parent: %w", err)
	}
	if _, err := os.Lstat(clean); err == nil {
		if _, _, err := nativeFileIdentity(clean); err != nil {
			return "", fmt.Errorf("validate existing sandbox installation directory: %w", err)
		}
		info, err := os.Stat(clean)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", errors.New("sandbox installation path is not a directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect sandbox installation path: %w", err)
	}
	return clean, nil
}

func acquireSetupLock(ownerSID string) (windows.Handle, func(windows.Handle) error, error) {
	return acquireSetupLockContext(context.Background(), ownerSID)
}

func acquireSetupLockContext(ctx context.Context, ownerSID string) (windows.Handle, func(windows.Handle) error, error) {
	runtime.LockOSThread()
	threadPinned := true
	unlockThread := func() {
		if threadPinned {
			threadPinned = false
			runtime.UnlockOSThread()
		}
	}
	name := "Global\\OAgentSandboxSetup_" + wfpDigest(ownerSID)
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		unlockThread()
		return 0, nil, err
	}
	r1, _, callErr := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(name16)))
	if r1 == 0 {
		unlockThread()
		return 0, nil, fmt.Errorf("create sandbox setup mutex: %w", callErr)
	}
	handle := windows.Handle(r1)
	for {
		if err := ctx.Err(); err != nil {
			closeErr := windows.CloseHandle(handle)
			unlockThread()
			return 0, nil, errors.Join(err, closeErr)
		}
		wait, err := windows.WaitForSingleObject(handle, 100)
		if err != nil {
			closeErr := windows.CloseHandle(handle)
			unlockThread()
			return 0, nil, errors.Join(err, closeErr)
		}
		if wait == windows.WAIT_OBJECT_0 || wait == windows.WAIT_ABANDONED {
			break
		}
		if wait != uint32(windows.WAIT_TIMEOUT) {
			closeErr := windows.CloseHandle(handle)
			unlockThread()
			return 0, nil, errors.Join(fmt.Errorf("unexpected setup mutex wait result 0x%x", wait), closeErr)
		}
	}
	return handle, func(handle windows.Handle) error {
		var joined error
		if r1, _, callErr := procReleaseMutex.Call(uintptr(handle)); r1 == 0 {
			joined = errors.Join(joined, fmt.Errorf("release sandbox setup mutex: %w", callErr))
		}
		joined = errors.Join(joined, windows.CloseHandle(handle))
		unlockThread()
		return joined
	}, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func equalStringLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("input exceeds sandbox protocol limit")
	}
	return data, nil
}

func makeCommandID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// Keep imported low-level functions anchored while the runner protocol is
// implemented below; these handles are also used by the authentication path.
var _ = procGetCurrentProcessID
var _ = procGetNamedPipeClientPID
var _ = syscall.Errno(0)
