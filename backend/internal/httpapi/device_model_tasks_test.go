package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestDeviceModelTaskPersistsSelectionAndRejectsStaleCatalog(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = store.UpsertProvider(ctx, domain.Provider{ID: "source-provider", UserID: owner, Name: "Cloud", Kind: "openai", BaseURL: "https://model.example", Model: "source-model", CreatedAt: now, UpdatedAt: now}, []byte{}, []byte{}); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateConversation(ctx, domain.Conversation{ID: "source", UserID: owner, Title: "Conversation", ProviderID: "source-provider", PermissionProfile: domain.PermissionProfileReadOnly, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "target", UserID: owner, Name: "PC", Platform: "windows"}, "node-hash"); err != nil {
		t.Fatal(err)
	}
	catalog := storage.NodeCatalog{Models: []storage.NodeModel{{ID: "local-model", Name: "Local", Model: "local"}}, PermissionProfiles: []domain.PermissionProfile{domain.PermissionProfileReadOnly}}
	if _, err = store.HeartbeatExecutionNodeWithCatalog(ctx, "node-hash", "windows", []string{"agent-runtime"}, storage.ExecutionNodeResources{MaxConcurrentTasks: 1}, &catalog, now); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactstore.New(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{workspaceID: owner, store: store, artifacts: artifacts}
	call := func(model, key string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]string{"targetNodeId": "target", "localProviderId": model, "content": "Use the chosen model"})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/api/v1/conversations/source/tasks/nodes", bytes.NewReader(body))
		req.SetPathValue("id", "source")
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		s.nodeAgentTaskSubmit(rec, req)
		return rec
	}
	bad := call("stale-model", "bad")
	if bad.Code != 409 {
		t.Fatal(bad.Code, bad.Body)
	}
	if _, err = store.ExecutionTaskByIdempotencyKey(ctx, owner, "bad"); err == nil {
		t.Fatal("stale model request created a task")
	}
	r := call("local-model", "chosen")
	if r.Code != http.StatusAccepted {
		t.Fatal(r.Code, r.Body)
	}
	task, err := store.ExecutionTaskByIdempotencyKey(ctx, owner, "chosen")
	if err != nil {
		t.Fatal(err)
	}
	var payload nodeAgentTaskPayload
	if err = json.Unmarshal(task.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.LocalProviderID != "local-model" || payload.PermissionProfile != domain.PermissionProfileReadOnly {
		t.Fatal("chosen execution settings were not durable", payload)
	}
	claimed, err := store.ClaimExecutionTask(ctx, "target", "lease", now.Add(time.Minute), now.Add(time.Second))
	if err != nil || string(claimed.Payload) != string(task.Payload) {
		t.Fatal("node runtime did not receive chosen model and permission", err)
	}
	if changed := call("stale-model", "chosen"); changed.Code != 409 {
		t.Fatal("idempotency changed model", changed.Code)
	}
	if again := call("local-model", "chosen"); again.Code != 200 {
		t.Fatal("retry failed after lease", again.Code, again.Body)
	}
}
