package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestExecutionTaskIdempotencyLeaseEventsAndExpiryRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_tasks", UserID: owner, Name: "Worker", Platform: "linux", Capabilities: []string{"shell"}}, "node-token-hash"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.HeartbeatExecutionNode(ctx, "node-token-hash", "linux", []string{"shell"}, now); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"kind":"agent_turn","prompt":"inspect the project"}`)
	base := storage.ExecutionTask{ID: "task_one", UserID: owner, NodeID: "node_tasks", IdempotencyKey: "request-1", Payload: payload}
	created, isNew, err := store.CreateExecutionTask(ctx, base, false, now)
	if err != nil || !isNew {
		t.Fatalf("create task: task=%+v new=%v err=%v", created, isNew, err)
	}
	byKey, err := store.ExecutionTaskByIdempotencyKey(ctx, owner, base.IdempotencyKey)
	if err != nil || byKey.ID != created.ID || string(byKey.Payload) != string(payload) {
		t.Fatalf("task lookup by idempotency key did not read back the durable submission: task=%+v err=%v", byKey, err)
	}
	retry := base
	retry.ID = "task_retry"
	readBack, isNew, err := store.CreateExecutionTask(ctx, retry, false, now.Add(time.Second))
	if err != nil || isNew || readBack.ID != created.ID {
		t.Fatalf("idempotent retry created a duplicate: task=%+v new=%v err=%v", readBack, isNew, err)
	}
	conflict := retry
	conflict.Payload = json.RawMessage(`{"kind":"agent_turn","prompt":"different payload"}`)
	if _, _, err := store.CreateExecutionTask(ctx, conflict, false, now.Add(time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("same key with different payload = %v, want conflict", err)
	}
	claimed, err := store.ClaimExecutionTask(ctx, "node_tasks", "lease-hash-one", now.Add(60*time.Second), now.Add(2*time.Second))
	if err != nil || claimed.Status != "leased" || claimed.Attempt != 1 || claimed.Sequence != 2 {
		t.Fatalf("claim failed: task=%+v err=%v", claimed, err)
	}
	for i, status := range []string{"accepted", "running"} {
		claimed, err = store.ReportExecutionTask(ctx, "node_tasks", created.ID, "lease-hash-one", status, nil, "", now.Add(time.Duration(3+i)*time.Second))
		if err != nil || claimed.Status != status {
			t.Fatalf("report %s: task=%+v err=%v", status, claimed, err)
		}
	}
	if _, err := store.ReportExecutionTask(ctx, "node_tasks", created.ID, "lease-hash-one", "incomplete", nil, "", now.Add(5*time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("node report was allowed to forge the cloud-only Agent incomplete state: %v", err)
	}
	claimed, err = store.ExecutionTask(ctx, owner, created.ID)
	if err != nil || claimed.Status != "running" || claimed.LeaseUntil == nil {
		t.Fatalf("rejected incomplete report changed the running task: task=%+v err=%v", claimed, err)
	}
	result := json.RawMessage(`{"summary":"finished"}`)
	claimed, err = store.ReportExecutionTask(ctx, "node_tasks", created.ID, "lease-hash-one", "reported_succeeded", result, "", now.Add(6*time.Second))
	if err != nil || claimed.Status != "reported_succeeded" || string(claimed.Result) != string(result) || claimed.LeaseUntil != nil {
		t.Fatalf("node result report was not committed: task=%+v err=%v", claimed, err)
	}
	events, err := store.ExecutionTaskEvents(ctx, owner, created.ID)
	if err != nil || len(events) != 5 {
		t.Fatalf("task event journal has %d rows, err=%v", len(events), err)
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("event sequence gap: %+v", events)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	claimed, err = store.ExecutionTask(ctx, owner, created.ID)
	if err != nil || claimed.Status != "reported_succeeded" || string(claimed.Result) != string(result) {
		t.Fatalf("reported result did not survive restart: task=%+v err=%v", claimed, err)
	}
	byKey, err = store.ExecutionTaskByIdempotencyKey(ctx, owner, base.IdempotencyKey)
	if err != nil || byKey.ID != created.ID || byKey.Status != "reported_succeeded" {
		t.Fatalf("idempotency lookup did not survive storage restart: task=%+v err=%v", byKey, err)
	}

	second := storage.ExecutionTask{ID: "task_expired", UserID: owner, NodeID: "node_tasks", IdempotencyKey: "request-2", Payload: payload}
	if _, _, err := store.CreateExecutionTask(ctx, second, false, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_tasks", "lease-hash-two", now.Add(11*time.Second), now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeases(ctx, now.Add(20*time.Second)); err != nil || count != 1 {
		t.Fatalf("expired lease reaper returned %d, err=%v", count, err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_tasks", "lease-hash-three", now.Add(80*time.Second), now.Add(20*time.Second)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("empty queue after expired lease = %v", err)
	}
	readBack, err = store.ExecutionTask(ctx, owner, second.ID)
	if err != nil || readBack.Status != "needs_reconciliation" {
		t.Fatalf("expired lease was blindly requeued instead of flagged: task=%+v err=%v", readBack, err)
	}
	if _, err := store.ValidateExecutionTaskRecoveryLease(ctx, "node_tasks", second.ID, "lease-hash-two"); err != nil {
		t.Fatalf("expired lease was not retained for evidence-only recovery: %v", err)
	}
	if _, err := store.ValidateExecutionTaskLease(ctx, "node_tasks", second.ID, "lease-hash-two", now.Add(21*time.Second)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired lease retained execution authorization: %v", err)
	}

	third := storage.ExecutionTask{ID: "task_cancel", UserID: owner, NodeID: "node_tasks", IdempotencyKey: "request-3", Payload: payload}
	if _, _, err := store.CreateExecutionTask(ctx, third, false, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_tasks", "lease-hash-four", now.Add(90*time.Second), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.CancelExecutionTask(ctx, owner, third.ID, now.Add(31*time.Second))
	if err != nil || !cancelled.CancelRequested || cancelled.Status != "leased" {
		t.Fatalf("running cancellation request lost state: task=%+v err=%v", cancelled, err)
	}
	pulsed, err := store.PulseExecutionTask(ctx, "node_tasks", third.ID, "lease-hash-four", now.Add(32*time.Second))
	if err != nil || !pulsed.CancelRequested {
		t.Fatalf("node pulse failed to observe cancellation request: task=%+v err=%v", pulsed, err)
	}
	cancelled, err = store.ReportExecutionTask(ctx, "node_tasks", third.ID, "lease-hash-four", "cancelled", nil, "", now.Add(33*time.Second))
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("node cancellation acknowledgment not persisted: task=%+v err=%v", cancelled, err)
	}
}

func TestExecutionTaskLeaseReconciliationIsAccountScoped(t *testing.T) {
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
	otherUser := domain.User{ID: "user_lease_scope", Email: "lease-scope@example.test", DisplayName: "Lease Scope", CreatedAt: time.Now().UTC()}
	if err := store.CreateUser(ctx, otherUser, "test-password-hash"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, node := range []struct{ id, user, token string }{
		{id: "node_lease_owner", user: owner, token: "lease-owner-token"},
		{id: "node_lease_other", user: otherUser.ID, token: "lease-other-token"},
	} {
		if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: node.id, UserID: node.user, Name: node.id, Platform: "linux"}, node.token); err != nil {
			t.Fatal(err)
		}
		if _, err := store.HeartbeatExecutionNode(ctx, node.token, "linux", nil, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []storage.ExecutionTask{
		{ID: "task_owner_scope", UserID: owner, NodeID: "node_lease_owner", IdempotencyKey: "owner-scope", Payload: json.RawMessage(`{"kind":"agent_turn"}`)},
		{ID: "task_other_scope", UserID: otherUser.ID, NodeID: "node_lease_other", IdempotencyKey: "other-scope", Payload: json.RawMessage(`{"kind":"agent_turn"}`)},
	} {
		if _, _, err := store.CreateExecutionTask(ctx, task, false, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimExecutionTask(ctx, task.NodeID, task.ID+"-lease", now.Add(time.Second), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, "", now.Add(2*time.Second)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty account lease sweep error = %v, want invalid", err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, now.Add(2*time.Second)); err != nil || count != 1 {
		t.Fatalf("owner lease sweep count=%d err=%v, want one", count, err)
	}
	otherTask, err := store.ExecutionTask(ctx, otherUser.ID, "task_other_scope")
	if err != nil || otherTask.Status != "leased" || otherTask.Sequence != 2 {
		t.Fatalf("owner sweep changed another account task: task=%+v err=%v", otherTask, err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, otherUser.ID, now.Add(2*time.Second)); err != nil || count != 1 {
		t.Fatalf("other account lease sweep count=%d err=%v, want one", count, err)
	}
	otherTask, err = store.ExecutionTask(ctx, otherUser.ID, "task_other_scope")
	if err != nil || otherTask.Status != "needs_reconciliation" || otherTask.LeaseUntil != nil {
		t.Fatalf("other account sweep did not durably reconcile its task: task=%+v err=%v", otherTask, err)
	}
}

func TestExecutionTaskSafeHandoffRequestIsIdempotentAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_safe_handoff", UserID: owner, Name: "Local", Platform: "linux", Capabilities: []string{"agent-runtime", "safe-handoff"}}, "safe-handoff-token"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.HeartbeatExecutionNode(ctx, "safe-handoff-token", "linux", []string{"agent-runtime", "safe-handoff"}, now); err != nil {
		t.Fatal(err)
	}
	task := storage.ExecutionTask{ID: "task_safe_handoff", UserID: owner, NodeID: "node_safe_handoff", IdempotencyKey: "safe-handoff-1", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}
	if _, _, err := store.CreateExecutionTask(ctx, task, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, task.NodeID, "handoff-lease", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	requested, err := store.RequestExecutionTaskHandoff(ctx, owner, task.ID, now.Add(time.Second))
	if err != nil || !requested.HandoffRequested || requested.CancelRequested || requested.Status != "leased" || requested.Sequence != 3 {
		t.Fatalf("safe handoff request changed execution ownership or was not durable: task=%+v err=%v", requested, err)
	}
	retried, err := store.RequestExecutionTaskHandoff(ctx, owner, task.ID, now.Add(2*time.Second))
	if err != nil || retried.Sequence != requested.Sequence || !retried.HandoffRequested {
		t.Fatalf("idempotent handoff request created another transition: task=%+v err=%v", retried, err)
	}
	pulsed, err := store.PulseExecutionTask(ctx, task.NodeID, task.ID, "handoff-lease", now.Add(3*time.Second))
	if err != nil || !pulsed.HandoffRequested || pulsed.Status != "leased" || pulsed.LeaseUntil == nil {
		t.Fatalf("leased worker did not read back the handoff request while retaining its lease: task=%+v err=%v", pulsed, err)
	}
	events, err := store.ExecutionTaskEvents(ctx, owner, task.ID)
	if err != nil || len(events) != 3 || events[2].Kind != "handoff_requested" || events[2].Sequence != requested.Sequence {
		t.Fatalf("handoff request journal did not read back exactly once: events=%+v err=%v", events, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	readBack, err := store.ExecutionTask(ctx, owner, task.ID)
	if err != nil || !readBack.HandoffRequested || readBack.Status != "leased" || readBack.Sequence != requested.Sequence {
		t.Fatalf("safe handoff request did not survive storage restart: task=%+v err=%v", readBack, err)
	}
}

func TestExecutionTaskRecoveryAuthorizationSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_recovery_restart", UserID: owner, Name: "Recovery node", Platform: "linux"}, "recovery-restart-node-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "recovery-restart-node-token", "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_recovery_restart", UserID: owner, NodeID: "node_recovery_restart", IdempotencyKey: "recovery-restart", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_recovery_restart", "expired-recovery-token-hash", now.Add(time.Second), now); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, now.Add(2*time.Second)); err != nil || count != 1 {
		t.Fatalf("expire task before restart: count=%d err=%v", count, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ValidateExecutionTaskRecoveryLease(ctx, "node_recovery_restart", "task_recovery_restart", "expired-recovery-token-hash"); err != nil {
		t.Fatalf("recovery authorization did not survive database restart: %v", err)
	}
	if _, err := store.ValidateExecutionTaskLease(ctx, "node_recovery_restart", "task_recovery_restart", "expired-recovery-token-hash", now.Add(3*time.Second)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("recovery authorization regained execution permission after restart: %v", err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, "task_recovery_restart")
	if err != nil || readBack.Status != "needs_reconciliation" || readBack.LeaseUntil != nil {
		t.Fatalf("task quarantine did not survive restart: task=%+v err=%v", readBack, err)
	}
}

func TestExpiredTaskAttemptRemainsFencedFromOtherNodeAndOldLeaseAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, nodeID := range []string{"node_fence_owner", "node_fence_candidate"} {
		tokenHash := nodeID + "-credential-hash"
		if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: nodeID, UserID: owner, Name: nodeID, Platform: "linux"}, tokenHash); err != nil {
			t.Fatal(err)
		}
		if _, err := store.HeartbeatExecutionNode(ctx, tokenHash, "linux", nil, now); err != nil {
			t.Fatal(err)
		}
	}
	const taskID = "task_fenced_attempt"
	if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{
		ID: taskID, UserID: owner, NodeID: "node_fence_owner", IdempotencyKey: "fenced-attempt",
		Payload: json.RawMessage(`{"kind":"agent_turn"}`),
	}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_fence_owner", "expired-fencing-lease", now.Add(time.Second), now); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, owner, now.Add(2*time.Second)); err != nil || count != 1 {
		t.Fatalf("expire and fence original attempt: count=%d err=%v", count, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ClaimExecutionTask(ctx, "node_fence_candidate", "candidate-lease", now.Add(time.Minute), now.Add(3*time.Second)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another online node claimed an attempt whose outcome is still unknown: %v", err)
	}
	if _, err := store.ReportExecutionTask(ctx, "node_fence_owner", taskID, "expired-fencing-lease", "reported_succeeded", json.RawMessage(`{"late":true}`), "", now.Add(3*time.Second)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired execution lease wrote a late task result after restart: %v", err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, taskID)
	if err != nil || readBack.Status != "needs_reconciliation" || readBack.Attempt != 1 || readBack.Sequence != 3 || len(readBack.Result) != 0 || readBack.LeaseUntil != nil {
		t.Fatalf("fenced task state changed after rejected stale mutations: task=%+v err=%v", readBack, err)
	}
	events, err := store.ExecutionTaskEvents(ctx, owner, taskID)
	if err != nil || len(events) != 3 || events[2].Kind != "needs_reconciliation" {
		t.Fatalf("fencing audit was not preserved after restart: events=%+v err=%v", events, err)
	}
}

func TestCreateExecutionTaskWithArtifactsRollsBackQueueWhenInputArtifactIsMissing(t *testing.T) {
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
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_context_rollback", UserID: owner, Name: "Context node", Platform: "linux"}, "node-context-rollback-token"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.HeartbeatExecutionNode(ctx, "node-context-rollback-token", "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.CreateExecutionTaskWithArtifacts(ctx, storage.ExecutionTask{
		ID: "task_context_rollback", UserID: owner, NodeID: "node_context_rollback", IdempotencyKey: "context-rollback-key", Payload: json.RawMessage(`{"kind":"agent_prompt"}`),
	}, false, []storage.ExecutionTaskArtifactLink{{ArtifactID: "missing_context_artifact", Role: "conversation_context"}}, now)
	if err == nil {
		t.Fatal("task with a missing required context artifact was queued")
	}
	if _, err := store.ExecutionTask(ctx, owner, "task_context_rollback"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed task/artifact transaction left a claimable task: %v", err)
	}
}

func TestValidateExecutionTaskLeaseRequiresLiveMatchingTaskAttempt(t *testing.T) {
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
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_lease_check", UserID: owner, Name: "Worker", Platform: "linux"}, "credential-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "credential-hash", "linux", nil, now); err != nil {
		t.Fatal(err)
	}
	task := storage.ExecutionTask{ID: "task_lease_check", UserID: owner, NodeID: "node_lease_check", IdempotencyKey: "lease-check", Payload: json.RawMessage(`{"kind":"artifact_test"}`)}
	if _, _, err := store.CreateExecutionTask(ctx, task, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, task.NodeID, "live-lease-hash", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ValidateExecutionTaskLease(ctx, task.NodeID, task.ID, "live-lease-hash", now.Add(time.Second))
	if err != nil || readBack.ID != task.ID || readBack.Status != "leased" {
		t.Fatalf("current task lease did not authorize: task=%+v err=%v", readBack, err)
	}
	if _, err := store.ValidateExecutionTaskLease(ctx, "other_node", task.ID, "live-lease-hash", now.Add(time.Second)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("different node lease lookup = %v, want not found", err)
	}
	if _, err := store.ValidateExecutionTaskLease(ctx, task.NodeID, task.ID, "wrong-lease-hash", now.Add(time.Second)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong lease = %v, want unauthorized", err)
	}
	if _, err := store.ValidateExecutionTaskLease(ctx, task.NodeID, task.ID, "live-lease-hash", now.Add(2*time.Minute)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("expired lease = %v, want unauthorized", err)
	}
}
