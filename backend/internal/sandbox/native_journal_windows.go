//go:build windows

package sandbox

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const maxNativeJournalSize = 1 << 20

type nativeACLRecord struct {
	Path         string `json:"path"`
	VolumeSerial uint32 `json:"volumeSerial"`
	FileIndex    uint64 `json:"fileIndex"`
	Write        bool   `json:"write,omitempty"`
	Deny         bool   `json:"deny,omitempty"`
	Revoked      bool   `json:"revoked,omitempty"`
	TreeObject   bool   `json:"treeObject,omitempty"`
}

type nativeCommandJournal struct {
	Version           int               `json:"version"`
	CommandID         string            `json:"commandId"`
	OwnerPID          int               `json:"ownerPid"`
	OwnerStart        uint64            `json:"ownerStart"`
	RunnerPID         uint32            `json:"runnerPid,omitempty"`
	RunnerStart       uint64            `json:"runnerStart,omitempty"`
	AccountSID        string            `json:"accountSid"`
	LogonSID          string            `json:"logonSid,omitempty"`
	RestrictedSIDs    []string          `json:"restrictedSids,omitempty"`
	EnabledPrivileges []string          `json:"enabledPrivileges,omitempty"`
	ScratchDir        string            `json:"scratchDir"`
	ScratchVolume     uint32            `json:"scratchVolume,omitempty"`
	ScratchIndex      uint64            `json:"scratchIndex,omitempty"`
	Stage             string            `json:"stage"`
	ACLs              []nativeACLRecord `json:"acls"`
	CreatedAt         string            `json:"createdAt"`
}

