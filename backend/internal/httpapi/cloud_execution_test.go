package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/projectpolicy"
	"axiom.local/agent/internal/storage"
)

type testProjectDeltaSnapshotter struct {
	path          string
	lfsPath       string
	submodulePath string
	baseOverride  string
	calls         int
}

func (s *testProjectDeltaSnapshotter) CreateProjectDeltaBundle(ctx context.Context, project domain.Project, taskID string) (result agent.ProjectDeltaBundle, retErr error) {
	s.calls++
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = project.Workdir
		output, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(output)), err
	}
	commit, err := run("rev-parse", "HEAD")
	if err != nil {
		return agent.ProjectDeltaBundle{}, err
	}
	ref := "refs/o-agent/task-deltas/" + taskID
	if _, err := run("update-ref", ref, commit); err != nil {
		return agent.ProjectDeltaBundle{}, err
	}
	defer func() {
		_, cleanupErr := run("update-ref", "-d", ref)
		retErr = errors.Join(retErr, cleanupErr)
	}()
	path := filepath.Join(s.path, taskID+".bundle")
	if _, err := run("bundle", "create", path, ref, "^"+project.ResolvedCommit); err != nil {
		return agent.ProjectDeltaBundle{}, err
	}
	if _, err := run("bundle", "verify", path); err != nil {
		return agent.ProjectDeltaBundle{}, err
	}
	base := project.ResolvedCommit
	if s.baseOverride != "" {
		base = s.baseOverride
	}
	return agent.ProjectDeltaBundle{Path: path, LFSObjectsPath: s.lfsPath, SubmoduleDeltasPath: s.submodulePath, BaseCommit: base, Commit: commit}, nil
}

func TestCloudAgentTaskSubmitPersistsAndIdempotentlyReadsBack(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.UpsertProvider(ctx, domain.Provider{ID: "provider_test", UserID: owner, Name: "Test", Kind: "openai", BaseURL: "https://example.invalid", Model: "test", ContextWindow: 8192, CreatedAt: now, UpdatedAt: now}, []byte{}, []byte{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConversation(ctx, domain.Conversation{ID: "conversation_cloud_test", UserID: owner, Title: "Cloud task", ProviderID: "provider_test", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnsureCloudExecutionNode(ctx, owner, now); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("cloud task input")
	digest := sha256.Sum256(content)
	inputArtifact, err := artifacts.StoreFromReader(ctx, owner, "input.txt", "text/plain", "cloud-task-user-input", int64(len(content)), hex.EncodeToString(digest[:]), bytes.NewReader(content), now)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store, artifacts: artifacts}
	call := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"content": "inspect the project", "artifactIds": []string{inputArtifact.ID}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/conversation_cloud_test/tasks/cloud", bytes.NewReader(body))
		req.SetPathValue("id", "conversation_cloud_test")
		req.Header.Set("Idempotency-Key", "cloud-task-test-key")
		response := httptest.NewRecorder()
		server.cloudAgentTaskSubmit(response, req)
		return response
	}
	first := call()
	if first.Code != http.StatusAccepted {
		t.Fatalf("first submit = %d: %s", first.Code, first.Body.String())
	}
	var firstBody struct {
		Task    storage.ExecutionTask `json:"task"`
		Created bool                  `json:"created"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	if !firstBody.Created || firstBody.Task.Status != "queued" || firstBody.Task.NodeID != storage.CloudExecutionNodeID(owner) {
		t.Fatalf("unexpected first submission: %+v", firstBody)
	}
	var payload struct {
		Kind           string              `json:"kind"`
		ConversationID string              `json:"conversationId"`
		Content        string              `json:"content"`
		Attachments    []taskInputArtifact `json:"attachments"`
	}
	if err := json.Unmarshal(firstBody.Task.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Kind != "agent_turn" || payload.ConversationID != "conversation_cloud_test" || payload.Content != "inspect the project" || len(payload.Attachments) != 1 || payload.Attachments[0] != (taskInputArtifact{ArtifactID: inputArtifact.ID, SHA256: inputArtifact.SHA256, ByteSize: inputArtifact.ByteSize, FileName: inputArtifact.FileName, MediaType: inputArtifact.MediaType}) {
		t.Fatalf("persisted task payload mismatch: %+v", payload)
	}
	attached, err := store.ExecutionTaskArtifacts(ctx, owner, firstBody.Task.ID)
	if err != nil || len(attached) != 1 || attached[0].ID != inputArtifact.ID || attached[0].Role != "user_input" || attached[0].SHA256 != inputArtifact.SHA256 || attached[0].ByteSize != inputArtifact.ByteSize {
		t.Fatalf("cloud task input link did not read back: %+v err=%v", attached, err)
	}
	second := call()
	if second.Code != http.StatusOK {
		t.Fatalf("idempotent submit = %d: %s", second.Code, second.Body.String())
	}
	var secondBody struct {
		Task    storage.ExecutionTask `json:"task"`
		Created bool                  `json:"created"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatal(err)
	}
	if secondBody.Created || secondBody.Task.ID != firstBody.Task.ID {
		t.Fatalf("idempotent retry did not return the original durable task: first=%+v second=%+v", firstBody, secondBody)
	}
	readBack, err := store.ExecutionTask(ctx, owner, firstBody.Task.ID)
	if err != nil || readBack.Status != "queued" || readBack.NodeID != storage.CloudExecutionNodeID(owner) {
		t.Fatalf("task missing from authoritative storage: task=%+v err=%v", readBack, err)
	}
}

func TestProjectDeltaImportRejectsUnverifiedTaskResult(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	node, _, err := store.EnsureCloudExecutionNode(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	const taskID = "task_delta_unverified"
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: taskID, UserID: owner, NodeID: node.ID, IdempotencyKey: "delta-unverified", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	leaseToken := "delta-unverified-lease"
	leaseHash := hashToken(leaseToken)
	if _, err := store.ClaimExecutionTask(ctx, node.ID, leaseHash, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, taskID, leaseHash, "accepted", nil, "", now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, taskID, leaseHash, "running", nil, "", now.Add(200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, taskID, leaseHash, "reported_succeeded", json.RawMessage(`{"agentStatus":"failed"}`), "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+taskID+"/import-project-delta", bytes.NewReader([]byte(`{}`)))
	request.SetPathValue("id", taskID)
	response := httptest.NewRecorder()
	server.executionTaskImportProjectDelta(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("unverified result import = %d: %s", response.Code, response.Body.String())
	}
	readBack, err := store.ExecutionTask(ctx, owner, taskID)
	if err != nil || readBack.Status != "reported_succeeded" || string(readBack.Result) != `{"agentStatus":"failed"}` {
		t.Fatalf("rejected import changed the authoritative task result: task=%+v err=%v", readBack, err)
	}
}

func TestTaskReadModelFindsDurableImportedProject(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const taskID = "task_import_readback"
	const base = "0123456789012345678901234567890123456789"
	const commit = "1123456789012345678901234567890123456789"
	source := domain.Project{ID: "project_import_source", UserID: owner, Name: "Source", Workdir: `C:\cloud\source`, RemoteRepoURL: "https://github.com/example/repo.git", RemoteBranch: "main", RepositoryProvider: "github", ResolvedCommit: base, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, source); err != nil {
		t.Fatal(err)
	}
	imported := domain.Project{ID: agent.ProjectDeltaProjectID(source.ID, taskID), UserID: owner, Name: "Source · task " + taskID, Workdir: `C:\cloud\imported`, RemoteRepoURL: source.RemoteRepoURL, RemoteBranch: "codex/task-" + taskID, RepositoryProvider: "github", ResolvedCommit: commit, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, imported); err != nil {
		t.Fatal(err)
	}
	task := storage.ExecutionTask{ID: taskID, UserID: owner, Status: "reported_succeeded", Result: json.RawMessage(`{"projectId":"project_import_source","projectDeltaCommit":"` + commit + `","projectDeltaArtifact":{"id":"art_delta"}}`)}
	server := &Server{workspaceID: owner, store: store}
	if err := server.attachImportedProject(httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil), &task); err != nil {
		t.Fatal(err)
	}
	if task.ImportedProject == nil || task.ImportedProject.ID != imported.ID || task.ImportedProject.ResolvedCommit != commit {
		t.Fatalf("task read model did not expose its durable imported project: %+v", task.ImportedProject)
	}
	imported.RemoteBranch = "main"
	if err := store.UpdateProject(ctx, owner, imported); err != nil {
		t.Fatal(err)
	}
	task.ImportedProject = nil
	if err := server.attachImportedProject(httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil), &task); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("mismatched durable branch error = %v, want conflict", err)
	}
}

func TestTaskReadModelFindsCloudExecutionWorktree(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const taskID = "task_cloud_worktree_readback"
	const base = "0123456789012345678901234567890123456789"
	source := domain.Project{ID: "project_cloud_worktree_source", UserID: owner, Name: "Cloud source", Workdir: `C:\cloud\source`, RemoteRepoURL: "https://github.com/example/cloud.git", RemoteBranch: "main", RepositoryProvider: "github", ResolvedCommit: base, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, source); err != nil {
		t.Fatal(err)
	}
	provider := domain.Provider{ID: "provider_cloud_worktree_readback", UserID: owner, Name: "Provider", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "conversation_cloud_worktree_readback", UserID: owner, Title: "Cloud task", ProviderID: provider.ID, ProjectID: source.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	workspace := domain.Project{ID: agent.ProjectDeltaProjectID(source.ID, taskID), UserID: owner, Name: "Cloud task worktree", Workdir: `C:\cloud\tasks\task-cloud`, RemoteRepoURL: source.RemoteRepoURL, RemoteBranch: "codex/task-" + taskID, RepositoryProvider: "github", ResolvedCommit: base, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]string{"conversationId": conversation.ID, "executionProjectId": workspace.ID})
	if err != nil {
		t.Fatal(err)
	}
	task := storage.ExecutionTask{ID: taskID, UserID: owner, Status: "completed", Result: result}
	server := &Server{workspaceID: owner, store: store}
	if err := server.attachImportedProject(httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil), &task); err != nil {
		t.Fatal(err)
	}
	if task.ImportedProject == nil || task.ImportedProject.ID != workspace.ID || task.ImportedProject.Workdir != workspace.Workdir {
		t.Fatalf("cloud task read model did not expose its durable execution worktree: %+v", task.ImportedProject)
	}
	workspace.RemoteBranch = "main"
	if err := store.UpdateProject(ctx, owner, workspace); err != nil {
		t.Fatal(err)
	}
	task.ImportedProject = nil
	if err := server.attachImportedProject(httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil), &task); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("mismatched cloud worktree branch error = %v, want conflict", err)
	}
}

