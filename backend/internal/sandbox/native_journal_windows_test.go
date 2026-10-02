//go:build windows

package sandbox

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNativeJournalGrantConflictUsesPathBoundariesAndRevocationState(t *testing.T) {
	grant := nativeACLRecord{Path: `C:\work\repo`, VolumeSerial: 1, FileIndex: 2}
	tests := []struct {
		name      string
		record    nativeACLRecord
		root      string
		conflicts bool
	}{
		{name: "same root", record: grant, root: `C:\work\repo`, conflicts: true},
		{name: "requested child", record: grant, root: `C:\work\repo\src`, conflicts: true},
		{name: "grant child", record: nativeACLRecord{Path: `C:\work\repo\.git\objects`, VolumeSerial: 1, FileIndex: 3}, root: `C:\work\repo`, conflicts: true},
		{name: "sibling sharing textual prefix", record: grant, root: `C:\work\repo-copy`, conflicts: false},
		{name: "revoked grant", record: nativeACLRecord{Path: grant.Path, Revoked: true}, root: `C:\work\repo`, conflicts: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, path, got := nativeJournalGrantConflicts([]nativeACLRecord{test.record}, []string{test.root})
			if got != test.conflicts {
				t.Fatalf("nativeJournalGrantConflicts() = %v, want %v (root=%q grant=%q)", got, test.conflicts, root, path)
			}
			if got && (root != test.root || path != test.record.Path) {
				t.Fatalf("conflict paths = (%q, %q), want (%q, %q)", root, path, test.root, test.record.Path)
			}
		})
	}
}

func TestNativeJournalBlocksOnlyUnfinishedConflictingScopes(t *testing.T) {
	grant := nativeACLRecord{Path: `C:\work\repo`, VolumeSerial: 1, FileIndex: 2}
	tests := []struct {
		name    string
		journal nativeCommandJournal
		active  bool
		root    string
		blocks  bool
	}{
		{name: "active running command may share read scope", journal: nativeCommandJournal{Stage: "running", LogonSID: "S-1-5-5-1-2", ACLs: []nativeACLRecord{grant}}, active: true, root: `C:\work\repo`, blocks: false},
		{name: "inactive command with a grant blocks overlapping scope", journal: nativeCommandJournal{Stage: "running", LogonSID: "S-1-5-5-1-2", ACLs: []nativeACLRecord{grant}}, active: false, root: `C:\work\repo`, blocks: true},
		{name: "cleaning command blocks until cleanup ends", journal: nativeCommandJournal{Stage: "cleaning", LogonSID: "S-1-5-5-1-2", ACLs: []nativeACLRecord{grant}}, active: true, root: `C:\work\repo`, blocks: true},
		{name: "pre-auth runner did not issue a grant", journal: nativeCommandJournal{Stage: "running", ACLs: []nativeACLRecord{grant}}, active: false, root: `C:\work\repo`, blocks: false},
		{name: "unrelated stale scope does not block", journal: nativeCommandJournal{Stage: "running", LogonSID: "S-1-5-5-1-2", ACLs: []nativeACLRecord{grant}}, active: false, root: `C:\work\other`, blocks: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, got := nativeJournalBlocksScope(test.journal, test.active, []string{test.root})
			if got != test.blocks {
				t.Fatalf("nativeJournalBlocksScope() = %v, want %v", got, test.blocks)
			}
		})
	}
}

func TestRejectConflictingNativeJournalGrantsReadsDurableJournal(t *testing.T) {
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ownerSID := owner.User.Sid.String()
	installDir := t.TempDir()
	journalDir := filepath.Join(installDir, "journals")
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	commandID, err := makeCommandID()
	if err != nil {
		t.Fatal(err)
	}
	manifest := nativeManifest{
		OwnerSID: ownerSID,
		Offline:  nativeCredential{SID: "S-1-5-21-100-200-300-101"},
		Online:   nativeCredential{SID: "S-1-5-21-100-200-300-102"},
	}
	journal := nativeCommandJournal{
		Version: 4, CommandID: commandID, OwnerPID: os.Getpid(), OwnerStart: 1,
		AccountSID: manifest.Offline.SID, LogonSID: "S-1-5-5-100-200",
		RestrictedSIDs:    []string{manifest.Offline.SID, "S-1-5-5-100-200", "S-1-1-0"},
		EnabledPrivileges: []string{"SeChangeNotifyPrivilege"}, Stage: "running",
		ACLs: []nativeACLRecord{{Path: `C:\work\repo`, VolumeSerial: 1, FileIndex: 2}},
	}
	data, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(journalDir, "command-"+commandID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	mutex, release, err := acquireSetupLock(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(mutex); err != nil {
			t.Errorf("release setup lock: %v", err)
		}
	}()

	err = rejectConflictingNativeJournalGrants(installDir, manifest, []string{`C:\work\repo\src`})
	if err == nil || !strings.Contains(err.Error(), commandID) {
		t.Fatalf("overlapping durable grant journal was not rejected: %v", err)
	}
	if err := rejectConflictingNativeJournalGrants(installDir, manifest, []string{`C:\work\another-repo`}); err != nil {
		t.Fatalf("unrelated workspace was blocked by journal: %v", err)
	}
	journal.Stage = "unknown-stage"
	data, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectConflictingNativeJournalGrants(installDir, manifest, []string{`C:\work\another-repo`}); err == nil {
		t.Fatal("unknown durable journal lifecycle stage did not fail closed")
	}
	journal.Stage = "running"
	journal.RunnerPID = 1234
	data, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectConflictingNativeJournalGrants(installDir, manifest, []string{`C:\work\another-repo`}); err == nil {
		t.Fatal("incomplete runner process identity did not fail closed")
	}
	journal.RunnerPID = 0
	data, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	unknownField := []byte(strings.TrimSuffix(string(data), "}") + `,"futureCleanupSemantics":true}`)
	if err := os.WriteFile(path, unknownField, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectConflictingNativeJournalGrants(installDir, manifest, []string{`C:\work\another-repo`}); err == nil {
		t.Fatal("journal with unrecognized cleanup fields did not fail closed")
	}
	journal.LogonSID = ""
	journal.RestrictedSIDs = nil
	journal.EnabledPrivileges = nil
	data, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectConflictingNativeJournalGrants(installDir, manifest, []string{`C:\work\another-repo`}); err == nil {
		t.Fatal("malformed durable grant journal did not fail closed")
	}
}

func TestOversizedNativeJournalDoesNotReplacePreviousDurableRecord(t *testing.T) {
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	commandID, err := makeCommandID()
	if err != nil {
		t.Fatal(err)
	}
	journal := nativeCommandJournal{
		Version: 4, CommandID: commandID, OwnerPID: os.Getpid(), OwnerStart: 1,
		AccountSID: owner.User.Sid.String(), LogonSID: "S-1-5-5-100-200",
		RestrictedSIDs:    []string{owner.User.Sid.String(), "S-1-5-5-100-200", "S-1-1-0"},
		EnabledPrivileges: []string{"SeChangeNotifyPrivilege"}, Stage: "prepared", CreatedAt: "small",
	}
	path := filepath.Join(t.TempDir(), "journal.json")
	ownerSID := owner.User.Sid.String()
	if err := writeNativeJournal(path, journal, ownerSID); err != nil {
		t.Fatalf("write initial durable journal: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	journal.CreatedAt = strings.Repeat("x", maxNativeJournalSize)
	if err := writeNativeJournal(path, journal, ownerSID); err == nil {
		t.Fatal("oversized native journal write succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("oversized journal write replaced the previous durable recovery record")
	}
}