func revokeNativeSIDTree(journalPath string, journal *nativeCommandJournal, sid *windows.SID, ownerSID string) error {
	if journal == nil || sid == nil || !sid.IsValid() {
		return errors.New("writable-tree ACL cleanup is missing its journal or logon SID")
	}
	known := make(map[string]int, len(journal.ACLs))
	for index, record := range journal.ACLs {
		key := strings.ToLower(filepath.Clean(record.Path))
		if prior, ok := known[key]; ok {
			if journal.ACLs[prior].VolumeSerial != record.VolumeSerial || journal.ACLs[prior].FileIndex != record.FileIndex {
				return fmt.Errorf("journal contains conflicting filesystem identities for %s", record.Path)
			}
			continue
		}
		known[key] = index
	}
	validateRecord := func(index int) error {
		record := journal.ACLs[index]
		volume, fileIndex, err := nativeFileIdentity(record.Path)
		if err != nil {
			return fmt.Errorf("verify journaled ACL cleanup object %s: %w", record.Path, err)
		}
		if volume != record.VolumeSerial || fileIndex != record.FileIndex {
			return fmt.Errorf("refuse ACL cleanup because %s now identifies a different filesystem object", record.Path)
		}
		return nil
	}
	roots := make([]int, 0, len(journal.ACLs))
	for index, record := range journal.ACLs {
		if record.Write && !record.TreeObject {
			roots = append(roots, index)
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		return len(filepath.Clean(journal.ACLs[roots[i]].Path)) < len(filepath.Clean(journal.ACLs[roots[j]].Path))
	})
	selectedRoots := make([]int, 0, len(roots))
	for _, index := range roots {
		path := filepath.Clean(journal.ACLs[index].Path)
		covered := false
		for _, rootIndex := range selectedRoots {
			if pathWithin(filepath.Clean(journal.ACLs[rootIndex].Path), path) {
				covered = true
				break
			}
		}
		if !covered {
			selectedRoots = append(selectedRoots, index)
		}
	}
	for _, index := range selectedRoots {
		if err := validateRecord(index); err != nil {
			return err
		}
		if journal.ACLs[index].Revoked {
			continue
		}
		record := journal.ACLs[index]
		if err := setNativeAccess(record.Path, sid, windows.REVOKE_ACCESS, true, false, record.VolumeSerial, record.FileIndex); err != nil {
			return fmt.Errorf("revoke logon SID from writable root %s: %w", record.Path, err)
		}
		journal.ACLs[index].Revoked = true
		if err := writeNativeJournal(journalPath, *journal, ownerSID); err != nil {
			return fmt.Errorf("persist writable-root ACL cleanup for %s: %w", record.Path, err)
		}
	}
	pending := make(map[int]bool)
	addPending := func(index int) {
		if !journal.ACLs[index].Revoked {
			pending[index] = true
		}
	}
	for index, record := range journal.ACLs {
		if record.TreeObject {
			if err := validateRecord(index); err != nil {
				return err
			}
			addPending(index)
		}
	}
	var visit func(string, int) error
	visit = func(path string, rootIndex int) error {
		volume, fileIndex, err := nativeFileIdentity(path)
		if err != nil {
			return fmt.Errorf("inspect writable-tree ACL cleanup object %s: %w", path, err)
		}
		key := strings.ToLower(filepath.Clean(path))
		index, exists := known[key]
		if strings.EqualFold(filepath.Clean(path), filepath.Clean(journal.ACLs[rootIndex].Path)) {
			index, exists = rootIndex, true
		}
		if exists {
			record := journal.ACLs[index]
			if record.VolumeSerial != volume || record.FileIndex != fileIndex {
				return fmt.Errorf("refuse ACL cleanup because %s now identifies a different filesystem object", path)
			}
			addPending(index)
		} else {
			descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				return fmt.Errorf("read writable-tree DACL for %s: %w", path, err)
			}
			if descriptor == nil {
				return fmt.Errorf("writable-tree DACL for %s is missing", path)
			}
			dacl, _, err := descriptor.DACL()
			if err != nil {
				return err
			}
			allow, err := explicitACLHasSID(dacl, sid, false)
			if err != nil {
				return err
			}
			deny, err := explicitACLHasSID(dacl, sid, true)
			if err != nil {
				return err
			}
			if allow || deny {
				verifyVolume, verifyIndex, verifyErr := nativeFileIdentity(path)
				if verifyErr != nil || verifyVolume != volume || verifyIndex != fileIndex {
					return errors.Join(fmt.Errorf("filesystem object changed while preparing ACL cleanup for %s", path), verifyErr)
				}
				journal.ACLs = append(journal.ACLs, nativeACLRecord{Path: filepath.Clean(path), VolumeSerial: volume, FileIndex: fileIndex, TreeObject: true})
				index = len(journal.ACLs) - 1
				known[key] = index
				addPending(index)
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect writable-tree ACL cleanup object %s: %w", path, err)
		}
		if !info.IsDir() {
			return nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("list writable-tree ACL cleanup directory %s: %w", path, err)
		}
		for _, entry := range entries {
			if err := visit(filepath.Join(path, entry.Name()), rootIndex); err != nil {
				return err
			}
		}
		return nil
	}
	for _, rootIndex := range selectedRoots {
		if err := visit(journal.ACLs[rootIndex].Path, rootIndex); err != nil {
			return err
		}
	}
	if len(pending) == 0 {
		return nil
	}
	indexes := make([]int, 0, len(pending))
	for index := range pending {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	// Persist all discovered identities before changing any descendant DACL.
	// If cleanup is interrupted, recovery can safely repeat the idempotent ACE
	// removals against these exact objects without rewriting the journal per file.
	if err := writeNativeJournal(journalPath, *journal, ownerSID); err != nil {
		return fmt.Errorf("persist discovered writable-tree cleanup objects: %w", err)
	}
	for _, index := range indexes {
		record := journal.ACLs[index]
		if err := setNativeAccess(record.Path, sid, windows.REVOKE_ACCESS, true, false, record.VolumeSerial, record.FileIndex); err != nil {
			return fmt.Errorf("revoke logon SID from %s: %w", record.Path, err)
		}
	}
	for _, index := range indexes {
		journal.ACLs[index].Revoked = true
	}
	if err := writeNativeJournal(journalPath, *journal, ownerSID); err != nil {
		return fmt.Errorf("persist writable-tree ACL cleanup results: %w", err)
	}
	return nil
}

func nativeJournalDirectory(installDir, ownerSID string) (string, error) {
	dir := filepath.Join(installDir, "journals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create native sandbox journal directory: %w", err)
	}
	if err := secureNativeDirectory(dir, ownerSID); err != nil {
		return "", err
	}
	return dir, nil
}

