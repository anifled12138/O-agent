package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func setupProjectTestServer(t *testing.T) (*Server, *storage.Store) {
	t.Helper()
	tempDir := t.TempDir()
	st, err := storage.Open(tempDir)
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	user := domain.User{
		ID:          "ws_test",
		Email:       "test@axiom.local",
		DisplayName: "Workspace Owner",
		CreatedAt:   time.Now().UTC(),
	}
	if err := st.CreateUser(context.Background(), user, "pass"); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	srv := &Server{
		workspaceID: "ws_test",
		store:       st,
	}
	return srv, st
}

func TestProjectAPIAndMoveConversation(t *testing.T) {
	server, store := setupProjectTestServer(t)
	handler := server.Handler()

	// 1. Create a project
	createBody, _ := json.Marshal(map[string]any{
		"name":                "电商后端重构",
		"instructions":        "遵循 Go 微服务规范，所有接口需符合 RESTful 约束",
		"instructionsEnabled": true,
		"workdir":             "/workspace/ecommerce",
		"remoteRepoUrl":       "https://github.com/example/ecommerce.git",
		"remoteBranch":        "main",
	})
	req := httptest.NewRequest("POST", "/api/v1/projects", bytes.NewReader(createBody))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}
	var createdProj domain.Project
	if err := json.Unmarshal(w.Body.Bytes(), &createdProj); err != nil {
		t.Fatalf("failed to decode created project: %v", err)
	}
	if createdProj.Name != "电商后端重构" || createdProj.ID == "" || !createdProj.InstructionsEnabled || createdProj.RemoteRepoURL != "https://github.com/example/ecommerce.git" || createdProj.RemoteBranch != "main" {
		t.Fatalf("unexpected created project: %+v", createdProj)
	}

	// 2. List projects
	req = httptest.NewRequest("GET", "/api/v1/projects", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	var projects []domain.Project
	if err := json.Unmarshal(w.Body.Bytes(), &projects); err != nil {
		t.Fatalf("failed to decode projects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != createdProj.ID {
		t.Fatalf("expected 1 project in list, got %+v", projects)
	}

	// 3. Update project
	updateBody, _ := json.Marshal(map[string]string{
		"name":         "电商核心重构",
		"instructions": "更新的规范指令",
	})
	req = httptest.NewRequest("PATCH", "/api/v1/projects/"+createdProj.ID, bytes.NewReader(updateBody))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	var updatedProj domain.Project
	if err := json.Unmarshal(w.Body.Bytes(), &updatedProj); err != nil {
		t.Fatalf("failed to decode updated project: %v", err)
	}
	if updatedProj.Name != "电商核心重构" || updatedProj.Instructions != "更新的规范指令" {
		t.Fatalf("unexpected updated project: %+v", updatedProj)
	}

	// 4. Create conversation directly inside the store
	now := time.Now().UTC()
	prov := domain.Provider{
		ID:        "prov_mock",
		UserID:    "ws_test",
		Name:      "Mock Provider",
		Kind:      "openai-compatible",
		BaseURL:   "http://localhost/v1",
		Model:     "fake",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.UpsertProvider(context.Background(), prov, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatalf("failed to upsert provider: %v", err)
	}

	convo := domain.Conversation{
		ID:         "run_proj_test",
		UserID:     "ws_test",
		Title:      "订单模块重构设计",
		ProviderID: "prov_mock",
		ProjectID:  createdProj.ID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := store.CreateConversation(context.Background(), convo); err != nil {
		t.Fatalf("failed to create convo: %v", err)
	}

	// 5. Move conversation OUT of project (移出项目文件夹)
	moveOutBody, _ := json.Marshal(map[string]string{
		"projectId": "",
	})
	req = httptest.NewRequest("PATCH", "/api/v1/conversations/"+convo.ID, bytes.NewReader(moveOutBody))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK moving out, got %d: %s", w.Code, w.Body.String())
	}
	var detailOut domain.ConversationDetail
	_ = json.Unmarshal(w.Body.Bytes(), &detailOut)
	if detailOut.ProjectID != "" {
		t.Fatalf("expected convo projectId to be empty, got %q", detailOut.ProjectID)
	}

	// 6. Move conversation back INTO project (移入项目文件夹)
	moveInBody, _ := json.Marshal(map[string]string{
		"projectId": createdProj.ID,
	})
	req = httptest.NewRequest("PATCH", "/api/v1/conversations/"+convo.ID, bytes.NewReader(moveInBody))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK moving in, got %d: %s", w.Code, w.Body.String())
	}
	var detailIn domain.ConversationDetail
	_ = json.Unmarshal(w.Body.Bytes(), &detailIn)
	if detailIn.ProjectID != createdProj.ID {
		t.Fatalf("expected convo projectId %q, got %q", createdProj.ID, detailIn.ProjectID)
	}

	// 7. Delete project (conversations within it should be unlinked, not deleted)
	req = httptest.NewRequest("DELETE", "/api/v1/projects/"+createdProj.ID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK deleting project, got %d", w.Code)
	}

	// Check conversation after project deletion: its projectId should be reset to empty
	req = httptest.NewRequest("GET", "/api/v1/conversations/"+convo.ID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	var detailAfter domain.ConversationDetail
	_ = json.Unmarshal(w.Body.Bytes(), &detailAfter)
	if detailAfter.ProjectID != "" {
		t.Fatalf("expected convo projectId to be cleared after project delete, got %q", detailAfter.ProjectID)
	}
}
