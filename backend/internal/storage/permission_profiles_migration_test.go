package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestPermissionProfileMigrationPreservesExistingChoiceAndRunsOnce(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migration.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE runtime_settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE conversations(id TEXT PRIMARY KEY,permission_profile TEXT NOT NULL);
CREATE TABLE agent_turns(id TEXT PRIMARY KEY,permission_profile TEXT NOT NULL);
CREATE TABLE agent_approvals(id TEXT PRIMARY KEY,permission_profile TEXT NOT NULL);
INSERT INTO conversations(id,permission_profile) VALUES('existing_workspace','workspace_autonomous'),('legacy_sensitive','ask_on_sensitive');
INSERT INTO agent_turns(id,permission_profile) VALUES('old_turn','workspace_autonomous'),('legacy_turn','ask_on_sensitive');
INSERT INTO agent_approvals(id,permission_profile) VALUES('old_approval','workspace_autonomous'),('legacy_approval','ask_on_sensitive');`); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if err := store.migrateConversationPermissionProfiles(ctx); err != nil {
		t.Fatalf("run migration: %v", err)
	}
	assertProfile := func(id, want string) {
		t.Helper()
		var got string
		if err := db.QueryRow(`SELECT permission_profile FROM conversations WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("conversation %s profile=%q, want %q", id, got, want)
		}
	}
	assertProfile("existing_workspace", "request_approval")
	assertProfile("legacy_sensitive", "workspace_autonomous")
	for _, item := range []struct{ table, id, want string }{
		{table: "agent_turns", id: "old_turn", want: "request_approval"},
		{table: "agent_turns", id: "legacy_turn", want: "workspace_autonomous"},
		{table: "agent_approvals", id: "old_approval", want: "request_approval"},
		{table: "agent_approvals", id: "legacy_approval", want: "workspace_autonomous"},
	} {
		var got string
		if err := db.QueryRow(`SELECT permission_profile FROM `+item.table+` WHERE id=?`, item.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != item.want {
			t.Fatalf("%s %s profile=%q, want %q", item.table, item.id, got, item.want)
		}
	}

	// A user can choose workspace-auto after migration; startup must not
	// reinterpret the newly selected value as a legacy profile.
	if _, err := db.Exec(`UPDATE conversations SET permission_profile='workspace_autonomous' WHERE id='existing_workspace'`); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateConversationPermissionProfiles(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	assertProfile("existing_workspace", "workspace_autonomous")

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := (&Store{db: reopened}).migrateConversationPermissionProfiles(ctx); err != nil {
		t.Fatalf("migration after restart: %v", err)
	}
	var persisted string
	if err := reopened.QueryRow(`SELECT permission_profile FROM conversations WHERE id='existing_workspace'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != "workspace_autonomous" {
		t.Fatalf("selected profile after restart=%q, want workspace_autonomous", persisted)
	}
}

func TestPermissionProfileMigrationRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "rollback.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE runtime_settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE conversations(id TEXT PRIMARY KEY,permission_profile TEXT NOT NULL);
CREATE TABLE agent_turns(id TEXT PRIMARY KEY,permission_profile TEXT NOT NULL);
CREATE TABLE agent_approvals(id TEXT PRIMARY KEY,permission_profile TEXT NOT NULL);
INSERT INTO conversations(id,permission_profile) VALUES('existing_workspace','workspace_autonomous');
INSERT INTO agent_turns(id,permission_profile) VALUES('old_turn','workspace_autonomous');
INSERT INTO agent_approvals(id,permission_profile) VALUES('old_approval','workspace_autonomous');
CREATE TRIGGER reject_migration_marker BEFORE INSERT ON runtime_settings
BEGIN SELECT RAISE(ABORT, 'simulated marker write failure'); END;`); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: db}
	if err := store.migrateConversationPermissionProfiles(ctx); err == nil {
		t.Fatal("migration succeeded despite marker write failure")
	}
	var profile string
	if err := db.QueryRow(`SELECT permission_profile FROM conversations WHERE id='existing_workspace'`).Scan(&profile); err != nil {
		t.Fatal(err)
	}
	if profile != "workspace_autonomous" {
		t.Fatalf("failed migration changed conversation profile to %q instead of rolling back", profile)
	}
	for _, item := range []struct{ table, id string }{{"agent_turns", "old_turn"}, {"agent_approvals", "old_approval"}} {
		if err := db.QueryRow(`SELECT permission_profile FROM `+item.table+` WHERE id=?`, item.id).Scan(&profile); err != nil {
			t.Fatal(err)
		}
		if profile != "workspace_autonomous" {
			t.Fatalf("failed migration changed %s profile to %q instead of rolling back", item.table, profile)
		}
	}
	var markers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runtime_settings WHERE key='migration.permission_profiles.request_approval.v1'`).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 {
		t.Fatalf("failed migration left %d markers", markers)
	}
}
