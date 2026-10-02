package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestProjectStorageLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	user := domain.User{
		ID:          "usr_test",
		Email:       "test@example.com",
		DisplayName: "Test User",
		CreatedAt:   time.Now().UTC(),
	}
	if err := store.CreateUser(ctx, user, "secret"); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	p1 := domain.Project{
		ID:                 "proj_1",
		UserID:             user.ID,
		Name:               "Project Alpha",
		Instructions:       "Follow Go idioms",
		Workdir:            filepath.Join(dir, "alpha"),
		RemoteRepoURL:      "https://github.com/example/alpha.git",
		RemoteBranch:       "develop",
		RepositoryProvider: "github",
		ResolvedCommit:     "0123456789012345678901234567890123456789",
		MeasuredBytes:      1234,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	}
	if err := store.CreateProject(ctx, p1); err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}

	projects, err := store.ListProjects(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListProjects failed: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "Project Alpha" || projects[0].RepositoryProvider != "github" || projects[0].ResolvedCommit != p1.ResolvedCommit || projects[0].MeasuredBytes != 1234 {
		t.Fatalf("unexpected projects list: %+v", projects)
	}

	got, err := store.Project(ctx, user.ID, "proj_1")
	if err != nil {
		t.Fatalf("Project get failed: %v", err)
	}
	if got.Instructions != "Follow Go idioms" || got.RemoteRepoURL != p1.RemoteRepoURL || got.RepositoryProvider != "github" || got.ResolvedCommit != p1.ResolvedCommit || got.MeasuredBytes != 1234 {
		t.Fatalf("unexpected instructions: %q", got.Instructions)
	}

	got.Name = "Project Alpha Renamed"
	got.Instructions = "Updated instructions"
	if err := store.UpdateProject(ctx, user.ID, got); err != nil {
		t.Fatalf("UpdateProject failed: %v", err)
	}

	updated, err := store.Project(ctx, user.ID, "proj_1")
	if err != nil {
		t.Fatalf("Project get after update failed: %v", err)
	}
	if updated.Name != "Project Alpha Renamed" || updated.Instructions != "Updated instructions" {
		t.Fatalf("unexpected updated project: %+v", updated)
	}

	if err := store.DeleteProject(ctx, user.ID, "proj_1"); err != nil {
		t.Fatalf("DeleteProject failed: %v", err)
	}

	projectsAfter, err := store.ListProjects(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListProjects after delete failed: %v", err)
	}
	if len(projectsAfter) != 0 {
		t.Fatalf("expected 0 projects, got %d", len(projectsAfter))
	}
}

