package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/storage"
)

type concurrencyProbeRuntime struct {
	entered chan struct{}
	release chan struct{}
}

type continuationProbeRuntime struct{ called atomic.Int32 }

type executionTaskIdentityProbe struct{ called atomic.Int32 }

type incompleteTurnRuntime struct {
	turn    domain.AgentTurn
	message domain.Message
}

func (r incompleteTurnRuntime) ExecuteTaskTurn(_ context.Context, userID, conversationID, _ string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	return domain.TurnReceipt{TurnID: r.turn.ID, ConversationID: conversationID, Status: r.turn.Status}, r.turn, r.message, nil
}

func (r *continuationProbeRuntime) ExecuteTaskTurn(context.Context, string, string, string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, errors.New("ordinary turn path should not be used")
}

func (r *continuationProbeRuntime) ExecuteTaskContinuation(_ context.Context, userID, turnID, content, idempotencyKey string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	r.called.Add(1)
	if userID != "owner" || turnID != "source_turn" || content != "continue safely" || idempotencyKey != "handoff-key" {
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, domain.ErrConflict
	}
	return domain.TurnReceipt{TurnID: "continued_turn", Status: "completed"}, domain.AgentTurn{ID: "continued_turn", Status: "completed"}, domain.Message{ID: "result"}, nil
}

func (*executionTaskIdentityProbe) ExecuteTaskTurn(context.Context, string, string, string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, errors.New("task-aware runtime path should be used")
}

func (r *executionTaskIdentityProbe) ExecuteExecutionTaskTurn(_ context.Context, userID, executionTaskID, conversationID, content string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	r.called.Add(1)
	if userID != "owner" || executionTaskID != "task_workspace_identity" || conversationID != "conversation" || content != "work in task workspace" {
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, domain.ErrConflict
	}
	turn := domain.AgentTurn{ID: "turn_task_workspace", ConversationID: conversationID, UserID: userID, Status: "completed"}
	return domain.TurnReceipt{TurnID: turn.ID, ConversationID: conversationID, Status: turn.Status}, turn, domain.Message{ID: "message_task_workspace"}, nil
}

func (r *executionTaskIdentityProbe) ExecuteExecutionTaskContinuation(_ context.Context, userID, executionTaskID, turnID, content, idempotencyKey string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	r.called.Add(1)
	if userID != "owner" || executionTaskID != "task_workspace_identity" || turnID != "source_turn" || content != "continue safely" || idempotencyKey != "handoff-key" {
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, domain.ErrConflict
	}
	turn := domain.AgentTurn{ID: "turn_task_continuation", ConversationID: "conversation", UserID: userID, Status: "completed"}
	return domain.TurnReceipt{TurnID: turn.ID, ConversationID: turn.ConversationID, Status: turn.Status}, turn, domain.Message{ID: "message_task_continuation"}, nil
}

func TestCloudWorkerPassesDurableTaskIdentityToTaskAwareAgent(t *testing.T) {
	runtime := &executionTaskIdentityProbe{}
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	worker, err := NewCloudWorker(store, runtime, "owner", "cloud-node")
	if err != nil {
		t.Fatal(err)
	}
	ordinary := cloudTaskPayload{Kind: "agent_turn", ConversationID: "conversation", Content: "work in task workspace"}
	ordinaryReceipt, ordinaryTurn, ordinaryMessage, err := worker.executeTaskTurn(context.Background(), "task_workspace_identity", ordinary)
	if err != nil || ordinaryReceipt.TurnID != "turn_task_workspace" || ordinaryTurn.Status != "completed" || ordinaryMessage.ID != "message_task_workspace" {
		t.Fatalf("task-aware Agent turn receipt=%+v turn=%+v message=%+v err=%v", ordinaryReceipt, ordinaryTurn, ordinaryMessage, err)
	}
	continuation := cloudTaskPayload{Kind: "agent_continuation", ConversationID: "conversation", TurnID: "source_turn", Content: "continue safely", IdempotencyKey: "handoff-key"}
	continuationReceipt, continuationTurn, continuationMessage, err := worker.executeTaskTurn(context.Background(), "task_workspace_identity", continuation)
	if err != nil || continuationReceipt.TurnID != "turn_task_continuation" || continuationTurn.Status != "completed" || continuationMessage.ID != "message_task_continuation" || runtime.called.Load() != 2 {
		t.Fatalf("task-aware continuation receipt=%+v turn=%+v message=%+v calls=%d err=%v", continuationReceipt, continuationTurn, continuationMessage, runtime.called.Load(), err)
	}
}