func TestContinueLocalTaskInCloudCreatesDurableConversationBranch(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.UpsertProvider(ctx, domain.Provider{ID: "provider_continue_cloud", UserID: owner, Name: "Test", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "test", CreatedAt: now, UpdatedAt: now}, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	base := "0123456789012345678901234567890123456789"
	deltaCommit := "1123456789012345678901234567890123456789"
	sourceProject := domain.Project{ID: "project_continue_source", UserID: owner, Name: "Source project", Workdir: `C:\cloud\source`, RemoteRepoURL: "https://github.com/example/repo.git", RemoteBranch: "main", RepositoryProvider: "github", ResolvedCommit: base, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, sourceProject); err != nil {
		t.Fatal(err)
	}
	const taskID = "task_continue_cloud"
	importedProject := domain.Project{ID: agent.ProjectDeltaProjectID(sourceProject.ID, taskID), UserID: owner, Name: "Source project · task " + taskID, Workdir: `C:\cloud\isolated`, RemoteRepoURL: sourceProject.RemoteRepoURL, RemoteBranch: "codex/task-" + taskID, RepositoryProvider: "github", ResolvedCommit: deltaCommit, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, importedProject); err != nil {
		t.Fatal(err)
	}
	sourceConversation := domain.Conversation{ID: "conversation_continue_source", UserID: owner, Title: "Source", ProviderID: "provider_continue_cloud", ProjectID: sourceProject.ID, PermissionProfile: domain.DefaultPermissionProfile(), CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, sourceConversation, generation); err != nil {
		t.Fatal(err)
	}
	sourceMessages := []domain.Message{{ID: "message_continue_source_user", ConversationID: sourceConversation.ID, Role: "user", Content: "Initial request", CreatedAt: now}, {ID: "message_continue_source_assistant", ConversationID: sourceConversation.ID, Role: "assistant", Content: "Working locally", CreatedAt: now.Add(time.Second)}}
	for _, message := range sourceMessages {
		if err := store.AddMessage(ctx, owner, message); err != nil {
			t.Fatal(err)
		}
	}
	sourceDetail, err := store.Conversation(ctx, owner, sourceConversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	storeArtifact := func(key, name, role string, content []byte) storage.Artifact {
		t.Helper()
		digest := sha256.Sum256(content)
		artifact, err := artifacts.StoreFromReader(ctx, owner, name, "application/json", key, int64(len(content)), hex.EncodeToString(digest[:]), bytes.NewReader(content), now)
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	contextBytes, err := json.Marshal(localTaskConversationContext{Conversation: sourceDetail, Project: &sourceProject})
	if err != nil {
		t.Fatal(err)
	}
	contextArtifact := storeArtifact("continue-context", "context.json", "conversation_context", contextBytes)
	transcriptMessages := []domain.Message{{ID: "message_continue_local_context_1", ConversationID: "conversation_continue_local", Role: "user", Content: "Initial request", CreatedAt: now}, {ID: "message_continue_local_context_2", ConversationID: "conversation_continue_local", Role: "assistant", Content: "Working locally", CreatedAt: now.Add(time.Second)}, {ID: "message_continue_local_user", ConversationID: "conversation_continue_local", Role: "user", Content: "Finish locally", CreatedAt: now.Add(2 * time.Second)}, {ID: "message_continue_local_result", ConversationID: "conversation_continue_local", Role: "assistant", Content: "Local result is ready", CreatedAt: now.Add(3 * time.Second)}}
	localConversation := domain.ConversationDetail{Conversation: domain.Conversation{ID: "conversation_continue_local", UserID: owner, Title: "Local task", ProviderID: sourceConversation.ProviderID, ProjectID: sourceProject.ID, PermissionProfile: sourceConversation.PermissionProfile, CreatedAt: now, UpdatedAt: now.Add(3 * time.Second)}, Messages: transcriptMessages}
	localTurn := domain.AgentTurn{ID: "turn_continue_local", ConversationID: localConversation.ID, Status: "incomplete", StopReason: "step_limit", ContinuationAvailable: true, ResultMessageID: "message_continue_local_result"}
	localEffectTrace := []domain.TraceEvent{
		{ConversationID: localConversation.ID, TurnID: localTurn.ID, Sequence: 1, Kind: "permission.checked", Details: json.RawMessage(`{"toolCallId":"call_publish","tool":"publish","effect":"external_write","outcome":"allow"}`)},
		{ConversationID: localConversation.ID, TurnID: localTurn.ID, Sequence: 2, Kind: "tool.started", Details: json.RawMessage(`{"toolCallId":"call_publish","name":"publish","effect":"external_write"}`)},
		{ConversationID: localConversation.ID, TurnID: localTurn.ID, Sequence: 3, Kind: "tool.completed", Details: json.RawMessage(`{"toolCallId":"call_publish","name":"publish","ok":true}`)},
	}
	transcriptBytes, err := json.Marshal(map[string]any{"sourceConversationId": sourceConversation.ID, "projectId": sourceProject.ID, "projectCommit": base, "conversationId": localConversation.ID, "agentTurnId": localTurn.ID, "resultMessageId": localTurn.ResultMessageID, "agentStatus": "incomplete", "agentStopReason": "step_limit", "transcript": map[string]any{"conversation": localConversation, "turns": []domain.AgentTurn{localTurn}, "trace": localEffectTrace}})
	if err != nil {
		t.Fatal(err)
	}
	transcriptArtifact := storeArtifact("continue-transcript", "transcript.json", "conversation_transcript", transcriptBytes)
	userInputArtifact := storeArtifact("continue-user-input", "notes.txt", "user_input", []byte("user-provided notes"))
	projectDeltaContent := []byte("task delta marker")
	projectDeltaArtifact := storeArtifact("continue-delta", "delta.bundle", "project_delta", projectDeltaContent)
	cloudNode, _, err := store.EnsureCloudExecutionNode(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"kind": "agent_prompt", "sourceConversationId": sourceConversation.ID, "content": "Finish locally", "sourceContextArtifactId": contextArtifact.ID, "sourceContextSHA256": contextArtifact.SHA256})
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: taskID, UserID: owner, NodeID: cloudNode.ID, IdempotencyKey: "continue-cloud-task", Payload: payload}, false, now); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		artifact storage.Artifact
		role     string
	}{{contextArtifact, "conversation_context"}, {transcriptArtifact, "conversation_transcript"}, {projectDeltaArtifact, "project_delta"}, {userInputArtifact, "user_input"}} {
		if err := store.AttachExecutionTaskArtifact(ctx, owner, taskID, item.artifact.ID, item.role, now); err != nil {
			t.Fatal(err)
		}
	}
	lease := "continue-cloud-lease"
	leaseHash := hashToken(lease)
	if _, err := store.ClaimExecutionTask(ctx, cloudNode.ID, leaseHash, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, cloudNode.ID, taskID, leaseHash, "accepted", nil, "", now.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, cloudNode.ID, taskID, leaseHash, "running", nil, "", now.Add(200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"sourceConversationId": sourceConversation.ID, "projectId": sourceProject.ID, "projectCommit": base, "projectDeltaBaseCommit": base, "projectDeltaCommit": deltaCommit, "conversationId": localConversation.ID, "agentTurnId": localTurn.ID, "resultMessageId": localTurn.ResultMessageID, "agentStatus": "incomplete", "agentStopReason": "step_limit", "transcriptArtifact": transcriptArtifact, "projectDeltaArtifact": projectDeltaArtifact})
	if _, err := store.ReportExecutionTask(ctx, cloudNode.ID, taskID, leaseHash, "reported_succeeded", result, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store, artifacts: artifacts}
	previewRequest := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+taskID+"/continuation-preview", nil)
	previewRequest.SetPathValue("id", taskID)
	previewResponse := httptest.NewRecorder()
	server.executionTaskHandoffPreview(previewResponse, previewRequest)
	var previewBody struct {
		TaskID                  string                      `json:"taskId"`
		Effects                 []taskHandoffExternalEffect `json:"effects"`
		RequiresAcknowledgement bool                        `json:"requiresAcknowledgement"`
	}
	if previewResponse.Code != http.StatusOK || json.Unmarshal(previewResponse.Body.Bytes(), &previewBody) != nil || previewBody.TaskID != taskID || !previewBody.RequiresAcknowledgement || len(previewBody.Effects) != 1 || previewBody.Effects[0].Tool != "publish" || previewBody.Effects[0].State != "completed" {
		t.Fatalf("handoff preview = %d: %s", previewResponse.Code, previewResponse.Body.String())
	}
	unacknowledged := httptest.NewRecorder()
	unauthorizedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+taskID+"/continue-in-cloud", bytes.NewReader([]byte(`{}`)))
	unauthorizedRequest.SetPathValue("id", taskID)
	server.executionTaskContinueInCloud(unacknowledged, unauthorizedRequest)
	if unacknowledged.Code != http.StatusPreconditionRequired || !bytes.Contains(unacknowledged.Body.Bytes(), []byte(`"requiresAcknowledgement":true`)) {
		t.Fatalf("unreviewed effect handoff = %d: %s", unacknowledged.Code, unacknowledged.Body.String())
	}
	if _, err := store.ExecutionTaskByIdempotencyKey(ctx, owner, "cloud-handoff:"+taskID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unreviewed handoff left a cloud task behind: %v", err)
	}
	if _, err := store.Conversation(ctx, owner, agent.TaskContinuationConversationID(sourceConversation.ID, taskID)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unreviewed handoff left a cloud continuation conversation behind: %v", err)
	}
	globalAcknowledgement := httptest.NewRecorder()
	globalAckRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+taskID+"/continue-in-cloud", bytes.NewReader([]byte(`{"acknowledgeExternalEffects":true}`)))
	globalAckRequest.SetPathValue("id", taskID)
	server.executionTaskContinueInCloud(globalAcknowledgement, globalAckRequest)
	if globalAcknowledgement.Code != http.StatusPreconditionRequired {
		t.Fatalf("global acknowledgement without per-effect decisions = %d: %s", globalAcknowledgement.Code, globalAcknowledgement.Body.String())
	}
	if _, err := store.ExecutionTaskByIdempotencyKey(ctx, owner, "cloud-handoff:"+taskID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("global acknowledgement left a cloud task behind: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+taskID+"/continue-in-cloud", bytes.NewReader([]byte(`{"acknowledgeExternalEffects":true,"effectResolutions":[{"toolCallId":"call_publish","outcome":"confirmed_applied"}]}`)))
	request.ContentLength = -1
	request.SetPathValue("id", taskID)
	response := httptest.NewRecorder()
	server.executionTaskContinueInCloud(response, request)
	var responseBody struct {
		Conversation domain.ConversationDetail `json:"conversation"`
		Project      domain.Project            `json:"project"`
		Task         storage.ExecutionTask     `json:"task"`
	}
	if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &responseBody) != nil {
		t.Fatalf("continue in cloud = %d: %s", response.Code, response.Body.String())
	}
	if responseBody.Conversation.ParentConversationID != sourceConversation.ID || responseBody.Conversation.ProjectID != importedProject.ID || responseBody.Conversation.ExecutionPaused || len(responseBody.Conversation.Messages) != len(transcriptMessages) || responseBody.Conversation.Messages[len(transcriptMessages)-1].Content != "Local result is ready" || responseBody.Project.ID != importedProject.ID {
		t.Fatalf("cloud continuation did not include the task exchange on its imported project: %+v", responseBody)
	}
	var queuedPayload struct {
		Kind                       string                        `json:"kind"`
		ConversationID             string                        `json:"conversationId"`
		Content                    string                        `json:"content"`
		HandoffEffectsAcknowledged bool                          `json:"handoffEffectsAcknowledged"`
		HandoffEffects             []taskHandoffExternalEffect   `json:"handoffEffects"`
		HandoffEffectResolutions   []taskHandoffEffectResolution `json:"handoffEffectResolutions"`
		Attachments                []taskInputArtifact           `json:"attachments"`
	}
	if responseBody.Task.ID == "" || responseBody.Task.Status != "queued" || responseBody.Task.NodeID != storage.CloudExecutionNodeID(owner) || responseBody.Task.LogicalTaskID != taskID || responseBody.Task.ParentTaskID != taskID || responseBody.Task.SegmentIndex != 1 || json.Unmarshal(responseBody.Task.Payload, &queuedPayload) != nil || queuedPayload.Kind != "agent_turn" || queuedPayload.ConversationID != responseBody.Conversation.ID || !queuedPayload.HandoffEffectsAcknowledged || len(queuedPayload.HandoffEffects) != 1 || len(queuedPayload.HandoffEffectResolutions) != 1 || queuedPayload.HandoffEffectResolutions[0].Outcome != "confirmed_applied" || !strings.Contains(queuedPayload.Content, "不得重复执行") || len(queuedPayload.Attachments) != 1 || queuedPayload.Attachments[0].ArtifactID != userInputArtifact.ID {
		t.Fatalf("cloud continuation was not read back as a durable cloud execution task: %+v payload=%s", responseBody.Task, responseBody.Task.Payload)
	}
	if err := server.verifyTaskInputArtifactLinks(ctx, responseBody.Task.ID, queuedPayload.Attachments); err != nil {
		t.Fatalf("cloud continuation did not atomically bind user input artifacts: %v", err)
	}
	parentReadBack, err := store.ExecutionTask(ctx, owner, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.attachImportedProject(httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil), &parentReadBack); err != nil {
		t.Fatal(err)
	}
	if parentReadBack.ContinuedConversation == nil || parentReadBack.ContinuationTaskID != responseBody.Task.ID || parentReadBack.ContinuationTaskStatus != "queued" {
		t.Fatalf("parent task read model did not expose the durable cloud continuation: %+v", parentReadBack)
	}
	retry := httptest.NewRecorder()
	retryRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+taskID+"/continue-in-cloud", bytes.NewReader([]byte(`{"acknowledgeExternalEffects":true,"effectResolutions":[{"toolCallId":"call_publish","outcome":"confirmed_applied"}]}`)))
	retryRequest.SetPathValue("id", taskID)
	server.executionTaskContinueInCloud(retry, retryRequest)
	if retry.Code != http.StatusOK || !bytes.Contains(retry.Body.Bytes(), []byte(responseBody.Conversation.ID)) || !bytes.Contains(retry.Body.Bytes(), []byte(responseBody.Task.ID)) {
		t.Fatalf("idempotent cloud continuation retry = %d: %s", retry.Code, retry.Body.String())
	}
	changedResolutionRetry := httptest.NewRecorder()
	changedResolutionRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+taskID+"/continue-in-cloud", bytes.NewReader([]byte(`{"effectResolutions":[{"toolCallId":"call_publish","outcome":"confirmed_not_applied"}]}`)))
	changedResolutionRequest.SetPathValue("id", taskID)
	server.executionTaskContinueInCloud(changedResolutionRetry, changedResolutionRequest)
	if changedResolutionRetry.Code != http.StatusConflict {
		t.Fatalf("changed external-effect decision on idempotent retry = %d: %s", changedResolutionRetry.Code, changedResolutionRetry.Body.String())
	}
	unchangedTaskReadBack, err := store.ExecutionTask(ctx, owner, responseBody.Task.ID)
	if err != nil {
		t.Fatalf("read original continuation after rejected decision change: %v", err)
	}
	var unchangedPayload struct {
		Resolutions []taskHandoffEffectResolution `json:"handoffEffectResolutions"`
	}
	if json.Unmarshal(unchangedTaskReadBack.Payload, &unchangedPayload) != nil || len(unchangedPayload.Resolutions) != 1 || unchangedPayload.Resolutions[0].Outcome != "confirmed_applied" {
		t.Fatalf("rejected handoff retry changed the durable effect audit: payload=%s", unchangedTaskReadBack.Payload)
	}
	// Drain the just-created continuation before adding the next queued task;
	// claims are ordered, so leaving it queued would make the following
	// completed-task scenario accidentally report against this task's lease.
	continuationLease := "continue-cloud-queued-task-lease"
	continuationLeaseHash := hashToken(continuationLease)
	transitionAt := time.Now().UTC()
	if _, err := store.ClaimExecutionTask(ctx, cloudNode.ID, continuationLeaseHash, transitionAt.Add(time.Minute), transitionAt); err != nil {
		t.Fatalf("claim persisted continuation for cleanup: %v", err)
	}
	for index, status := range []string{"accepted", "running", "reported_failed"} {
		errorText := ""
		if status == "reported_failed" {
			errorText = "test cleanup"
		}
		if _, err := store.ReportExecutionTask(ctx, cloudNode.ID, responseBody.Task.ID, continuationLeaseHash, status, nil, errorText, transitionAt.Add(time.Duration(index+1)*time.Second)); err != nil {
			t.Fatalf("drain persisted continuation status %q: %v", status, err)
		}
	}

	completedTaskID := taskID + "_completed"
	completedProject := importedProject
	completedProject.ID = agent.ProjectDeltaProjectID(sourceProject.ID, completedTaskID)
	completedProject.Name += " completed"
	completedProject.RemoteBranch = "codex/task-" + completedTaskID
	if err := store.CreateProject(ctx, completedProject); err != nil {
		t.Fatal(err)
	}
	completedTurn := localTurn
	completedTurn.Status = "completed"
	completedTurn.StopReason = "assistant_response"
	completedTurn.ContinuationAvailable = false
	completedTranscript, err := json.Marshal(map[string]any{"sourceConversationId": sourceConversation.ID, "projectId": sourceProject.ID, "projectCommit": base, "conversationId": localConversation.ID, "agentTurnId": completedTurn.ID, "resultMessageId": completedTurn.ResultMessageID, "agentStatus": "completed", "agentStopReason": "assistant_response", "transcript": map[string]any{"conversation": localConversation, "turns": []domain.AgentTurn{completedTurn}}})
	if err != nil {
		t.Fatal(err)
	}
	completedTranscriptArtifact := storeArtifact("continue-completed-transcript", "completed-transcript.json", "conversation_transcript", completedTranscript)
	completedTaskPayload := payload
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: completedTaskID, UserID: owner, NodeID: cloudNode.ID, IdempotencyKey: "continue-cloud-completed-task", Payload: completedTaskPayload}, false, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		artifact storage.Artifact
		role     string
	}{{contextArtifact, "conversation_context"}, {completedTranscriptArtifact, "conversation_transcript"}, {projectDeltaArtifact, "project_delta"}} {
		if err := store.AttachExecutionTaskArtifact(ctx, owner, completedTaskID, item.artifact.ID, item.role, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	completedLease := "continue-cloud-completed-lease"
	completedLeaseHash := hashToken(completedLease)
	if _, err := store.ClaimExecutionTask(ctx, cloudNode.ID, completedLeaseHash, now.Add(3*time.Minute), now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, cloudNode.ID, completedTaskID, completedLeaseHash, "accepted", nil, "", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, cloudNode.ID, completedTaskID, completedLeaseHash, "running", nil, "", now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	completedResult, err := json.Marshal(map[string]any{"sourceConversationId": sourceConversation.ID, "projectId": sourceProject.ID, "projectCommit": base, "projectDeltaBaseCommit": base, "projectDeltaCommit": deltaCommit, "conversationId": localConversation.ID, "agentTurnId": completedTurn.ID, "resultMessageId": completedTurn.ResultMessageID, "agentStatus": "completed", "agentStopReason": "assistant_response", "transcriptArtifact": completedTranscriptArtifact, "projectDeltaArtifact": projectDeltaArtifact})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, cloudNode.ID, completedTaskID, completedLeaseHash, "reported_succeeded", completedResult, "", now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	completedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+completedTaskID+"/continue-in-cloud", bytes.NewReader([]byte(`{}`)))
	completedRequest.SetPathValue("id", completedTaskID)
	completedResponse := httptest.NewRecorder()
	server.executionTaskContinueInCloud(completedResponse, completedRequest)
	if completedResponse.Code != http.StatusAccepted || !bytes.Contains(completedResponse.Body.Bytes(), []byte(completedProject.ID)) || !bytes.Contains(completedResponse.Body.Bytes(), []byte(`"kind":"agent_turn"`)) {
		t.Fatalf("completed task continuation = %d: %s", completedResponse.Code, completedResponse.Body.String())
	}

	// Expired-lease evidence remains quarantined, but a user can create a
	// separate cloud continuation after reviewing effects and confirming stop.
	recoveryNow := time.Now().UTC()
	recoveryNode, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_recovery_continue", UserID: owner, Name: "Recovery node", Platform: "linux"}, hashToken("recovery-node-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, hashToken("recovery-node-credential"), "linux", nil, recoveryNow); err != nil {
		t.Fatal(err)
	}
	recoveryPayload, _ := json.Marshal(map[string]string{"kind": "agent_prompt", "sourceConversationId": sourceConversation.ID, "content": "Finish locally", "sourceContextArtifactId": contextArtifact.ID, "sourceContextSHA256": contextArtifact.SHA256})
	recoveryTask, _, err := store.CreateExecutionTaskWithArtifacts(ctx, storage.ExecutionTask{ID: "task_recovery_continue", UserID: owner, NodeID: recoveryNode.ID, IdempotencyKey: "recovery-continue", Payload: recoveryPayload}, false, []storage.ExecutionTaskArtifactLink{{ArtifactID: contextArtifact.ID, Role: "conversation_context"}}, recoveryNow)
	if err != nil {
		t.Fatal(err)
	}
	const recoveryLease = "recovery-continue-lease"
	if _, err := store.ClaimExecutionTask(ctx, recoveryNode.ID, hashToken(recoveryLease), recoveryNow.Add(time.Minute), recoveryNow); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, recoveryNow.Add(2*time.Minute)); err != nil || count != 1 {
		t.Fatalf("expire recovery continuation lease: count=%d err=%v", count, err)
	}
	if err := store.AttachExecutionTaskRecoveryEvidence(ctx, recoveryNode.ID, recoveryTask.ID, hashToken(recoveryLease), transcriptArtifact.ID, "recovery_transcript", recoveryNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	recoveryManifest, _ := json.Marshal(map[string]any{"sourceConversationId": sourceConversation.ID, "projectId": sourceProject.ID, "projectCommit": base, "conversationId": localConversation.ID, "agentTurnId": localTurn.ID, "resultMessageId": localTurn.ResultMessageID, "agentStatus": "incomplete", "agentStopReason": "step_limit", "transcriptArtifact": transcriptArtifact})
	if _, err := store.SaveExecutionTaskRecoveryResult(ctx, recoveryNode.ID, recoveryTask.ID, hashToken(recoveryLease), recoveryManifest, recoveryNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	recoveryPreview := httptest.NewRecorder()
	recoveryPreviewRequest := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+recoveryTask.ID+"/continuation-preview", nil)
	recoveryPreviewRequest.SetPathValue("id", recoveryTask.ID)
	server.executionTaskHandoffPreview(recoveryPreview, recoveryPreviewRequest)
	if recoveryPreview.Code != http.StatusOK || !bytes.Contains(recoveryPreview.Body.Bytes(), []byte(`"recoveryContinuation":true`)) || !bytes.Contains(recoveryPreview.Body.Bytes(), []byte(`"requiresOldNodeStopConfirmation":true`)) {
		t.Fatalf("recovery preview = %d: %s", recoveryPreview.Code, recoveryPreview.Body.String())
	}
	missingStop := httptest.NewRecorder()
	missingStopRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+recoveryTask.ID+"/continue-in-cloud", bytes.NewReader([]byte(`{"acknowledgeExternalEffects":true,"effectResolutions":[{"toolCallId":"call_publish","outcome":"unknown"}]}`)))
	missingStopRequest.SetPathValue("id", recoveryTask.ID)
	server.executionTaskContinueInCloud(missingStop, missingStopRequest)
	if missingStop.Code != http.StatusPreconditionRequired || !bytes.Contains(missingStop.Body.Bytes(), []byte(`"requiresOldNodeStopConfirmation":true`)) {
		t.Fatalf("recovery continuation without stop confirmation = %d: %s", missingStop.Code, missingStop.Body.String())
	}
	confirmed := httptest.NewRecorder()
	confirmedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+recoveryTask.ID+"/continue-in-cloud", bytes.NewReader([]byte(`{"acknowledgeExternalEffects":true,"confirmOldNodeStopped":true,"effectResolutions":[{"toolCallId":"call_publish","outcome":"unknown"}]}`)))
	confirmedRequest.SetPathValue("id", recoveryTask.ID)
	server.executionTaskContinueInCloud(confirmed, confirmedRequest)
	if confirmed.Code != http.StatusAccepted {
		t.Fatalf("confirmed recovery continuation = %d: %s", confirmed.Code, confirmed.Body.String())
	}
	recoveryTaskReadBack, err := store.ExecutionTask(ctx, owner, recoveryTask.ID)
	if err != nil || recoveryTaskReadBack.Status != "needs_reconciliation" || len(recoveryTaskReadBack.Result) != 0 || len(recoveryTaskReadBack.RecoveryResult) == 0 {
		t.Fatalf("recovery continuation rewrote original task outcome: task=%+v err=%v", recoveryTaskReadBack, err)
	}

	projectRecoveryNode, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_recovery_project", UserID: owner, Name: "Project recovery node", Platform: "linux"}, hashToken("recovery-project-node-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, hashToken("recovery-project-node-credential"), "linux", nil, recoveryNow); err != nil {
		t.Fatal(err)
	}
	projectRecoveryTask, _, err := store.CreateExecutionTaskWithArtifacts(ctx, storage.ExecutionTask{ID: "task_recovery_project", UserID: owner, NodeID: projectRecoveryNode.ID, IdempotencyKey: "recovery-project", Payload: recoveryPayload}, false, []storage.ExecutionTaskArtifactLink{{ArtifactID: contextArtifact.ID, Role: "conversation_context"}}, recoveryNow)
	if err != nil {
		t.Fatal(err)
	}
	const projectRecoveryLease = "recovery-project-lease"
	if _, err := store.ClaimExecutionTask(ctx, projectRecoveryNode.ID, hashToken(projectRecoveryLease), recoveryNow.Add(time.Minute), recoveryNow); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, recoveryNow.Add(2*time.Minute)); err != nil || count != 1 {
		t.Fatalf("expire recovery project lease: count=%d err=%v", count, err)
	}
	if err := store.AttachExecutionTaskRecoveryEvidence(ctx, projectRecoveryNode.ID, projectRecoveryTask.ID, hashToken(projectRecoveryLease), transcriptArtifact.ID, "recovery_transcript", recoveryNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskRecoveryEvidence(ctx, projectRecoveryNode.ID, projectRecoveryTask.ID, hashToken(projectRecoveryLease), projectDeltaArtifact.ID, "recovery_project_delta", recoveryNow.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	projectRecoveryManifest, _ := json.Marshal(map[string]any{"sourceConversationId": sourceConversation.ID, "projectId": sourceProject.ID, "projectCommit": base, "projectDeltaBaseCommit": base, "projectDeltaCommit": deltaCommit, "conversationId": localConversation.ID, "agentTurnId": localTurn.ID, "resultMessageId": localTurn.ResultMessageID, "agentStatus": "incomplete", "agentStopReason": "step_limit", "transcriptArtifact": transcriptArtifact, "projectDeltaArtifact": projectDeltaArtifact})
	if _, err := store.SaveExecutionTaskRecoveryResult(ctx, projectRecoveryNode.ID, projectRecoveryTask.ID, hashToken(projectRecoveryLease), projectRecoveryManifest, recoveryNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	projectImport := httptest.NewRecorder()
	projectImportRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+projectRecoveryTask.ID+"/import-project-delta", bytes.NewReader([]byte(`{}`)))
	projectImportRequest.SetPathValue("id", projectRecoveryTask.ID)
	server.executionTaskImportProjectDelta(projectImport, projectImportRequest)
	if projectImport.Code != http.StatusPreconditionRequired || !bytes.Contains(projectImport.Body.Bytes(), []byte(`"requiresOldNodeStopConfirmation":true`)) {
		t.Fatalf("recovered project import without stop confirmation = %d: %s", projectImport.Code, projectImport.Body.String())
	}
	projectsAfterRejectedImport, err := store.ListProjects(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projectsAfterRejectedImport {
		if project.ID == agent.ProjectDeltaProjectID(sourceProject.ID, projectRecoveryTask.ID) {
			t.Fatalf("recovery project import created task branch without stop confirmation: %+v", project)
		}
	}
	projectRecoveryReadBack, err := store.ExecutionTask(ctx, owner, projectRecoveryTask.ID)
	if err != nil || projectRecoveryReadBack.Status != "needs_reconciliation" || len(projectRecoveryReadBack.Result) != 0 {
		t.Fatalf("rejected recovery import changed original task outcome: task=%+v err=%v", projectRecoveryReadBack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	continuedTaskReadBack, err := store.ExecutionTask(ctx, owner, responseBody.Task.ID)
	if err != nil {
		t.Fatalf("read cloud handoff task after database restart: %v", err)
	}
	var restartedPayload struct {
		HandoffEffectsAcknowledged bool                          `json:"handoffEffectsAcknowledged"`
		HandoffEffectResolutions   []taskHandoffEffectResolution `json:"handoffEffectResolutions"`
	}
	if json.Unmarshal(continuedTaskReadBack.Payload, &restartedPayload) != nil || !restartedPayload.HandoffEffectsAcknowledged || len(restartedPayload.HandoffEffectResolutions) != 1 || restartedPayload.HandoffEffectResolutions[0].ToolCallID != "call_publish" || restartedPayload.HandoffEffectResolutions[0].Outcome != "confirmed_applied" {
		t.Fatalf("per-effect handoff audit did not survive database restart: task=%+v", continuedTaskReadBack)
	}
}

func TestValidLocalTaskContinuationState(t *testing.T) {
	for _, test := range []struct {
		status string
		reason string
		valid  bool
	}{
		{status: "completed", reason: "assistant_response", valid: true},
		{status: "incomplete", reason: "step_limit", valid: true},
		{status: "incomplete", reason: "model_call_limit", valid: true},
		{status: "incomplete", reason: "stalled", valid: true},
		{status: "incomplete", reason: "cancelled"},
		{status: "needs_reconciliation", reason: "tool_effect_unknown"},
		{status: "completed", reason: "step_limit"},
	} {
		t.Run(test.status+"/"+test.reason, func(t *testing.T) {
			if got := validLocalTaskContinuationState(test.status, test.reason); got != test.valid {
				t.Fatalf("validLocalTaskContinuationState(%q, %q) = %t, want %t", test.status, test.reason, got, test.valid)
			}
		})
	}
}

func TestLocalAgentTaskSubmitTargetsPairedNodeAndPreservesIdempotencyAfterClaim(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}
	repoDir := filepath.Join(t.TempDir(), "cloud-project")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, git, args...)
		cmd.Dir = repoDir
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	runGit("init")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.invalid")
	runGit("remote", "add", "origin", "https://github.com/example/shared")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("pinned base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "--all")
	runGit("commit", "-m", "pinned base")
	baseCommit := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("cloud workspace change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "--all")
	runGit("commit", "-m", "cloud workspace change")
	deltaDir := filepath.Join(t.TempDir(), "bundles")
	if err := os.MkdirAll(deltaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lfsArchivePath := filepath.Join(deltaDir, "project-lfs-objects.tar")
	if err := os.WriteFile(lfsArchivePath, []byte("verified Git LFS archive payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	submoduleArchivePath := filepath.Join(deltaDir, "project-submodule-deltas.tar")
	if err := os.WriteFile(submoduleArchivePath, []byte("verified Git submodule archive payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotter := &testProjectDeltaSnapshotter{path: deltaDir, lfsPath: lfsArchivePath, submodulePath: submoduleArchivePath}
	if err := store.UpsertProvider(ctx, domain.Provider{ID: "provider_local_submit", UserID: owner, Name: "Local", Kind: "openai", BaseURL: "https://example.invalid", Model: "test", ContextWindow: 8192, CreatedAt: now, UpdatedAt: now}, []byte{}, []byte{}); err != nil {
		t.Fatal(err)
	}
	// A large local project is valid work. 500 MB only selects the transfer
	// strategy; it must never prevent dispatch to an online local node.
	project := domain.Project{ID: "project_local_submit", UserID: owner, Name: "Shared repository", Workdir: repoDir, RemoteRepoURL: "https://github.com/example/shared", RemoteBranch: "main", RepositoryProvider: "github", ResolvedCommit: baseCommit, MeasuredBytes: projectpolicy.DirectTransferBatchBytes + 1, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConversation(ctx, domain.Conversation{ID: "conversation_local_submit", UserID: owner, Title: "Local job", ProviderID: "provider_local_submit", ProjectID: project.ID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	sourceMessages := []domain.Message{
		{ID: "message_local_source_user", ConversationID: "conversation_local_submit", Role: "user", Content: "Earlier request", CreatedAt: now.Add(-2 * time.Minute)},
		{ID: "message_local_source_assistant", ConversationID: "conversation_local_submit", Role: "assistant", Content: "Earlier answer", CreatedAt: now.Add(-time.Minute)},
	}
	for _, message := range sourceMessages {
		if err := store.AddMessage(ctx, owner, message); err != nil {
			t.Fatal(err)
		}
	}
	const nodeSecret = "local-node-submit-credential"
	nodeDigest := sha256.Sum256([]byte(nodeSecret))
	node, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_local_submit", UserID: owner, Name: "Laptop", Platform: "windows"}, hex.EncodeToString(nodeDigest[:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, hex.EncodeToString(nodeDigest[:]), "windows", []string{"agent-runtime"}, now); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	inputContent := []byte("user task input file\n")
	inputDigest := sha256.Sum256(inputContent)
	inputArtifact, err := artifacts.StoreFromReader(ctx, owner, "notes.txt", "text/plain", "local-submit-user-input", int64(len(inputContent)), hex.EncodeToString(inputDigest[:]), bytes.NewReader(inputContent), now)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store, artifacts: artifacts, projectDeltas: snapshotter}
	call := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"targetNodeId": node.ID, "content": "prepare the local project", "artifactIds": []string{inputArtifact.ID}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/conversation_local_submit/tasks/nodes", bytes.NewReader(body))
		req.SetPathValue("id", "conversation_local_submit")
		req.Header.Set("Idempotency-Key", "local-node-submit-key")
		response := httptest.NewRecorder()
		server.nodeAgentTaskSubmit(response, req)
		return response
	}
	first := call()
	if first.Code != http.StatusAccepted {
		t.Fatalf("local task submit = %d (snapshot calls=%d): %s", first.Code, snapshotter.calls, first.Body.String())
	}
	var submitted struct {
		Task    storage.ExecutionTask `json:"task"`
		Created bool                  `json:"created"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &submitted); err != nil || !submitted.Created || submitted.Task.Status != "queued" || submitted.Task.NodeID != node.ID {
		t.Fatalf("local task was not durably queued to selected node: %+v err=%v", submitted, err)
	}
	var payload struct {
		Kind                    string                     `json:"kind"`
		SourceConversationID    string                     `json:"sourceConversationId"`
		Content                 string                     `json:"content"`
		SourceContextArtifactID string                     `json:"sourceContextArtifactId"`
		SourceContextSHA256     string                     `json:"sourceContextSHA256"`
		SourceContextByteSize   int64                      `json:"sourceContextByteSize"`
		SourceProjectDelta      *nodeAgentTaskProjectDelta `json:"sourceProjectDelta"`
		Attachments             []taskInputArtifact        `json:"attachments"`
	}
	if err := json.Unmarshal(submitted.Task.Payload, &payload); err != nil || payload.Kind != "agent_prompt" || payload.SourceConversationID != "conversation_local_submit" || payload.Content != "prepare the local project" || payload.SourceContextArtifactID == "" || len(payload.SourceContextSHA256) != 64 || payload.SourceContextByteSize <= 0 || payload.SourceProjectDelta == nil || payload.SourceProjectDelta.BaseCommit != baseCommit || payload.SourceProjectDelta.Commit == baseCommit || payload.SourceProjectDelta.ByteSize <= 0 || len(payload.SourceProjectDelta.SHA256) != 64 || payload.SourceProjectDelta.LFSObjects == nil || payload.SourceProjectDelta.LFSObjects.ByteSize != int64(len("verified Git LFS archive payload")) || payload.SourceProjectDelta.SubmoduleDeltas == nil || payload.SourceProjectDelta.SubmoduleDeltas.ByteSize != int64(len("verified Git submodule archive payload")) || len(payload.Attachments) != 1 || payload.Attachments[0] != (taskInputArtifact{ArtifactID: inputArtifact.ID, SHA256: inputArtifact.SHA256, ByteSize: inputArtifact.ByteSize, FileName: inputArtifact.FileName, MediaType: inputArtifact.MediaType}) {
		t.Fatalf("persisted local task payload mismatch: %+v err=%v", payload, err)
	}
	attached, err := store.ExecutionTaskArtifacts(ctx, owner, submitted.Task.ID)
	if err != nil || len(attached) != 5 {
		t.Fatalf("source context was not atomically attached to the task: %+v err=%v", attached, err)
	}
	roles := map[string]storage.ExecutionTaskArtifact{}
	for _, artifact := range attached {
		roles[artifact.Role] = artifact
	}
	if roles["conversation_context"].ID != payload.SourceContextArtifactID || roles["conversation_context"].SHA256 != payload.SourceContextSHA256 || roles["conversation_context"].ByteSize != payload.SourceContextByteSize || roles["source_project_delta"].ID != payload.SourceProjectDelta.ArtifactID || roles["source_project_delta"].SHA256 != payload.SourceProjectDelta.SHA256 || roles["source_project_delta"].ByteSize != payload.SourceProjectDelta.ByteSize {
		t.Fatalf("source context and project delta were not durably associated with the task: %+v", attached)
	}
	if roles["source_project_lfs_objects"].ID != payload.SourceProjectDelta.LFSObjects.ArtifactID || roles["source_project_lfs_objects"].SHA256 != payload.SourceProjectDelta.LFSObjects.SHA256 || roles["source_project_lfs_objects"].ByteSize != payload.SourceProjectDelta.LFSObjects.ByteSize {
		t.Fatalf("source Git LFS archive was not durably associated with the task: %+v", attached)
	}
	if roles["source_project_submodule_deltas"].ID != payload.SourceProjectDelta.SubmoduleDeltas.ArtifactID || roles["source_project_submodule_deltas"].SHA256 != payload.SourceProjectDelta.SubmoduleDeltas.SHA256 || roles["source_project_submodule_deltas"].ByteSize != payload.SourceProjectDelta.SubmoduleDeltas.ByteSize {
		t.Fatalf("source submodule delta archive was not durably associated with the task: %+v", attached)
	}
	if roles["user_input"].ID != inputArtifact.ID || roles["user_input"].SHA256 != inputArtifact.SHA256 || roles["user_input"].ByteSize != inputArtifact.ByteSize {
		t.Fatalf("user input attachment was not durably associated with the task: %+v", attached)
	}
	deltaArtifact, deltaFile, err := artifacts.Open(ctx, owner, payload.SourceProjectDelta.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	deltaPath := filepath.Join(t.TempDir(), "stored-project-delta.bundle")
	deltaOnDisk, err := os.Create(deltaPath)
	if err != nil {
		_ = deltaFile.Close()
		t.Fatal(err)
	}
	if _, err := io.Copy(deltaOnDisk, deltaFile); err != nil {
		_ = deltaFile.Close()
		_ = deltaOnDisk.Close()
		t.Fatal(err)
	}
	if err := errors.Join(deltaFile.Close(), deltaOnDisk.Close()); err != nil {
		t.Fatal(err)
	}
	if deltaArtifact.ID != payload.SourceProjectDelta.ArtifactID || deltaArtifact.SHA256 != payload.SourceProjectDelta.SHA256 || deltaArtifact.ByteSize != payload.SourceProjectDelta.ByteSize {
		t.Fatalf("source project delta failed artifact read-back: %+v", deltaArtifact)
	}
	submoduleArtifact, submoduleFile, err := artifacts.Open(ctx, owner, payload.SourceProjectDelta.SubmoduleDeltas.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	storedSubmoduleArchive, readErr := io.ReadAll(submoduleFile)
	closeErr := submoduleFile.Close()
	if readErr != nil || closeErr != nil || submoduleArtifact.SHA256 != payload.SourceProjectDelta.SubmoduleDeltas.SHA256 || submoduleArtifact.ByteSize != payload.SourceProjectDelta.SubmoduleDeltas.ByteSize || string(storedSubmoduleArchive) != "verified Git submodule archive payload" {
		t.Fatalf("source submodule delta artifact failed read-back: artifact=%+v read=%v close=%v", submoduleArtifact, readErr, closeErr)
	}
	verifyBundle := exec.CommandContext(ctx, git, "bundle", "verify", deltaPath)
	verifyBundle.Dir = repoDir
	if output, err := verifyBundle.CombinedOutput(); err != nil {
		t.Fatalf("durable source project delta is not a readable Git bundle: %v: %s", err, output)
	}
	_, contextFile, err := artifacts.Open(ctx, owner, payload.SourceContextArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	var persistedContext localTaskConversationContext
	if err := json.NewDecoder(contextFile).Decode(&persistedContext); err != nil {
		_ = contextFile.Close()
		t.Fatal(err)
	}
	if err := contextFile.Close(); err != nil {
		t.Fatal(err)
	}
	if len(persistedContext.Conversation.Messages) != len(sourceMessages) || persistedContext.Conversation.Messages[0].Content != sourceMessages[0].Content || persistedContext.Conversation.Messages[1].Content != sourceMessages[1].Content {
		t.Fatalf("source conversation history was not durably snapshotted: %+v", persistedContext.Conversation.Messages)
	}
	if persistedContext.Project == nil || persistedContext.Project.ID != project.ID || persistedContext.Project.ResolvedCommit != project.ResolvedCommit || persistedContext.Project.RemoteRepoURL != project.RemoteRepoURL || persistedContext.Project.MeasuredBytes != project.MeasuredBytes || persistedContext.Project.Workdir != "" {
		t.Fatalf("source project identity was not transferred without a host-specific path: %+v", persistedContext.Project)
	}
	lease := "local-submit-lease"
	leaseDigest := sha256.Sum256([]byte(lease))
	if _, err := store.ClaimExecutionTask(ctx, node.ID, hex.EncodeToString(leaseDigest[:]), now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, node.ID, submitted.Task.ID, hex.EncodeToString(leaseDigest[:]), "accepted", nil, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second := call()
	if second.Code != http.StatusOK {
		t.Fatalf("idempotent retry after node claim = %d: %s", second.Code, second.Body.String())
	}
	var repeated struct {
		Task    storage.ExecutionTask `json:"task"`
		Created bool                  `json:"created"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &repeated); err != nil || repeated.Created || repeated.Task.ID != submitted.Task.ID || repeated.Task.Status != "accepted" {
		t.Fatalf("idempotent retry did not return current durable task state: %+v err=%v", repeated, err)
	}
	if snapshotter.calls != 1 {
		t.Fatalf("idempotent retry regenerated the cloud project snapshot %d times", snapshotter.calls)
	}
	snapshotter.baseOverride = strings.Repeat("f", 40)
	invalidBody, err := json.Marshal(map[string]any{"targetNodeId": node.ID, "content": "reject a snapshot from a moved base"})
	if err != nil {
		t.Fatal(err)
	}
	invalidRequest := httptest.NewRequest(http.MethodPost, "/api/v1/conversations/conversation_local_submit/tasks/nodes", bytes.NewReader(invalidBody))
	invalidRequest.SetPathValue("id", "conversation_local_submit")
	invalidRequest.Header.Set("Idempotency-Key", "local-node-submit-invalid-baseline")
	invalidResponse := httptest.NewRecorder()
	server.nodeAgentTaskSubmit(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusConflict {
		t.Fatalf("mismatched project snapshot baseline was accepted: %d %s", invalidResponse.Code, invalidResponse.Body.String())
	}
	if entries, err := os.ReadDir(deltaDir); err != nil || len(entries) != 0 {
		t.Fatalf("rejected project snapshot bundle was not cleaned up: entries=%v err=%v", entries, err)
	}
	if _, err := store.ExecutionTaskByIdempotencyKey(ctx, owner, "local-node-submit-invalid-baseline"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("mismatched project snapshot left a durable task behind: err=%v", err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, submitted.Task.ID)
	if err != nil || readBack.Status != "accepted" || readBack.NodeID != node.ID {
		t.Fatalf("node claim was not preserved after idempotent retry: %+v err=%v", readBack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedTask, err := restarted.ExecutionTask(ctx, owner, submitted.Task.ID)
	if err != nil || restartedTask.Status != "accepted" {
		t.Fatalf("task state did not survive storage restart: %+v err=%v", restartedTask, err)
	}
	restartedArtifacts, err := restarted.ExecutionTaskArtifacts(ctx, owner, submitted.Task.ID)
	if err != nil || len(restartedArtifacts) != 5 {
		t.Fatalf("source context artifact association did not survive storage restart: %+v err=%v", restartedArtifacts, err)
	}
	restartedRoles := map[string]storage.ExecutionTaskArtifact{}
	for _, artifact := range restartedArtifacts {
		restartedRoles[artifact.Role] = artifact
	}
	if restartedRoles["conversation_context"].ID != payload.SourceContextArtifactID || restartedRoles["conversation_context"].SHA256 != payload.SourceContextSHA256 || restartedRoles["conversation_context"].ByteSize != payload.SourceContextByteSize || restartedRoles["source_project_delta"].ID != payload.SourceProjectDelta.ArtifactID || restartedRoles["source_project_delta"].SHA256 != payload.SourceProjectDelta.SHA256 || restartedRoles["source_project_delta"].ByteSize != payload.SourceProjectDelta.ByteSize {
		t.Fatalf("source context/project delta associations did not survive storage restart: %+v", restartedArtifacts)
	}
	if restartedRoles["source_project_lfs_objects"].ID != payload.SourceProjectDelta.LFSObjects.ArtifactID || restartedRoles["source_project_lfs_objects"].SHA256 != payload.SourceProjectDelta.LFSObjects.SHA256 || restartedRoles["source_project_lfs_objects"].ByteSize != payload.SourceProjectDelta.LFSObjects.ByteSize {
		t.Fatalf("source Git LFS archive association did not survive storage restart: %+v", restartedArtifacts)
	}
	if restartedRoles["source_project_submodule_deltas"].ID != payload.SourceProjectDelta.SubmoduleDeltas.ArtifactID || restartedRoles["source_project_submodule_deltas"].SHA256 != payload.SourceProjectDelta.SubmoduleDeltas.SHA256 || restartedRoles["source_project_submodule_deltas"].ByteSize != payload.SourceProjectDelta.SubmoduleDeltas.ByteSize {
		t.Fatalf("source submodule archive association did not survive storage restart: %+v", restartedArtifacts)
	}
	if restartedRoles["user_input"].ID != inputArtifact.ID || restartedRoles["user_input"].SHA256 != inputArtifact.SHA256 || restartedRoles["user_input"].ByteSize != inputArtifact.ByteSize {
		t.Fatalf("user input attachment association did not survive storage restart: %+v", restartedArtifacts)
	}
	restartedArtifactStore, err := artifactstore.New(dataDir, restarted)
	if err != nil {
		t.Fatal(err)
	}
	restartedContextArtifact, contextFile, err := restartedArtifactStore.Open(ctx, owner, payload.SourceContextArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	var restartedContext localTaskConversationContext
	if err := json.NewDecoder(contextFile).Decode(&restartedContext); err != nil {
		_ = contextFile.Close()
		t.Fatal(err)
	}
	if err := contextFile.Close(); err != nil {
		t.Fatal(err)
	}
	if restartedContextArtifact.SHA256 != payload.SourceContextSHA256 || restartedContext.Project == nil || restartedContext.Project.MeasuredBytes != project.MeasuredBytes {
		t.Fatalf("large local project context did not survive artifact/storage restart: artifact=%+v project=%+v", restartedContextArtifact, restartedContext.Project)
	}
	restartedDelta, restartedDeltaFile, err := restartedArtifactStore.Open(ctx, owner, payload.SourceProjectDelta.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	restartedDigest := sha256.New()
	restartedSize, copyErr := io.Copy(restartedDigest, restartedDeltaFile)
	closeErr = restartedDeltaFile.Close()
	if copyErr != nil || closeErr != nil || restartedDelta.ID != payload.SourceProjectDelta.ArtifactID || restartedDelta.SHA256 != hex.EncodeToString(restartedDigest.Sum(nil)) || restartedDelta.SHA256 != payload.SourceProjectDelta.SHA256 || restartedDelta.ByteSize != restartedSize || restartedDelta.ByteSize != payload.SourceProjectDelta.ByteSize {
		t.Fatalf("project delta artifact bytes did not survive storage restart: artifact=%+v size=%d copyErr=%v closeErr=%v", restartedDelta, restartedSize, copyErr, closeErr)
	}
}

func TestExecutionTaskSafeHandoffHTTPRoutePersistsAndPulseReadsRequest(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	const nodeToken = "http-safe-handoff-node-token"
	capabilities := []string{"agent-runtime", "safe-handoff"}
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_http_handoff", UserID: owner, Name: "Laptop", Platform: "linux", Capabilities: capabilities}, nodeToken); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, nodeToken, "linux", capabilities, now); err != nil {
		t.Fatal(err)
	}
	const taskID = "task_http_handoff"
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: taskID, UserID: owner, NodeID: "node_http_handoff", IdempotencyKey: "http-handoff", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_http_handoff", hashToken("http-handoff-lease"), now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store}
	mux := http.NewServeMux()
	server.executionNodeRoutes(mux)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+taskID+"/handoff", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	var handoff storage.ExecutionTask
	if err := json.Unmarshal(response.Body.Bytes(), &handoff); err != nil || response.Code != http.StatusOK || handoff.ID != taskID || !handoff.HandoffRequested || handoff.Status != "leased" {
		t.Fatalf("safe handoff route did not return durable request state: code=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	pulseRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/tasks/"+taskID+"/pulse", strings.NewReader(`{"progressPhase":"preparing"}`))
	pulseRequest.Header.Set("X-O-Task-Lease", "http-handoff-lease")
	pulseRequest = pulseRequest.WithContext(context.WithValue(pulseRequest.Context(), executionNodeContextKey{}, "node_http_handoff"))
	pulseResponse := httptest.NewRecorder()
	mux.ServeHTTP(pulseResponse, pulseRequest)
	var pulsed storage.ExecutionTask
	if err := json.Unmarshal(pulseResponse.Body.Bytes(), &pulsed); err != nil || pulseResponse.Code != http.StatusOK || !pulsed.HandoffRequested || pulsed.Status != "leased" || pulsed.ProgressPhase != "preparing" {
		t.Fatalf("node HTTP pulse did not read back handoff and durable phase while retaining task ownership: code=%d task=%+v body=%s err=%v", pulseResponse.Code, pulsed, pulseResponse.Body.String(), err)
	}
}