func TestProjectRepositoryIdentitySurvivesRestartAndUpdateRollsBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	user := domain.User{ID: "usr_project_restart", Email: "project-restart@example.com", DisplayName: "Project", CreatedAt: time.Now().UTC()}
	if err := store.CreateUser(ctx, user, "secret"); err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "proj_restart", UserID: user.ID, Name: "Repo", RemoteRepoURL: "https://gitee.com/acme/repo.git", RemoteBranch: "trunk", RepositoryProvider: "gitee", ResolvedCommit: "abcdefabcdefabcdefabcdefabcdefabcdefabcd", MeasuredBytes: 9876, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	readBack, err := store.Project(ctx, user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if readBack.RepositoryProvider != project.RepositoryProvider || readBack.ResolvedCommit != project.ResolvedCommit || readBack.MeasuredBytes != project.MeasuredBytes {
		t.Fatalf("repository identity did not survive restart: %+v", readBack)
	}

	dbPath := filepath.Join(dir, "axiom.db")
	probe, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.ExecContext(ctx, `CREATE TRIGGER reject_project_update BEFORE UPDATE ON projects BEGIN SELECT RAISE(ABORT,'forced project update failure'); END`); err != nil {
		_ = probe.Close()
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	readBack.Name = "must roll back"
	if err := store.UpdateProject(ctx, user.ID, readBack); err == nil {
		t.Fatal("UpdateProject succeeded despite the injected SQLite failure")
	}
	afterFailure, err := store.Project(ctx, user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.Name != project.Name || afterFailure.ResolvedCommit != project.ResolvedCommit {
		t.Fatalf("failed update changed durable project state: %+v", afterFailure)
	}
}

func TestProjectPublicationIsIdempotentDurableAndRollsBackWithProjectBaseline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	user := domain.User{ID: "usr_pub", Email: "pub@example.com", DisplayName: "Publication", CreatedAt: time.Now().UTC()}
	if err := store.CreateUser(ctx, user, "secret"); err != nil {
		t.Fatal(err)
	}
	base := "0123456789012345678901234567890123456789"
	commit := "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	project := domain.Project{ID: "proj_pub", UserID: user.ID, Name: "Repo", Workdir: t.TempDir(), RemoteRepoURL: "https://github.com/acme/repo.git", RemoteBranch: "main", ResolvedCommit: base, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	childCommit := "fedcbafedcbafedcbafedcbafedcbafedcbafedc"
	childCommitTwo := "fedcbafedcbafedcbafedcbafedcbafedcbafed1"
	publication := domain.ProjectPublication{ID: "pub_1", UserID: user.ID, ProjectID: project.ID, IdempotencyKey: "request-1", TargetBranch: "main", ExpectedRemoteSHA: base, CommitSHA: commit, Submodules: []domain.ProjectPublicationSubmodule{{Path: "vendor/lib", RepositoryURL: "https://github.com/acme/lib.git", CommitSHA: childCommit, Ref: "refs/heads/o-agent-objects/" + childCommit, Status: "pending"}, {Path: "vendor/second", RepositoryURL: "https://github.com/acme/second.git", CommitSHA: childCommitTwo, Ref: "refs/heads/o-agent-objects/" + childCommitTwo, Status: "pending"}}}
	first, created, err := store.BeginProjectPublication(ctx, publication)
	if err != nil || !created || first.Status != "publishing" {
		t.Fatalf("begin publication = %+v, created=%t, err=%v", first, created, err)
	}
	replay, created, err := store.BeginProjectPublication(ctx, publication)
	if err != nil || created || replay.ID != first.ID {
		t.Fatalf("idempotent replay = %+v, created=%t, err=%v", replay, created, err)
	}
	changed := publication
	changed.CommitSHA = base
	if _, _, err := store.BeginProjectPublication(ctx, changed); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflicting idempotency reuse error = %v", err)
	}
	parallel := publication
	parallel.ID = "pub_2"
	parallel.IdempotencyKey = "request-2"
	if _, _, err := store.BeginProjectPublication(ctx, parallel); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("parallel publication to active branch error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	readBack, err := store.ProjectPublication(ctx, user.ID, project.ID, first.ID)
	if err != nil || readBack.Status != "publishing" || readBack.ExpectedRemoteSHA != base || len(readBack.Submodules) != 2 || readBack.Submodules[0].CommitSHA != childCommit || readBack.Submodules[0].Status != "pending" || readBack.Submodules[1].CommitSHA != childCommitTwo || readBack.Submodules[1].Status != "pending" {
		t.Fatalf("publication did not survive restart: %+v, %v", readBack, err)
	}
	if _, err := store.CompleteProjectPublication(ctx, user.ID, project.ID, first.ID, commit); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("publication completed before child ref read-back: %v", err)
	}
	readBack, err = store.SetProjectPublicationSubmoduleState(ctx, user.ID, project.ID, first.ID, "vendor/lib", "published", childCommit, "")
	if err != nil || readBack.Submodules[0].Status != "published" || readBack.Submodules[0].RemoteSHA != childCommit {
		t.Fatalf("child ref evidence did not durably read back: %+v, %v", readBack, err)
	}
	if _, err := store.CompleteProjectPublication(ctx, user.ID, project.ID, first.ID, commit); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("publication completed before every child ref read-back: %v", err)
	}
	readBack, err = store.SetProjectPublicationSubmoduleState(ctx, user.ID, project.ID, first.ID, "vendor/second", "published", childCommitTwo, "")
	if err != nil || readBack.Submodules[1].Status != "published" || readBack.Submodules[1].RemoteSHA != childCommitTwo {
		t.Fatalf("second child ref evidence did not durably read back: %+v, %v", readBack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatalf("restart after child ref read-back: %v", err)
	}
	readBack, err = store.ProjectPublication(ctx, user.ID, project.ID, first.ID)
	if err != nil || len(readBack.Submodules) != 2 || readBack.Submodules[0].Status != "published" || readBack.Submodules[0].RemoteSHA != childCommit || readBack.Submodules[1].Status != "published" || readBack.Submodules[1].RemoteSHA != childCommitTwo {
		t.Fatalf("published child ref state did not survive restart: %+v, %v", readBack, err)
	}
	replay, created, err = store.BeginProjectPublication(ctx, publication)
	if err != nil || created || replay.ID != first.ID || replay.Submodules[0].Status != "published" || replay.Submodules[1].Status != "published" {
		t.Fatalf("idempotent retry after child ref progress = %+v created=%t err=%v", replay, created, err)
	}

	dbPath := filepath.Join(dir, "axiom.db")
	probe, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.ExecContext(ctx, `CREATE TRIGGER reject_publication_project_update BEFORE UPDATE ON projects BEGIN SELECT RAISE(ABORT,'forced project update failure'); END`); err != nil {
		_ = probe.Close()
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteProjectPublication(ctx, user.ID, project.ID, first.ID, commit); err == nil {
		t.Fatal("completion succeeded despite injected project update failure")
	}
	publicationAfterFailure, err := store.ProjectPublication(ctx, user.ID, project.ID, first.ID)
	if err != nil || publicationAfterFailure.Status != "publishing" {
		t.Fatalf("publication completion did not roll back atomically: %+v, %v", publicationAfterFailure, err)
	}
	projectAfterFailure, err := store.Project(ctx, user.ID, project.ID)
	if err != nil || projectAfterFailure.ResolvedCommit != base {
		t.Fatalf("project baseline changed after rollback: %+v, %v", projectAfterFailure, err)
	}
	probe, err = sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.ExecContext(ctx, `DROP TRIGGER reject_publication_project_update`); err != nil {
		_ = probe.Close()
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteProjectPublication(ctx, user.ID, project.ID, first.ID, base); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("mismatched remote SHA error = %v", err)
	}
	completed, err := store.CompleteProjectPublication(ctx, user.ID, project.ID, first.ID, commit)
	if err != nil || completed.Status != "published" || completed.RemoteSHA != commit {
		t.Fatalf("complete publication = %+v, %v", completed, err)
	}
	projectAfterComplete, err := store.Project(ctx, user.ID, project.ID)
	if err != nil || projectAfterComplete.ResolvedCommit != commit {
		t.Fatalf("project baseline was not advanced with publication: %+v, %v", projectAfterComplete, err)
	}
}

func TestProjectPublicationSubmodulePlanMigratesFromLegacySchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	user := domain.User{ID: "usr_pub_migrate", Email: "pub-migrate@example.com", DisplayName: "Publication Migration", CreatedAt: time.Now().UTC()}
	if err := store.CreateUser(ctx, user, "secret"); err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "proj_pub_migrate", UserID: user.ID, Name: "Repo", Workdir: t.TempDir(), RemoteRepoURL: "https://github.com/acme/repo.git", RemoteBranch: "main", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	probe, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "axiom.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.ExecContext(ctx, `DROP TABLE project_publications;
CREATE TABLE project_publications (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 idempotency_key TEXT NOT NULL, target_branch TEXT NOT NULL,
 expected_remote_sha TEXT NOT NULL DEFAULT '', commit_sha TEXT NOT NULL,
 remote_sha TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
 error_text TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
 UNIQUE(user_id,idempotency_key)
);
CREATE INDEX idx_project_publications_project ON project_publications(user_id,project_id,created_at DESC);
CREATE UNIQUE INDEX idx_project_publications_active_branch ON project_publications(user_id,project_id,target_branch) WHERE status IN ('publishing','needs_reconciliation');
INSERT INTO project_publications(id,user_id,project_id,idempotency_key,target_branch,expected_remote_sha,commit_sha,status,created_at,updated_at) VALUES('legacy_pub',?,?, 'legacy-key','main','',?,'failed',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);`, user.ID, project.ID, "abcdefabcdefabcdefabcdefabcdefabcdefabcd"); err != nil {
		_ = probe.Close()
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatalf("open and migrate legacy publication schema: %v", err)
	}
	defer store.Close()
	legacy, err := store.ProjectPublication(ctx, user.ID, project.ID, "legacy_pub")
	if err != nil || len(legacy.Submodules) != 0 {
		t.Fatalf("legacy publication did not read after migration: %+v, %v", legacy, err)
	}
	childSHA := "0123456789012345678901234567890123456789"
	created, inserted, err := store.BeginProjectPublication(ctx, domain.ProjectPublication{ID: "new_pub", UserID: user.ID, ProjectID: project.ID, IdempotencyKey: "new-key", TargetBranch: "main", CommitSHA: "1234567890123456789012345678901234567890", Submodules: []domain.ProjectPublicationSubmodule{{Path: "lib", RepositoryURL: "https://github.com/acme/lib.git", CommitSHA: childSHA, Ref: "refs/heads/o-agent-objects/" + childSHA, Status: "pending"}}})
	if err != nil || !inserted || len(created.Submodules) != 1 || created.Submodules[0].Status != "pending" {
		t.Fatalf("new plan did not persist after migration: %+v inserted=%t err=%v", created, inserted, err)
	}
}

func TestEmptyConversationAndProviderRemainReadableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	user := domain.User{ID: "usr_restart", Email: "restart@example.com", DisplayName: "Restart", CreatedAt: time.Now().UTC()}
	if err := store.CreateUser(ctx, user, "secret"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	provider := domain.Provider{ID: "provider_restart", UserID: user.ID, Name: "Provider", Kind: "openai-compatible", BaseURL: "http://127.0.0.1:8000/v1", Model: "test", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "conversation_restart", UserID: user.ID, Title: "新对话", ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.ProviderSecret(ctx, user.ID, provider.ID); err != nil {
		t.Fatalf("provider secret unreadable: %v", err)
	}
	if _, err := store.Conversation(ctx, user.ID, conversation.ID); err != nil {
		t.Fatalf("conversation unreadable: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Conversation(ctx, user.ID, conversation.ID); err != nil {
		t.Fatalf("empty conversation was lost after restart: %v", err)
	}
	conversations, err := reopened.ListConversations(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID != conversation.ID {
		t.Fatalf("empty conversation missing from list after restart: %+v", conversations)
	}
}