func writeNativeJournal(path string, journal nativeCommandJournal, ownerSID string) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("encode native command journal: %w", err)
	}
	if len(data) == 0 || len(data) > maxNativeJournalSize {
		return fmt.Errorf("native command journal is %d bytes; maximum is %d", len(data), maxNativeJournalSize)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	tmp := path + "." + hex.EncodeToString(random[:]) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary native command journal: %w", err)
	}
	if _, err := f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return errors.Join(fmt.Errorf("persist native command journal: %w", err), removeNativeTemporaryFile(tmp))
	}
	if err := secureNativeFile(tmp, ownerSID, false); err != nil {
		return errors.Join(err, removeNativeTemporaryFile(tmp))
	}
	if err := windows.MoveFileEx(windows.StringToUTF16Ptr(tmp), windows.StringToUTF16Ptr(path), windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return errors.Join(fmt.Errorf("atomically update native command journal: %w", err), removeNativeTemporaryFile(tmp))
	}
	readBack, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back native command journal: %w", err)
	}
	var verified nativeCommandJournal
	if err := json.Unmarshal(readBack, &verified); err != nil {
		return err
	}
	if !reflect.DeepEqual(verified, journal) {
		return errors.New("native command journal read-back mismatch")
	}
	return nil
}

func RecoverNativeJournals(installDir string) (RecoveryReport, error) {
	manifest, err := readNativeManifest(installDir)
	if errors.Is(err, os.ErrNotExist) {
		return RecoveryReport{}, nil
	}
	if err != nil {
		return RecoveryReport{}, fmt.Errorf("read native sandbox manifest before recovery: %w", err)
	}
	mutex, release, err := acquireSetupLock(manifest.OwnerSID)
	if err != nil {
		return RecoveryReport{}, fmt.Errorf("acquire sandbox recovery lock: %w", err)
	}
	report, recoverErr := recoverNativeJournalsLocked(installDir)
	releaseErr := release(mutex)
	return report, errors.Join(recoverErr, releaseErr)
}

func nativeJournalGrantConflicts(records []nativeACLRecord, roots []string) (string, string, bool) {
	for _, grant := range records {
		if grant.Revoked {
			continue
		}
		for _, root := range roots {
			if pathWithin(grant.Path, root) || pathWithin(root, grant.Path) {
				return root, grant.Path, true
			}
		}
	}
	return "", "", false
}

func nativeJournalBlocksScope(journal nativeCommandJournal, active bool, roots []string) (string, string, bool) {
	if (active && journal.Stage != "cleaning") || journal.LogonSID == "" {
		return "", "", false
	}
	return nativeJournalGrantConflicts(journal.ACLs, roots)
}

