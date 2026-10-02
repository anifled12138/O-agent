package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/httpapi"
	"axiom.local/agent/internal/storage"
)

func TestRemoveDownloadedTaskArtifactsReportsCleanupFailure(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "artifact.bundle")
	if err := os.WriteFile(file, []byte("downloaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeDownloadedTaskArtifacts(file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("downloaded task artifact remains after successful cleanup: %v", err)
	}
	nonEmptyDir := filepath.Join(root, "non-empty")
	if err := os.Mkdir(nonEmptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(nonEmptyDir, "keep")
	if err := os.WriteFile(child, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeDownloadedTaskArtifacts(nonEmptyDir); err == nil {
		t.Fatal("cleanup failure for a non-empty directory was silently reported as success")
	}
	if content, err := os.ReadFile(child); err != nil || string(content) != "keep" {
		t.Fatalf("failed cleanup damaged remaining data: content=%q err=%v", content, err)
	}
}

func TestNodeDirectArtifactUploadUsesChecksumAndRejectsProviderRedirects(t *testing.T) {
	content := []byte("verified task output")
	digest := sha256.Sum256(content)
	checksum := base64.StdEncoding.EncodeToString(digest[:])
	var received atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("x-amz-checksum-sha256") != checksum {
			http.Error(w, "bad upload contract", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, content) {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer provider.Close()
	client := &nodeAPIClient{http: provider.Client()}
	if err := client.uploadDirectArtifact(context.Background(), provider.URL+"/signed", map[string]string{"x-amz-checksum-sha256": checksum}, int64(len(content)), bytes.NewReader(content)); err != nil {
		t.Fatalf("checksum-bound direct upload failed: %v", err)
	}
	if received.Load() != 1 {
		t.Fatalf("provider did not receive exactly one direct upload: %d", received.Load())
	}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	if err := client.uploadDirectArtifact(context.Background(), redirector.URL+"/signed", map[string]string{"x-amz-checksum-sha256": checksum}, int64(len(content)), bytes.NewReader(content)); err == nil {
		t.Fatal("provider redirect was reported as a successful object upload")
	}
	if redirected.Load() != 0 {
		t.Fatal("direct upload followed a redirect and disclosed the signed checksum header to another host")
	}
	secretQuery := "X-Amz-Signature=opaque-upload-signature"
	rejector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream failed at "+r.URL.String())
	}))
	rejectURL := rejector.URL + "/signed?" + secretQuery
	rejectErr := client.uploadDirectArtifact(context.Background(), rejectURL, map[string]string{"x-amz-checksum-sha256": checksum}, int64(len(content)), bytes.NewReader(content))
	rejector.Close()
	if rejectErr == nil || strings.Contains(rejectErr.Error(), "opaque-upload-signature") || strings.Contains(rejectErr.Error(), "X-Amz-Signature") {
		t.Fatalf("provider error leaked the short-lived signed upload URL: %v", rejectErr)
	}
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachableURL := unreachable.URL + "/signed?" + secretQuery
	unreachable.Close()
	transportErr := client.uploadDirectArtifact(context.Background(), unreachableURL, map[string]string{"x-amz-checksum-sha256": checksum}, int64(len(content)), bytes.NewReader(content))
	if transportErr == nil || strings.Contains(transportErr.Error(), "opaque-upload-signature") || strings.Contains(transportErr.Error(), "X-Amz-Signature") {
		t.Fatalf("transport error leaked the short-lived signed upload URL: %v", transportErr)
	}
}

func TestLocalNodeTranscriptIsUploadedAndTaskBoundBeforeSuccessReport(t *testing.T) {
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
	const credential = "integration-node-credential-0123456789"
	credentialHash := sha256.Sum256([]byte(credential))
	node, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_transcript_integration", UserID: owner, Name: "Local test node", Platform: "linux"}, hex.EncodeToString(credentialHash[:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, hex.EncodeToString(credentialHash[:]), "linux", []string{"agent-runtime"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	sourceContext := []byte(`{"conversation":{"id":"conversation_remote_1","messages":[{"id":"source_msg_1","conversationId":"conversation_remote_1","role":"user","content":"write"}]},"turns":[],"trace":[]}`)
	sourceDigest := sha256.Sum256(sourceContext)
	sourceArtifact, err := artifacts.StoreFromReader(ctx, owner, "conversation-context.json", "application/json", "test-source-context", int64(len(sourceContext)), hex.EncodeToString(sourceDigest[:]), bytes.NewReader(sourceContext), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(localAgentPrompt{Kind: "agent_prompt", SourceConversationID: "conversation_remote_1", Content: "write", SourceContextArtifactID: sourceArtifact.ID, SourceContextSHA256: sourceArtifact.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_transcript_integration", UserID: owner, NodeID: node.ID, IdempotencyKey: "transcript-integration", Payload: payload}, false, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachExecutionTaskArtifact(ctx, owner, task.ID, sourceArtifact.ID, "conversation_context", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(owner, nil, nil, nil, nil, nil, nil, store, nil, "")
	api.SetArtifactStore(artifacts)
	apiHandler := api.Handler()
	var injectedChunkFailure atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/chunks/") && injectedChunkFailure.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusServiceUnavailable)
			if _, err := io.WriteString(w, `{"error":"temporary test outage"}`); err != nil {
				t.Errorf("write injected transient response: %v", err)
			}
			return
		}
		apiHandler.ServeHTTP(w, r)
	}))
	defer server.Close()
	transcript := json.RawMessage(`{"conversation":{"id":"run_local_1","messages":[{"role":"user","content":"write"},{"role":"assistant","content":"paused safely"}]},"turns":[{"id":"turn_local_1","status":"incomplete","stopReason":"step_limit","continuationAvailable":true}],"trace":[{"id":"trace_local_1","turnId":"turn_local_1","kind":"turn.incomplete"}]}`)
	bundleBytes := bytes.Repeat([]byte("x"), int(nodeArtifactChunkSize+1))
	bundlePath := filepath.Join(t.TempDir(), "task-delta.bundle")
	if err := os.WriteFile(bundlePath, bundleBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(localTranscriptEnvelope{SourceConversationID: "conversation_remote_1", ProjectID: "project_shared", ProjectCommit: "0123456789012345678901234567890123456789", ProjectDeltaPath: bundlePath, ProjectDeltaBaseCommit: "0123456789012345678901234567890123456789", ProjectDeltaCommit: "1123456789012345678901234567890123456789", ConversationID: "run_local_1", AgentTurnID: "turn_local_1", ResultMessageID: "message_local_1", AgentStatus: "incomplete", AgentStopReason: "step_limit", AssistantText: "paused safely", Transcript: transcript})
	worker, err := NewNodeWorker(server.URL, credential, "linux", nil, localAgentPreparationProbe{sourceContext: sourceContext, result: input})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if !injectedChunkFailure.Load() {
		t.Fatal("test did not inject the transient artifact chunk failure")
	}
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := readBack.Result
	var readResult struct {
		SourceConversationID string                        `json:"sourceConversationId"`
		ProjectID            string                        `json:"projectId"`
		ProjectCommit        string                        `json:"projectCommit"`
		ProjectDeltaBase     string                        `json:"projectDeltaBaseCommit"`
		ProjectDeltaCommit   string                        `json:"projectDeltaCommit"`
		ConversationID       string                        `json:"conversationId"`
		AssistantText        string                        `json:"assistantText"`
		AgentStatus          string                        `json:"agentStatus"`
		AgentStopReason      string                        `json:"agentStopReason"`
		TranscriptArtifact   storage.ExecutionTaskArtifact `json:"transcriptArtifact"`
		Transcript           json.RawMessage               `json:"transcript"`
		ProjectDeltaArtifact storage.ExecutionTaskArtifact `json:"projectDeltaArtifact"`
	}
	if err := json.Unmarshal(result, &readResult); err != nil {
		t.Fatal(err)
	}
	if readResult.SourceConversationID != "conversation_remote_1" || readResult.ProjectID != "project_shared" || readResult.ProjectCommit != "0123456789012345678901234567890123456789" || readResult.ProjectDeltaBase != "0123456789012345678901234567890123456789" || readResult.ProjectDeltaCommit != "1123456789012345678901234567890123456789" || readResult.ProjectDeltaArtifact.ID == "" || readResult.ProjectDeltaArtifact.ByteSize != int64(len(bundleBytes)) || readResult.ConversationID != "run_local_1" || readResult.AssistantText != "paused safely" || readResult.AgentStatus != "incomplete" || readResult.AgentStopReason != "step_limit" || len(readResult.Transcript) != 0 || readResult.TranscriptArtifact.ID == "" {
		t.Fatalf("result did not replace transcript with a cloud artifact reference: %s", result)
	}
	items, err := store.ExecutionTaskArtifacts(ctx, owner, task.ID)
	if err != nil || len(items) != 3 {
		t.Fatalf("durable task context/transcript/project artifact associations missing: items=%+v err=%v", items, err)
	}
	var contextFound, transcriptFound, projectDeltaFound bool
	for _, item := range items {
		if item.ID == sourceArtifact.ID && item.Role == "conversation_context" && item.SHA256 == sourceArtifact.SHA256 {
			contextFound = true
		}
		if item.ID == readResult.TranscriptArtifact.ID && item.Role == "conversation_transcript" {
			transcriptFound = true
		}
		if item.ID == readResult.ProjectDeltaArtifact.ID && item.Role == "project_delta" {
			projectDeltaFound = true
		}
	}
	if !contextFound || !transcriptFound || !projectDeltaFound {
		t.Fatalf("context, transcript, or project delta artifact association missing: %+v", items)
	}
	if _, err := os.Stat(bundlePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uploaded project delta was not removed after cloud read-back: stat error=%v", err)
	}
	_, transcriptFile, err := artifacts.Open(ctx, owner, readResult.TranscriptArtifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	var storedTranscript strings.Builder
	if _, err := io.Copy(&storedTranscript, transcriptFile); err != nil {
		_ = transcriptFile.Close()
		t.Fatal(err)
	}
	if err := transcriptFile.Close(); err != nil {
		t.Fatal(err)
	}
	if storedTranscript.String() != string(transcript) {
		t.Fatalf("cloud transcript mismatch: %q", storedTranscript.String())
	}
	_, projectDeltaFile, err := artifacts.Open(ctx, owner, readResult.ProjectDeltaArtifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedDelta, err := io.ReadAll(projectDeltaFile)
	if closeErr := projectDeltaFile.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !bytes.Equal(storedDelta, bundleBytes) {
		t.Fatalf("cloud project delta mismatch: bytes=%q err=%v", storedDelta, err)
	}
	var archived struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
		Turns []struct {
			ID string `json:"id"`
		} `json:"turns"`
		Trace []struct {
			ID string `json:"id"`
		} `json:"trace"`
	}
	if err := json.Unmarshal([]byte(storedTranscript.String()), &archived); err != nil || archived.Conversation.ID != "run_local_1" || len(archived.Turns) != 1 || archived.Turns[0].ID != "turn_local_1" || len(archived.Trace) != 1 || archived.Trace[0].ID != "trace_local_1" {
		t.Fatalf("uploaded transcript lost conversation/turn/trace records: %+v err=%v", archived, err)
	}
	if readBack.Status != "reported_succeeded" || string(readBack.Result) != string(result) || readBack.LeaseUntil != nil {
		t.Fatalf("task success did not follow verified transcript persistence: task=%+v err=%v", readBack, err)
	}
}

func TestLocalNodeOutboxRecoversResultAfterWorkerRestart(t *testing.T) {
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
	const credential = "restart-node-credential-0123456789"
	credentialHashBytes := sha256.Sum256([]byte(credential))
	credentialHash := hex.EncodeToString(credentialHashBytes[:])
	node, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_outbox_restart", UserID: owner, Name: "Restart test node", Platform: "linux"}, credentialHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, credentialHash, "linux", []string{"agent-runtime"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	serverAPI := httpapi.New(owner, nil, nil, nil, nil, nil, nil, store, nil, "")
	serverAPI.SetArtifactStore(artifacts)
	server := httptest.NewServer(serverAPI.Handler())
	defer server.Close()
	now := time.Now().UTC()
	task, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_outbox_restart", UserID: owner, NodeID: node.ID, IdempotencyKey: "restart-outbox", Payload: json.RawMessage(`{"kind":"agent_prompt"}`)}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	const lease = "restart-outbox-lease-token"
	leaseHashBytes := sha256.Sum256([]byte(lease))
	claimed, err := store.ClaimExecutionTask(ctx, node.ID, hex.EncodeToString(leaseHashBytes[:]), now.Add(time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"accepted", "running"} {
		if _, err := store.ReportExecutionTask(ctx, node.ID, task.ID, hex.EncodeToString(leaseHashBytes[:]), status, nil, "", time.Now().UTC()); err != nil {
			t.Fatalf("record pre-crash task status %q: %v", status, err)
		}
	}
	transcript := json.RawMessage(`{"conversation":{"id":"restart-run"},"turns":[],"trace":[]}`)
	bundleBytes := []byte("durable project delta survives node process restart")
	bundlePath := filepath.Join(t.TempDir(), "restart-project.bundle")
	if err := os.WriteFile(bundlePath, bundleBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	lfsBytes := []byte("LFS archive survives node process restart")
	lfsPath := filepath.Join(t.TempDir(), "restart-project-lfs.tar")
	if err := os.WriteFile(lfsPath, lfsBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	submoduleBytes := []byte("submodule changes survive node process restart")
	submodulePath := filepath.Join(t.TempDir(), "restart-project-submodules.tar")
	if err := os.WriteFile(submodulePath, submoduleBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(localTranscriptEnvelope{SourceConversationID: "source-run", ProjectID: "project-restart", ProjectCommit: "0123456789012345678901234567890123456789", ProjectDeltaPath: bundlePath, ProjectLFSPath: lfsPath, ProjectSubmoduleDeltasPath: submodulePath, ProjectDeltaBaseCommit: "0123456789012345678901234567890123456789", ProjectDeltaCommit: "1123456789012345678901234567890123456789", ConversationID: "restart-run", AgentTurnID: "turn-restart", ResultMessageID: "message-restart", AgentStatus: "completed", AssistantText: "survived restart", Transcript: transcript})
	if err != nil {
		t.Fatal(err)
	}
	outboxPath := filepath.Join(t.TempDir(), "node-task-outbox")
	firstProcessOutbox, err := newNodeTaskOutbox(outboxPath)
	if err != nil {
		t.Fatal(err)
	}
	persistedRecord, err := firstProcessOutbox.Save(task.ID, lease, result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bundlePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source project delta was not replaced by its durable outbox copy: %v", err)
	}
	var persistedEnvelope localTranscriptEnvelope
	if err := json.Unmarshal(persistedRecord.Result, &persistedEnvelope); err != nil {
		t.Fatal(err)
	}
	if persistedEnvelope.ProjectDeltaPath == "" {
		t.Fatal("durable outbox result lost its project delta path")
	}
	if copied, err := os.ReadFile(persistedEnvelope.ProjectDeltaPath); err != nil || !bytes.Equal(copied, bundleBytes) {
		t.Fatalf("project delta was not durably copied into the outbox: bytes=%q err=%v", copied, err)
	}
	if _, err := os.Lstat(lfsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source Git LFS archive was not replaced by its durable outbox copy: %v", err)
	}
	if persistedEnvelope.ProjectLFSPath == "" {
		t.Fatal("durable outbox result lost its Git LFS archive path")
	}
	if copied, err := os.ReadFile(persistedEnvelope.ProjectLFSPath); err != nil || !bytes.Equal(copied, lfsBytes) {
		t.Fatalf("Git LFS archive was not durably copied into the outbox: bytes=%q err=%v", copied, err)
	}
	if _, err := os.Lstat(submodulePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source submodule archive was not replaced by its durable outbox copy: %v", err)
	}
	if persistedEnvelope.ProjectSubmoduleDeltasPath == "" {
		t.Fatal("durable outbox result lost its submodule delta archive path")
	}
	if copied, err := os.ReadFile(persistedEnvelope.ProjectSubmoduleDeltasPath); err != nil || !bytes.Equal(copied, submoduleBytes) {
		t.Fatalf("submodule archive was not durably copied into the outbox: bytes=%q err=%v", copied, err)
	}
	// Simulate process loss by reopening the persisted outbox with a fresh worker.
	restartedWorker, err := NewNodeWorker(server.URL, credential, "linux", nil, nodeWorkerExecutorFunc(func(context.Context, storage.ExecutionTask) (json.RawMessage, error) {
		return nil, errors.New("recovery must use the saved result, not execute the task again")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedWorker.SetOutboxDirectory(outboxPath); err != nil {
		t.Fatal(err)
	}
	if err := restartedWorker.recoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if readBack.Status != "reported_succeeded" || readBack.LeaseUntil != nil {
		t.Fatalf("recovered task did not reach durable cloud success: %+v", readBack)
	}
	var cloudResult struct {
		AssistantText      string                        `json:"assistantText"`
		TranscriptArtifact storage.ExecutionTaskArtifact `json:"transcriptArtifact"`
		ProjectDelta       storage.ExecutionTaskArtifact `json:"projectDeltaArtifact"`
		ProjectSubmodules  storage.ExecutionTaskArtifact `json:"projectSubmoduleDeltasArtifact"`
	}
	if err := json.Unmarshal(readBack.Result, &cloudResult); err != nil || cloudResult.AssistantText != "survived restart" || cloudResult.TranscriptArtifact.ID == "" {
		t.Fatalf("cloud result does not contain the restored transcript reference: %+v err=%v", cloudResult, err)
	}
	if cloudResult.ProjectDelta.ID == "" || cloudResult.ProjectDelta.ByteSize != int64(len(bundleBytes)) {
		t.Fatalf("cloud result does not contain the restored project delta reference: %+v", cloudResult.ProjectDelta)
	}
	if cloudResult.ProjectSubmodules.ID == "" || cloudResult.ProjectSubmodules.ByteSize != int64(len(submoduleBytes)) {
		t.Fatalf("cloud result does not contain the restored submodule delta reference: %+v", cloudResult.ProjectSubmodules)
	}
	_, file, err := artifacts.Open(ctx, owner, cloudResult.TranscriptArtifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedTranscript, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(storedTranscript, transcript) {
		t.Fatalf("recovered transcript did not persist to cloud artifact storage: read=%v close=%v", readErr, closeErr)
	}
	_, file, err = artifacts.Open(ctx, owner, cloudResult.ProjectDelta.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedDelta, readErr := io.ReadAll(file)
	closeErr = file.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(storedDelta, bundleBytes) {
		t.Fatalf("recovered project delta did not persist to cloud artifact storage: read=%v close=%v", readErr, closeErr)
	}
	_, file, err = artifacts.Open(ctx, owner, cloudResult.ProjectSubmodules.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedSubmodules, readErr := io.ReadAll(file)
	closeErr = file.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(storedSubmodules, submoduleBytes) {
		t.Fatalf("recovered submodule delta did not persist to cloud artifact storage: read=%v close=%v", readErr, closeErr)
	}
	remaining, err := restartedWorker.outbox.List()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("verified successful recovery did not remove local outbox entry: remaining=%+v err=%v", remaining, err)
	}
	status, err := restartedWorker.client.taskStatus(ctx, task.ID)
	if err != nil || status.Status != "reported_succeeded" || !bytes.Equal(status.Result, readBack.Result) {
		t.Fatalf("node-scoped cloud status did not read back the recovered result: status=%+v err=%v", status, err)
	}
	_ = claimed
}

func TestLocalNodeOutboxRetainsResultWhenCloudLeaseExpired(t *testing.T) {
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
	const credential = "expired-node-credential-0123456789"
	credentialDigest := sha256.Sum256([]byte(credential))
	credentialHash := hex.EncodeToString(credentialDigest[:])
	node, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_outbox_expired", UserID: owner, Name: "Expired lease test node", Platform: "linux"}, credentialHash)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.HeartbeatExecutionNode(ctx, credentialHash, "linux", []string{"agent-runtime"}, now); err != nil {
		t.Fatal(err)
	}
	task, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_outbox_expired", UserID: owner, NodeID: node.ID, IdempotencyKey: "expired-outbox", Payload: json.RawMessage(`{"kind":"agent_prompt"}`)}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	const lease = "expired-outbox-lease-token"
	leaseDigest := sha256.Sum256([]byte(lease))
	leaseHash := hex.EncodeToString(leaseDigest[:])
	if _, err := store.ClaimExecutionTask(ctx, node.ID, leaseHash, now.Add(time.Second), now); err != nil {
		t.Fatal(err)
	}
	for index, status := range []string{"accepted", "running"} {
		if _, err := store.ReportExecutionTask(ctx, node.ID, task.ID, leaseHash, status, nil, "", now.Add(time.Duration(index+1)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	artifacts, err := artifactstore.New(dataDir, store)
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(owner, nil, nil, nil, nil, nil, nil, store, nil, "")
	api.SetArtifactStore(artifacts)
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	outboxPath := filepath.Join(t.TempDir(), "node-task-outbox")
	outbox, err := newNodeTaskOutbox(outboxPath)
	if err != nil {
		t.Fatal(err)
	}
	transcript := json.RawMessage(`{"conversation":{"id":"offline-run"},"turns":[],"trace":[]}`)
	localResult, err := json.Marshal(localTranscriptEnvelope{SourceConversationID: "source-run", ConversationID: "offline-run", AgentTurnID: "offline-turn", ResultMessageID: "offline-message", AgentStatus: "completed", AssistantText: "saved before upload failure", Transcript: transcript})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Save(task.ID, lease, localResult); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileExpiredExecutionTaskLeases(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	var executed atomic.Bool
	worker, err := NewNodeWorker(server.URL, credential, "linux", nil, nodeWorkerExecutorFunc(func(context.Context, storage.ExecutionTask) (json.RawMessage, error) {
		executed.Store(true)
		return json.RawMessage(`{"should":"not run"}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.SetOutboxDirectory(outboxPath); err != nil {
		t.Fatal(err)
	}
	if err := worker.recoverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if executed.Load() {
		t.Fatal("expired task lease was used to replay a local result")
	}
	remaining, err := worker.outbox.List()
	if err != nil || len(remaining) != 1 || remaining[0].TaskID != task.ID || !bytes.Equal(remaining[0].Result, localResult) || !remaining[0].hasUploadedResult() {
		t.Fatalf("expired task result was not retained for reconciliation: remaining=%+v err=%v", remaining, err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || readBack.Status != "needs_reconciliation" || readBack.LeaseUntil != nil || readBack.Sequence != 7 || len(readBack.Result) != 0 || len(readBack.RecoveryResult) == 0 {
		t.Fatalf("expired task did not remain quarantined in cloud: task=%+v err=%v", readBack, err)
	}
	items, err := store.ExecutionTaskArtifacts(ctx, owner, task.ID)
	if err != nil || len(items) != 1 || items[0].Role != "recovery_transcript" || items[0].ByteSize != int64(len(transcript)) {
		t.Fatalf("recovery transcript artifact was not attached to the quarantined task: items=%+v err=%v", items, err)
	}
	_, file, err := artifacts.Open(ctx, owner, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(stored, transcript) {
		t.Fatalf("recovery transcript content failed cloud readback: read=%v close=%v", readErr, closeErr)
	}
	if executed.Load() {
		t.Fatal("recovery evidence upload executed or replayed the task")
	}
}

type localAgentPreparationProbe struct {
	sourceContext []byte
	result        json.RawMessage
}

func (p localAgentPreparationProbe) PrepareNodeTask(ctx context.Context, task storage.ExecutionTask, client *nodeAPIClient, lease string) (storage.ExecutionTask, error) {
	return (&LocalAgentExecutor{}).PrepareNodeTask(ctx, task, client, lease)
}

func (p localAgentPreparationProbe) ExecuteNodeTask(_ context.Context, task storage.ExecutionTask) (json.RawMessage, error) {
	var input localAgentPrompt
	if err := json.Unmarshal(task.Payload, &input); err != nil || input.SourceContextFilePath == "" {
		return nil, errors.New("source conversation context was not prepared for local execution")
	}
	defer os.Remove(input.SourceContextFilePath)
	actual, err := os.ReadFile(input.SourceContextFilePath)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(actual, p.sourceContext) {
		return nil, errors.New("downloaded source context did not match its cloud snapshot")
	}
	return p.result, nil
}
