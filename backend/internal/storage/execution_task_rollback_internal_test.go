package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestNodeRevocationRollsBackWhenTaskEventCannotBePersisted(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.RegisterExecutionNode(ctx, ExecutionNode{ID: "node_rollback", UserID: owner, Name: "Worker", Platform: "linux", Capabilities: []string{}}, "rollback-token-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "rollback-token-hash", "linux", []string{}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExecutionTask(ctx, ExecutionTask{ID: "task_rollback", UserID: owner, NodeID: "node_rollback", IdempotencyKey: "rollback-key", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_rollback", "rollback-lease", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_task_reconciliation BEFORE INSERT ON execution_task_events WHEN NEW.kind='needs_reconciliation' BEGIN SELECT RAISE(ABORT,'injected task journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeExecutionNode(ctx, owner, "node_rollback", now.Add(time.Second)); err == nil {
		t.Fatal("node revoke reported success after the task audit event failed")
	}
	if _, err := store.ExecutionNodeForToken(ctx, "rollback-token-hash"); err != nil {
		t.Fatalf("failed revoke left node credential revoked: %v", err)
	}
	task, err := store.ExecutionTask(ctx, owner, "task_rollback")
	if err != nil || task.Status != "leased" || task.Sequence != 2 || task.LeaseUntil == nil {
		t.Fatalf("failed revoke did not roll back active task state: task=%+v err=%v", task, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_task_reconciliation`); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeExecutionNode(ctx, owner, "node_rollback", now.Add(2*time.Second)); err != nil {
		t.Fatalf("revoke retry did not complete after removing injected failure: %v", err)
	}
	if _, err := store.ExecutionNodeForToken(ctx, "rollback-token-hash"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("successful retry old credential lookup = %v, want unauthorized", err)
	}
}

func TestSafeHandoffRequestRollsBackWhenAuditEventCannotBePersisted(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	capabilities := []string{"safe-handoff"}
	if _, err := store.RegisterExecutionNode(ctx, ExecutionNode{ID: "node_handoff_rollback", UserID: owner, Name: "Worker", Platform: "linux", Capabilities: capabilities}, "handoff-rollback-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "handoff-rollback-token", "linux", capabilities, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExecutionTask(ctx, ExecutionTask{ID: "task_handoff_rollback", UserID: owner, NodeID: "node_handoff_rollback", IdempotencyKey: "handoff-rollback-key", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_handoff_rollback", "handoff-rollback-lease", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_handoff_event BEFORE INSERT ON execution_task_events WHEN NEW.kind='handoff_requested' BEGIN SELECT RAISE(ABORT,'injected handoff audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestExecutionTaskHandoff(ctx, owner, "task_handoff_rollback", now.Add(time.Second)); err == nil {
		t.Fatal("handoff request reported success after its audit event failed")
	}
	task, err := store.ExecutionTask(ctx, owner, "task_handoff_rollback")
	if err != nil || task.HandoffRequested || task.Status != "leased" || task.Sequence != 2 {
		t.Fatalf("failed handoff request did not roll back the task transition: task=%+v err=%v", task, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_handoff_event`); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.RequestExecutionTaskHandoff(ctx, owner, "task_handoff_rollback", now.Add(2*time.Second))
	if err != nil || !readBack.HandoffRequested || readBack.Sequence != 3 {
		t.Fatalf("handoff retry did not commit after removing injected failure: task=%+v err=%v", readBack, err)
	}
}

func TestExecutionTaskProgressIsMonotonicDurableAndAtomicWithLeasePulse(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	capabilities := []string{"safe-handoff"}
	if _, err := store.RegisterExecutionNode(ctx, ExecutionNode{ID: "node_phase", UserID: owner, Name: "Worker", Platform: "linux", Capabilities: capabilities}, "phase-node-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatExecutionNode(ctx, "phase-node-token", "linux", capabilities, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateExecutionTask(ctx, ExecutionTask{ID: "task_phase", UserID: owner, NodeID: "node_phase", IdempotencyKey: "phase-key", Payload: json.RawMessage(`{"kind":"agent_turn"}`)}, false, now); err != nil {
		t.Fatal(err)
	}
	const leaseHash = "phase-lease-hash"
	if _, err := store.ClaimExecutionTask(ctx, "node_phase", leaseHash, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, "node_phase", "task_phase", leaseHash, "accepted", nil, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, "node_phase", "task_phase", leaseHash, "running", nil, "", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestExecutionTaskHandoff(ctx, owner, "task_phase", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	prepared, err := store.PulseExecutionTaskWithPhase(ctx, "node_phase", "task_phase", leaseHash, "preparing", now.Add(4*time.Second))
	if err != nil || prepared.ProgressPhase != "preparing" || prepared.ProgressUpdatedAt == nil {
		t.Fatalf("preparing phase was not durably read back with its lease pulse: task=%+v err=%v", prepared, err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_task_phase BEFORE INSERT ON execution_task_events WHEN NEW.kind='phase' BEGIN SELECT RAISE(ABORT,'injected phase event failure'); END`); err != nil {
		t.Fatal(err)
	}
	leaseBeforeFailure := prepared.LeaseUntil
	if _, err := store.PulseExecutionTaskWithPhase(ctx, "node_phase", "task_phase", leaseHash, "agent_and_snapshot", now.Add(5*time.Second)); err == nil {
		t.Fatal("phase pulse reported success after its durable event failed")
	}
	afterFailure, err := store.ExecutionTask(ctx, owner, "task_phase")
	if err != nil || afterFailure.ProgressPhase != "preparing" || afterFailure.Sequence != prepared.Sequence || afterFailure.LeaseUntil == nil || leaseBeforeFailure == nil || !afterFailure.LeaseUntil.Equal(*leaseBeforeFailure) {
		t.Fatalf("failed phase event did not roll back both phase and lease renewal: task=%+v err=%v", afterFailure, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_task_phase`); err != nil {
		t.Fatal(err)
	}
	for index, phase := range []string{"agent_and_snapshot", "waiting_for_safe_boundary", "safe_boundary_reached", "local_result_durable", "uploading_artifacts", "artifacts_verified"} {
		readBack, err := store.PulseExecutionTaskWithPhase(ctx, "node_phase", "task_phase", leaseHash, phase, now.Add(time.Duration(6+index)*time.Second))
		if err != nil || readBack.ProgressPhase != phase || readBack.ProgressUpdatedAt == nil {
			t.Fatalf("phase %q did not read back: task=%+v err=%v", phase, readBack, err)
		}
	}
	if _, err := store.PulseExecutionTaskWithPhase(ctx, "node_phase", "task_phase", leaseHash, "agent_and_snapshot", now.Add(20*time.Second)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("backward progress transition error = %v, want conflict", err)
	}
	events, err := store.ExecutionTaskEvents(ctx, owner, "task_phase")
	if err != nil {
		t.Fatal(err)
	}
	phaseCount := 0
	for _, event := range events {
		if event.Kind == "phase" {
			phaseCount++
		}
	}
	if phaseCount != 7 {
		t.Fatalf("durable phase log has %d transitions, want 7", phaseCount)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	readBack, err := store.ExecutionTask(ctx, owner, "task_phase")
	if err != nil || readBack.ProgressPhase != "artifacts_verified" || readBack.ProgressUpdatedAt == nil || *readBack.ProgressUpdatedAt != now.Add(11*time.Second) {
		t.Fatalf("final progress state did not survive restart: task=%+v err=%v", readBack, err)
	}
}