func validateNativeCommandJournal(entryName string, journal nativeCommandJournal, manifest nativeManifest) error {
	if (journal.Version != 2 && journal.Version != 3 && journal.Version != 4) ||
		!validNativeCommandID(journal.CommandID) || entryName != "command-"+journal.CommandID+".json" ||
		journal.OwnerPID <= 0 || journal.OwnerStart == 0 ||
		(journal.RunnerPID == 0) != (journal.RunnerStart == 0) ||
		(journal.Stage != "prepared" && journal.Stage != "granted" && journal.Stage != "running" && journal.Stage != "cleaning" && journal.Stage != "finished") ||
		(journal.AccountSID != manifest.Offline.SID && journal.AccountSID != manifest.Online.SID) {
		return errors.New("invalid command ownership or lifecycle data")
	}
	if journal.LogonSID == "" {
		if journal.Stage != "prepared" || journal.RunnerPID != 0 {
			return errors.New("command journal is missing its authenticated logon SID")
		}
	} else {
		logonSID, err := windows.StringToSid(journal.LogonSID)
		if err != nil || !logonSID.IsValid() {
			return errors.Join(errors.New("command journal has an invalid logon SID"), err)
		}
	}
	if journal.Version >= 4 && journal.LogonSID != "" &&
		(!equalStringLists(journal.RestrictedSIDs, []string{journal.AccountSID, journal.LogonSID, "S-1-1-0"}) || !equalStringLists(journal.EnabledPrivileges, []string{"SeChangeNotifyPrivilege"})) {
		return errors.New("command journal has an invalid restricted token baseline")
	}
	for _, grant := range journal.ACLs {
		if !filepath.IsAbs(grant.Path) || grant.VolumeSerial == 0 || grant.FileIndex == 0 {
			return errors.New("command journal contains an invalid filesystem grant")
		}
	}
	return nil
}

func decodeNativeCommandJournal(data []byte) (nativeCommandJournal, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var journal nativeCommandJournal
	if err := decoder.Decode(&journal); err != nil {
		return nativeCommandJournal{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nativeCommandJournal{}, errors.New("native command journal contains trailing JSON data")
		}
		return nativeCommandJournal{}, err
	}
	return journal, nil
}

// rejectConflictingNativeJournalGrants prevents a new command from reusing a
// filesystem scope while a prior command's logon SID still has unrevoked ACLs.
// It is called while the per-user setup mutex is held, so journal and command
// lease creation cannot race the scan. Active non-cleaning commands are left
// alone; their own workspace leases and logon SID define the live scope.
func rejectConflictingNativeJournalGrants(installDir string, manifest nativeManifest, roots []string) error {
	if len(roots) == 0 {
		return nil
	}
	dir := filepath.Join(installDir, "journals")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list native sandbox journals before command start: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "command-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect native sandbox journal %q before command start: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxNativeJournalSize {
			return fmt.Errorf("native sandbox journal %q is not a bounded regular file", entry.Name())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read native sandbox journal %q before command start: %w", entry.Name(), err)
		}
		journal, err := decodeNativeCommandJournal(data)
		if err != nil {
			return fmt.Errorf("decode native sandbox journal %q before command start: %w", entry.Name(), err)
		}
		if err := validateNativeCommandJournal(entry.Name(), journal, manifest); err != nil {
			return fmt.Errorf("native sandbox journal %q: %w", entry.Name(), err)
		}
		active, err := nativeCommandLeaseActive(manifest.OwnerSID, journal.CommandID)
		if err != nil {
			return fmt.Errorf("inspect native sandbox command lease %s: %w", journal.CommandID, err)
		}
		// The runner's logon SID is journaled before any ACL is changed, so a
		// journal without one cannot represent an issued filesystem grant.
		if root, grantPath, conflicts := nativeJournalBlocksScope(journal, active, roots); conflicts {
			return fmt.Errorf("command scope %s overlaps unfinished sandbox cleanup %s at %s; repair the sandbox or restart the Agent to recover its journal", root, journal.CommandID, grantPath)
		}
	}
	return nil
}

