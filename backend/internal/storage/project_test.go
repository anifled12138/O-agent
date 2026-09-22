package storage_test

import (
	"context"
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
	defer store.Close()

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
		ID:           "proj_1",
		UserID:       user.ID,
		Name:         "Project Alpha",
		Instructions: "Follow Go idioms",
		Workdir:      filepath.Join(dir, "alpha"),
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := store.CreateProject(ctx, p1); err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}

	projects, err := store.ListProjects(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListProjects failed: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "Project Alpha" {
		t.Fatalf("unexpected projects list: %+v", projects)
	}

	got, err := store.Project(ctx, user.ID, "proj_1")
	if err != nil {
		t.Fatalf("Project get failed: %v", err)
	}
	if got.Instructions != "Follow Go idioms" {
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