func TestCloudWorkerDispatchesDurableContinuationPayload(t *testing.T) {
	runtime := &continuationProbeRuntime{}
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	worker, err := NewCloudWorker(store, runtime, "owner", "cloud-node")
	if err != nil {
		t.Fatal(err)
	}
	input := cloudTaskPayload{Kind: "agent_continuation", ConversationID: "conversation", TurnID: "source_turn", Content: "continue safely", IdempotencyKey: "handoff-key"}
	if !validCloudTaskPayload(input) {
		t.Fatal("valid continuation task payload was rejected")
	}
	receipt, turn, message, err := worker.executeTaskTurn(context.Background(), "task_handoff_test", input)
	if err != nil || runtime.called.Load() != 1 || receipt.TurnID != "continued_turn" || turn.Status != "completed" || message.ID != "result" {
		t.Fatalf("continuation dispatch receipt=%+v turn=%+v message=%+v calls=%d err=%v", receipt, turn, message, runtime.called.Load(), err)
	}
}

func TestCloudWorkerPersistsSafeBudgetPauseWithoutReportingFailureOrCompletion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	provider := domain.Provider{ID: "prv_cloud_pause", UserID: owner, Name: "Cloud pause", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake"}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := domain.Conversation{ID: "run_cloud_pause", UserID: owner, Title: "Cloud pause", ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	node, _, err := store.EnsureCloudExecutionNode(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	input := domain.Message{ID: "msg_cloud_pause_input", ConversationID: conversation.ID, Role: "user", Content: "continue safely", CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_cloud_pause", ConversationID: conversation.ID, ProviderID: conversation.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, owner, turn, input, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	message := domain.Message{ID: "msg_cloud_pause_result", ConversationID: conversation.ID, Role: "assistant", Content: "Paused at a safe budget boundary.", CreatedAt: now.Add(time.Second)}
	checkpoint := &storage.ContinuationState{Version: 1, Ciphertext: []byte("encrypted-checkpoint"), Nonce: []byte("nonce"), ContentHash: "checkpoint-hash"}
	if err := store.FinishAgentTurnWithContinuation(ctx, owner, turn.ID, "incomplete", "model_call_limit", &message, json.RawMessage(`{}`), checkpoint); err != nil {
		t.Fatal(err)
	}
	turn, err = store.AgentTurn(ctx, owner, turn.ID)
	if err != nil || turn.Status != "incomplete" || !turn.ContinuationAvailable {
		t.Fatalf("safe continuation checkpoint was not available: turn=%+v err=%v", turn, err)
	}
	payload, err := json.Marshal(cloudTaskPayload{Kind: "agent_turn", ConversationID: conversation.ID, Content: "continue safely"})
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_cloud_pause", UserID: owner, NodeID: node.ID, IdempotencyKey: "cloud-pause", Payload: payload}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewCloudWorker(store, incompleteTurnRuntime{turn: turn, message: message}, owner, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || readBack.Status != "incomplete" || readBack.Error != "" || readBack.LeaseUntil != nil {
		t.Fatalf("safe budget stop was mapped to success/failure instead of incomplete: task=%+v err=%v", readBack, err)
	}
	var result map[string]string
	if err := json.Unmarshal(readBack.Result, &result); err != nil || result["agentStatus"] != "incomplete" || result["agentStopReason"] != "model_call_limit" || result["continuationStatus"] != "available" {
		t.Fatalf("cloud task lost its resumable Agent state: result=%s err=%v", readBack.Result, err)
	}
	events, err := store.ExecutionTaskEvents(ctx, owner, task.ID)
	if err != nil || events[len(events)-1].Kind != "agent_turn.incomplete" {
		t.Fatalf("cloud task journal did not expose safe pause: events=%+v err=%v", events, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = nil
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	readBack, err = store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || readBack.Status != "incomplete" || string(readBack.Result) == "" {
		t.Fatalf("safe cloud pause was not durable across restart: task=%+v err=%v", readBack, err)
	}
}

func TestCloudTaskPayloadValidationRequiresContinuationIdentity(t *testing.T) {
	for _, input := range []cloudTaskPayload{
		{Kind: "agent_turn", ConversationID: "conversation", Content: "run"},
		{Kind: "agent_continuation", ConversationID: "conversation", TurnID: "turn", Content: "continue", IdempotencyKey: "key"},
	} {
		if !validCloudTaskPayload(input) {
			t.Errorf("valid cloud task payload was rejected: %+v", input)
		}
	}
	for _, input := range []cloudTaskPayload{
		{Kind: "agent_turn", ConversationID: "conversation"},
		{Kind: "agent_continuation", ConversationID: "conversation", TurnID: "turn", Content: "continue"},
		{Kind: "unknown", ConversationID: "conversation", Content: "run"},
	} {
		if validCloudTaskPayload(input) {
			t.Errorf("invalid cloud task payload was accepted: %+v", input)
		}
	}
}

func (r concurrencyProbeRuntime) ExecuteTaskTurn(ctx context.Context, userID, conversationID, content string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	r.entered <- struct{}{}
	select {
	case <-r.release:
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, errors.New("probe turn ended without completion")
	case <-ctx.Done():
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, ctx.Err()
	}
}

func TestCloudWorkerRunsConfiguredConcurrentTasksAndPersistsOutcomes(t *testing.T) {
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
	for i := range 2 {
		payload, _ := json.Marshal(cloudTaskPayload{Kind: "agent_turn", ConversationID: fmt.Sprintf("conversation_parallel_%d", i), Content: "run"})
		if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: fmt.Sprintf("task_parallel_%d", i), UserID: owner, NodeID: node.ID, IdempotencyKey: fmt.Sprintf("parallel-%d", i), Payload: payload}, false, now); err != nil {
			t.Fatal(err)
		}
	}
	runtime := concurrencyProbeRuntime{entered: make(chan struct{}, 2), release: make(chan struct{})}
	worker, err := NewCloudWorker(store, runtime, owner, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	var availableSlots atomic.Int32
	availableSlots.Store(1)
	worker.capacity = func() (int, error) { return int(availableSlots.Load()), nil }
	worker.loopLimit = 2
	worker.pulseInterval = 10 * time.Millisecond
	if err := worker.SetConcurrency(2); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { worker.Run(workerCtx); close(done) }()
	select {
	case <-runtime.entered:
	case <-time.After(2 * time.Second):
		close(runtime.release)
		cancel()
		t.Fatal("the first task was not admitted")
	}
	deadline := time.Now().Add(2 * time.Second)
	queueVerified := false
	for time.Now().Before(deadline) {
		first, firstErr := store.ExecutionTask(ctx, owner, "task_parallel_0")
		second, secondErr := store.ExecutionTask(ctx, owner, "task_parallel_1")
		if firstErr == nil && secondErr == nil && (first.Status == "running" || second.Status == "running") {
			other := first
			if first.Status == "running" {
				other = second
			}
			if other.Status != "queued" {
				close(runtime.release)
				cancel()
				t.Fatalf("second task escaped resource admission while capacity was one: statuses=%s,%s", first.Status, second.Status)
			}
			queueVerified = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !queueVerified {
		close(runtime.release)
		cancel()
		t.Fatal("could not read back one running task and one queued task under single-slot capacity")
	}
	availableSlots.Store(2)
	select {
	case <-runtime.entered:
	case <-time.After(2 * time.Second):
		close(runtime.release)
		cancel()
		t.Fatal("queued task was not admitted after host capacity increased")
	}
	close(runtime.release)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		first, firstErr := store.ExecutionTask(ctx, owner, "task_parallel_0")
		second, secondErr := store.ExecutionTask(ctx, owner, "task_parallel_1")
		if firstErr == nil && secondErr == nil && first.Status == "reported_failed" && second.Status == "reported_failed" {
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("worker did not persist both task outcomes")
}

func TestCloudWorkerReconcilesExpiredLocalLeaseWithoutNodeReconnect(t *testing.T) {
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
	cloudNode, _, err := store.EnsureCloudExecutionNode(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	localNode, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_disconnected", UserID: owner, Name: "Offline laptop", Platform: "linux"}, "node-disconnected-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "node-disconnected-token", "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(cloudTaskPayload{Kind: "agent_turn", ConversationID: "conversation_disconnected", Content: "run locally"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_disconnected_lease", UserID: owner, NodeID: localNode.ID, IdempotencyKey: "disconnected-lease", Payload: payload}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, localNode.ID, "expired-lease-hash", now.Add(-time.Second), now); err != nil {
		t.Fatal(err)
	}
	worker, err := NewCloudWorker(store, incompleteTurnRuntime{}, owner, cloudNode.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Keep every execution slot unavailable: lease recovery must run as
	// independent maintenance and must not depend on task admission.
	worker.capacity = func() (int, error) { return 0, nil }
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { worker.Run(workerCtx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		readBack, readErr := store.ExecutionTask(ctx, owner, "task_disconnected_lease")
		if readErr == nil && readBack.Status == "needs_reconciliation" {
			if readBack.LeaseUntil != nil || readBack.Sequence != 3 {
				t.Fatalf("reconciled task retained lease or wrong sequence: %+v", readBack)
			}
			events, eventErr := store.ExecutionTaskEvents(ctx, owner, readBack.ID)
			if eventErr != nil || len(events) != 3 || events[2].Kind != "needs_reconciliation" {
				t.Fatalf("reconciliation event not durable: events=%+v err=%v", events, eventErr)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	readBack, readErr := store.ExecutionTask(ctx, owner, "task_disconnected_lease")
	t.Fatalf("cloud maintenance did not reconcile disconnected node lease: task=%+v err=%v", readBack, readErr)
}

func TestCloudWorkerConcurrencyRejectsUnsupportedLimits(t *testing.T) {
	worker := &CloudWorker{}
	for _, limit := range []int{0, domain.MaxCloudWorkerConcurrency + 1} {
		if err := worker.SetConcurrency(limit); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("SetConcurrency(%d) err=%v, want invalid", limit, err)
		}
	}
	if err := worker.SetConcurrency(domain.MaxCloudWorkerConcurrency); err != nil {
		t.Fatalf("SetConcurrency(max=%d): %v", domain.MaxCloudWorkerConcurrency, err)
	}
}

type unresolvedOnCancelRuntime struct{}

func (unresolvedOnCancelRuntime) ExecuteTaskTurn(ctx context.Context, userID, conversationID, content string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	<-ctx.Done()
	return domain.TurnReceipt{TurnID: "turn_cancel_uncertain", ConversationID: conversationID}, domain.AgentTurn{ID: "turn_cancel_uncertain", ConversationID: conversationID, UserID: userID, Status: "needs_reconciliation"}, domain.Message{}, errors.New("external tool effect could not be confirmed")
}

func TestCloudWorkerCancellationPreservesNeedsReconciliation(t *testing.T) {
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
	payload, _ := json.Marshal(cloudTaskPayload{Kind: "agent_turn", ConversationID: "conversation_cancel_uncertain", Content: "perform an external change"})
	task, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_cancel_uncertain", UserID: owner, NodeID: node.ID, IdempotencyKey: "cancel-uncertain", Payload: payload}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewCloudWorker(store, unresolvedOnCancelRuntime{}, owner, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker.pulseInterval = time.Millisecond
	cancelCtx, stop := context.WithCancel(ctx)
	defer stop()
	cancelDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			current, readErr := store.ExecutionTask(ctx, owner, task.ID)
			if readErr == nil && current.Status == "running" {
				_, cancelErr := store.CancelExecutionTask(ctx, owner, task.ID, time.Now().UTC())
				cancelDone <- cancelErr
				return
			}
			time.Sleep(time.Millisecond)
		}
		cancelDone <- errors.New("task did not enter running state")
	}()
	if err := worker.RunOne(cancelCtx); err != nil {
		t.Fatalf("worker should persist the reconciliation state: %v", err)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || readBack.Status != "needs_reconciliation" || !readBack.CancelRequested {
		t.Fatalf("cancel request concealed uncertain external effect: task=%+v err=%v", readBack, err)
	}
}

type completedTurnRuntime struct {
	store        *storage.Store
	userID       string
	conversation domain.Conversation
	generation   domain.AgentGeneration
}

func (r completedTurnRuntime) ExecuteTaskTurn(ctx context.Context, userID, conversationID, content string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	if userID != r.userID || conversationID != r.conversation.ID || content != "implement the feature" {
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	input := domain.Message{ID: "msg_cloud_worker_input", ConversationID: conversationID, Role: "user", Content: content, CreatedAt: now}
	turn := domain.AgentTurn{ID: "turn_cloud_worker_result", ConversationID: conversationID, ProviderID: r.conversation.ProviderID, AgentGenerationID: r.generation.ID, AgentDefinitionDigest: r.generation.DefinitionDigest, StartedAt: now, UpdatedAt: now}
	if err := r.store.StartAgentTurn(ctx, userID, turn, input, []byte(`{}`)); err != nil {
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, err
	}
	message := domain.Message{ID: "msg_cloud_worker_output", ConversationID: conversationID, Role: "assistant", Content: "Implementation finished", CreatedAt: now.Add(time.Second)}
	if err := r.store.FinishAgentTurn(ctx, userID, turn.ID, "completed", "assistant_response", &message, []byte(`{}`)); err != nil {
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, err
	}
	readBack, err := r.store.AgentTurn(ctx, userID, turn.ID)
	if err != nil {
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, err
	}
	return domain.TurnReceipt{TurnID: turn.ID, ConversationID: conversationID, Status: "running"}, readBack, message, nil
}

func (r completedTurnRuntime) ExecuteExecutionTaskTurn(ctx context.Context, userID, executionTaskID, conversationID, content string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	if executionTaskID != "task_cloud_worker" {
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, fmt.Errorf("unexpected durable execution task ID %q", executionTaskID)
	}
	return r.ExecuteTaskTurn(ctx, userID, conversationID, content)
}

func TestCloudWorkerCompletesTaskOnlyAfterAgentTurnReadback(t *testing.T) {
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
	provider := domain.Provider{ID: "prv_cloud_worker", UserID: owner, Name: "Cloud Worker", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake"}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := domain.Conversation{ID: "run_cloud_worker", UserID: owner, Title: "Cloud task", ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	node, _, err := store.EnsureCloudExecutionNode(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(cloudTaskPayload{Kind: "agent_turn", ConversationID: conversation.ID, Content: "implement the feature"})
	task, created, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_cloud_worker", UserID: owner, NodeID: node.ID, IdempotencyKey: "cloud-worker-1", Payload: payload}, false, now)
	if err != nil || !created {
		t.Fatalf("task creation: task=%+v created=%v err=%v", task, created, err)
	}
	worker, err := NewCloudWorker(store, completedTurnRuntime{store: store, userID: owner, conversation: conversation, generation: generation}, owner, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || readBack.Status != "completed" || readBack.LeaseUntil != nil {
		t.Fatalf("Agent task did not reach verified terminal state: task=%+v err=%v", readBack, err)
	}
	var result map[string]string
	if err := json.Unmarshal(readBack.Result, &result); err != nil || result["agentTurnId"] != "turn_cloud_worker_result" || result["agentStatus"] != "completed" {
		t.Fatalf("task result lacks Agent terminal readback: result=%s err=%v", readBack.Result, err)
	}
	events, err := store.ExecutionTaskEvents(ctx, owner, task.ID)
	if err != nil || len(events) != 5 || events[len(events)-1].Kind != "agent_turn.verified" {
		t.Fatalf("task event journal does not prove the downstream Agent effect: events=%+v err=%v", events, err)
	}
}

func TestCloudWorkerDoesNotReplayAClaimedTaskAfterLeaseLoss(t *testing.T) {
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
	provider := domain.Provider{ID: "prv_cloud_lease", UserID: owner, Name: "Cloud Lease", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake"}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversation := domain.Conversation{ID: "run_cloud_lease", UserID: owner, Title: "Lease", ProviderID: provider.ID, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	node, _, err := store.EnsureCloudExecutionNode(ctx, owner, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(cloudTaskPayload{Kind: "unknown", ConversationID: conversation.ID, Content: "do not execute"})
	task, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_cloud_invalid", UserID: owner, NodeID: node.ID, IdempotencyKey: "cloud-invalid-1", Payload: payload}, false, now)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewCloudWorker(store, completedTurnRuntime{store: store, userID: owner, conversation: conversation, generation: generation}, owner, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || readBack.Status != "reported_failed" {
		t.Fatalf("invalid task was falsely completed: task=%+v err=%v", readBack, err)
	}
	if turns, err := store.AgentTurns(ctx, owner, conversation.ID); err != nil || len(turns) != 0 {
		t.Fatalf("invalid task reached Agent runtime: turns=%+v err=%v", turns, err)
	}
}