func recoverNativeJournalsLocked(installDir string) (RecoveryReport, error) {
	var report RecoveryReport
	manifest, err := readNativeManifest(installDir)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, fmt.Errorf("read native sandbox manifest before recovery: %w", err)
	}
	dir := filepath.Join(installDir, "journals")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, fmt.Errorf("list native sandbox journals: %w", err)
	}
	root, err := filepath.Abs(filepath.Clean(installDir))
	if err != nil {
		return report, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !strings.HasPrefix(entry.Name(), "command-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return report, fmt.Errorf("inspect native sandbox journal %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxNativeJournalSize {
			return report, fmt.Errorf("native sandbox journal %q is not a bounded regular file", entry.Name())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return report, err
		}
		journal, err := decodeNativeCommandJournal(data)
		if err != nil {
			return report, fmt.Errorf("decode native command journal %q: %w", entry.Name(), err)
		}
		if err := validateNativeCommandJournal(entry.Name(), journal, manifest); err != nil {
			return report, fmt.Errorf("native sandbox journal %q: %w", entry.Name(), err)
		}
		if journal.Version >= 3 {
			active, err := nativeCommandLeaseActive(manifest.OwnerSID, journal.CommandID)
			if err != nil {
				return report, fmt.Errorf("inspect native command lease %s: %w", journal.CommandID, err)
			}
			if active {
				report.UnresolvedGrants++
				continue
			}
		} else {
			ownerAlive, err := processIdentityAlive(uint32(journal.OwnerPID), journal.OwnerStart)
			if err != nil {
				return report, err
			}
			if ownerAlive {
				report.UnresolvedGrants++
				continue
			}
		}
		if journal.RunnerPID != 0 && journal.RunnerStart != 0 {
			runnerAlive, err := processIdentityAlive(journal.RunnerPID, journal.RunnerStart)
			if err != nil {
				return report, err
			}
			if runnerAlive {
				process, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, journal.RunnerPID)
				if err != nil {
					return report, fmt.Errorf("open orphaned sandbox runner %d: %w", journal.RunnerPID, err)
				}
				termErr := windows.TerminateProcess(process, 1)
				wait, waitErr := windows.WaitForSingleObject(process, 10_000)
				closeErr := windows.CloseHandle(process)
				if wait != windows.WAIT_OBJECT_0 {
					waitErr = errors.Join(waitErr, fmt.Errorf("runner %d did not exit after termination (wait=%d)", journal.RunnerPID, wait))
				}
				if err := errors.Join(termErr, waitErr, closeErr); err != nil {
					return report, fmt.Errorf("stop orphaned sandbox runner %d: %w", journal.RunnerPID, err)
				}
			}
		}
		var logonSID *windows.SID
		if journal.LogonSID != "" {
			logonSID, err = windows.StringToSid(journal.LogonSID)
			if err != nil {
				return report, fmt.Errorf("decode journal logon SID: %w", err)
			}
		}
		var cleanupErr error
		if logonSID != nil && journal.Stage != "cleaning" {
			journal.Stage = "cleaning"
			if err := writeNativeJournal(path, journal, manifest.OwnerSID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("persist recovery cleaning stage: %w", err))
			}
		}
		if logonSID != nil {
			if err := revokeNativeSIDTree(path, &journal, logonSID, manifest.OwnerSID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("revoke logon SID from recovered writable trees: %w", err))
			}
		}
		for i := len(journal.ACLs) - 1; logonSID != nil && i >= 0; i-- {
			grant := journal.ACLs[i]
			if grant.Revoked {
				continue
			}
			candidate, err := filepath.Abs(filepath.Clean(grant.Path))
			if err != nil || !filepath.IsAbs(grant.Path) || grant.VolumeSerial == 0 || grant.FileIndex == 0 {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("refuse recovery for invalid journaled filesystem identity: %s", grant.Path))
				continue
			}
			volume, index, identityErr := nativeFileIdentity(candidate)
			if errors.Is(identityErr, os.ErrNotExist) || errors.Is(identityErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(identityErr, windows.ERROR_PATH_NOT_FOUND) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("journaled ACL target moved or disappeared before its grant was verified revoked: %s", candidate))
				continue
			} else if identityErr != nil {
				cleanupErr = errors.Join(cleanupErr, identityErr)
				continue
			}
			if volume != grant.VolumeSerial || index != grant.FileIndex {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("refuse recovery because journaled path now identifies a different filesystem object: %s", candidate))
				continue
			}
			if err := setNativeAccess(candidate, logonSID, windows.REVOKE_ACCESS, grant.Write, grant.Deny, grant.VolumeSerial, grant.FileIndex); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("revoke stale logon SID from %s: %w", candidate, err))
				report.UnresolvedGrants++
				if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					report.PermissionDenied++
				} else {
					report.OtherFailures++
				}
				continue
			}
			journal.ACLs[i].Revoked = true
			if err := writeNativeJournal(path, journal, manifest.OwnerSID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("persist recovered ACL revocation for %s: %w", candidate, err))
			}
		}
		if cleanupErr == nil && logonSID != nil {
			if err := verifyNativeSIDAbsentRoots(journal.ACLs, logonSID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("verify recovered logon SID removal from writable trees: %w", err))
			}
		}
		if journal.ScratchDir != "" {
			scratch, err := filepath.Abs(filepath.Clean(journal.ScratchDir))
			if err != nil || !pathWithin(filepath.Join(root, "sessions"), scratch) || journal.ScratchVolume == 0 || journal.ScratchIndex == 0 {
				cleanupErr = errors.Join(cleanupErr, errors.New("refuse recovery of scratch without a trusted session path identity"))
			} else if volume, index, identityErr := nativeFileIdentity(scratch); errors.Is(identityErr, os.ErrNotExist) || errors.Is(identityErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(identityErr, windows.ERROR_PATH_NOT_FOUND) {
				// A stopped command may already have removed its private scratch.
			} else if identityErr != nil {
				cleanupErr = errors.Join(cleanupErr, identityErr)
			} else if volume != journal.ScratchVolume || index != journal.ScratchIndex {
				cleanupErr = errors.Join(cleanupErr, errors.New("refuse scratch recovery because the path now identifies a different filesystem object"))
			} else if err := os.RemoveAll(scratch); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove orphaned command scratch: %w", err))
			}
		}
		if cleanupErr == nil && journal.Stage != "finished" {
			journal.Stage = "finished"
			if err := writeNativeJournal(path, journal, manifest.OwnerSID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("persist recovered finished stage: %w", err))
			}
		}
		if cleanupErr != nil {
			return report, fmt.Errorf("recover native sandbox journal %q: %w", entry.Name(), cleanupErr)
		}
		if _, err := os.Lstat(journal.ScratchDir); journal.ScratchDir != "" && !errors.Is(err, os.ErrNotExist) {
			return report, fmt.Errorf("scratch directory remains after recovery: %s", journal.ScratchDir)
		}
		if err := os.Remove(path); err != nil {
			return report, fmt.Errorf("remove recovered native command journal: %w", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return report, fmt.Errorf("native command journal remains after recovery: %s", path)
		}
	}
	_ = manifest
	return report, nil
}

func drainNativeJournalsLocked(installDir string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		report, err := recoverNativeJournalsLocked(installDir)
		if err != nil {
			return err
		}
		if report.UnresolvedGrants == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("native sandbox maintenance timed out with %d active or unresolved command journals", report.UnresolvedGrants)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func processIdentityAlive(pid uint32, expectedStart uint64) (alive bool, resultErr error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(process)) }()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return false, err
	}
	start := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if start != expectedStart {
		return false, nil
	}
	wait, err := windows.WaitForSingleObject(process, 0)
	if err != nil {
		return false, err
	}
	return wait == uint32(windows.WAIT_TIMEOUT), nil
}

func verifySIDAbsent(path string, sid *windows.SID) error {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read back recovered DACL %s: %w", path, err)
	}
	if descriptor == nil {
		return fmt.Errorf("recovered path %s has no DACL", path)
	}
	if strings.Contains(strings.ToUpper(descriptor.String()), strings.ToUpper(sid.String())) {
		return fmt.Errorf("logon SID remains in recovered DACL for %s", path)
	}
	return nil
}
