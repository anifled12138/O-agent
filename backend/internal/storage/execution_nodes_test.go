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

func TestExecutionNodeHeartbeatRevocationAndRestart(t *testing.T) {
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
	created, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_one", UserID: owner, Name: "Laptop", Platform: "windows", Capabilities: []string{"browser", "shell"}}, "token-hash-one")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "node_one" || created.LastSeen != nil {
		t.Fatalf("unexpected registration read-back: %+v", created)
	}
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_two", UserID: owner, Name: "Duplicate token", Platform: "windows", Capabilities: []string{}}, "token-hash-one"); err == nil {
		t.Fatal("duplicate credential hash was accepted")
	}
	nodes, err := store.ExecutionNodes(ctx, owner)
	if err != nil || len(nodes) != 1 || nodes[0].ID != "node_one" {
		t.Fatalf("failed registration left partial node state: nodes=%+v err=%v", nodes, err)
	}
	seenAt := time.Now().UTC().Truncate(time.Microsecond)
	heartbeat, err := store.HeartbeatExecutionNode(ctx, "token-hash-one", "windows", []string{"browser", "shell"}, seenAt)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.LastSeen == nil || !heartbeat.LastSeen.Equal(seenAt) {
		t.Fatalf("heartbeat was not durably read back: %+v", heartbeat)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	readBack, err := store.ExecutionNode(ctx, owner, "node_one")
	if err != nil || readBack.LastSeen == nil || !readBack.LastSeen.Equal(seenAt) {
		t.Fatalf("node state did not survive restart: node=%+v err=%v", readBack, err)
	}
	if err := store.RevokeExecutionNode(ctx, owner, "node_one", seenAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecutionNodeForToken(ctx, "token-hash-one"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("revoked credential lookup = %v, want unauthorized", err)
	}
	readBack, err = store.ExecutionNode(ctx, owner, "node_one")
	if err != nil || readBack.RevokedAt == nil {
		t.Fatalf("revocation did not survive authoritative read-back: node=%+v err=%v", readBack, err)
	}
	if err := store.RevokeExecutionNode(ctx, owner, "node_one", seenAt.Add(2*time.Second)); err != nil {
		t.Fatalf("repeated revoke should read back the already-revoked state: %v", err)
	}
}

func TestExecutionNodeResourceSnapshotPersistsAndInvalidUpdateRollsBack(t *testing.T) {
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
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_capacity", UserID: owner, Name: "Laptop", Platform: "linux"}, "node-capacity-token"); err != nil {
		t.Fatal(err)
	}
	want := storage.ExecutionNodeResources{MemoryTotalBytes: 4 << 30, MemoryAvailableBytes: 3 << 30, LogicalCPUs: 4, MaxConcurrentTasks: 1}
	seenAt := time.Now().UTC().Truncate(time.Microsecond)
	readBack, err := store.HeartbeatExecutionNodeWithResources(ctx, "node-capacity-token", "linux", []string{"agent-runtime"}, want, seenAt)
	if err != nil || readBack.Resources != want {
		t.Fatalf("resource heartbeat was not read back: node=%+v err=%v", readBack, err)
	}
	invalid := want
	invalid.MemoryAvailableBytes = invalid.MemoryTotalBytes + 1
	if _, err := store.HeartbeatExecutionNodeWithResources(ctx, "node-capacity-token", "linux", []string{"agent-runtime"}, invalid, seenAt.Add(time.Second)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("invalid resource update error=%v, want invalid", err)
	}
	readBack, err = store.ExecutionNode(ctx, owner, "node_capacity")
	if err != nil || readBack.Resources != want || readBack.LastSeen == nil || !readBack.LastSeen.Equal(seenAt) {
		t.Fatalf("failed heartbeat changed durable resource snapshot: node=%+v err=%v", readBack, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	readBack, err = store.ExecutionNode(ctx, owner, "node_capacity")
	if err != nil || readBack.Resources != want || readBack.LastSeen == nil || !readBack.LastSeen.Equal(seenAt) {
		t.Fatalf("resource snapshot did not survive storage restart: node=%+v err=%v", readBack, err)
	}
}

func TestExecutionTaskForNodeScopesStatusAndRejectsRevokedNodes(t *testing.T) {
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
	for _, id := range []string{"node_status_owner", "node_status_other"} {
		if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: id, UserID: owner, Name: id, Platform: "linux"}, id+"-credential"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.HeartbeatExecutionNode(ctx, id+"-credential", "linux", nil, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	task, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: "task_status_owner", UserID: owner, NodeID: "node_status_owner", IdempotencyKey: "task-status-owner", Payload: json.RawMessage(`{"kind":"agent_prompt"}`)}, false, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ExecutionTaskForNode(ctx, "node_status_owner", task.ID)
	if err != nil || readBack.ID != task.ID || readBack.Status != "queued" {
		t.Fatalf("own task status did not read back: task=%+v err=%v", readBack, err)
	}
	if _, err := store.ExecutionTaskForNode(ctx, "node_status_other", task.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("different node could read task status: err=%v", err)
	}
	if err := store.RevokeExecutionNode(ctx, owner, "node_status_owner", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecutionTaskForNode(ctx, "node_status_owner", task.ID); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("revoked node could read task status: err=%v", err)
	}
}

func TestNodeTaskClaimHonorsReportedCapacityAndReleasesSlot(t *testing.T) {
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
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_admission", UserID: owner, Name: "Laptop", Platform: "linux"}, "node-admission-token"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	resources := storage.ExecutionNodeResources{MemoryTotalBytes: 8 << 30, MemoryAvailableBytes: 6 << 30, LogicalCPUs: 8, MaxConcurrentTasks: 1}
	if _, err := store.HeartbeatExecutionNodeWithResources(ctx, "node-admission-token", "linux", nil, resources, now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"task_slot_a", "task_slot_b"} {
		if _, _, err := store.CreateExecutionTask(ctx, storage.ExecutionTask{ID: id, UserID: owner, NodeID: "node_admission", IdempotencyKey: id, Payload: json.RawMessage(`{"kind":"agent_prompt"}`)}, false, now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ClaimExecutionTask(ctx, "node_admission", "lease-slot-a", now.Add(time.Minute), now)
	if err != nil || first.ID != "task_slot_a" {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_admission", "lease-slot-b", now.Add(time.Minute), now.Add(time.Second)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second claim error=%v, want queue wait at reported capacity", err)
	}
	if _, err := store.ReportExecutionTask(ctx, "node_admission", first.ID, "lease-slot-a", "accepted", nil, "", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, "node_admission", first.ID, "lease-slot-a", "running", nil, "", now.Add(1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReportExecutionTask(ctx, "node_admission", first.ID, "lease-slot-a", "reported_failed", nil, "verified failure", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimExecutionTask(ctx, "node_admission", "lease-slot-b", now.Add(time.Minute), now.Add(3*time.Second))
	if err != nil || second.ID != "task_slot_b" {
		t.Fatalf("claim after slot release = %+v, %v", second, err)
	}
}

func TestRevokingNodeStopsQueuedTasksAndMarksActiveTasksUnknown(t *testing.T) {
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
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: "node_revoke", UserID: owner, Name: "Worker", Platform: "linux", Capabilities: []string{}}, "node-revoke-hash"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.HeartbeatExecutionNode(ctx, "node-revoke-hash", "linux", []string{}, now); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"kind":"agent_turn"}`)
	reported := storage.ExecutionTask{ID: "reported", UserID: owner, NodeID: "node_revoke", IdempotencyKey: "reported-key", Payload: payload}
	if _, _, err := store.CreateExecutionTask(ctx, reported, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_revoke", "lease-reported", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	for index, state := range []string{"accepted", "running"} {
		if _, err := store.ReportExecutionTask(ctx, "node_revoke", "reported", "lease-reported", state, nil, "", now.Add(time.Duration(index+1)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ReportExecutionTask(ctx, "node_revoke", "reported", "lease-reported", "reported_succeeded", json.RawMessage(`{"ok":true}`), "", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	active := storage.ExecutionTask{ID: "active", UserID: owner, NodeID: "node_revoke", IdempotencyKey: "active-key", Payload: payload}
	if _, _, err := store.CreateExecutionTask(ctx, active, false, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExecutionTask(ctx, "node_revoke", "lease-active", now.Add(64*time.Second), now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	queued := storage.ExecutionTask{ID: "queued", UserID: owner, NodeID: "node_revoke", IdempotencyKey: "queued-key", Payload: payload}
	if _, _, err := store.CreateExecutionTask(ctx, queued, false, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeExecutionNode(ctx, owner, "node_revoke", now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	activeRead, err := store.ExecutionTask(ctx, owner, "active")
	if err != nil || activeRead.Status != "needs_reconciliation" || activeRead.LeaseUntil != nil || activeRead.Error == "" {
		t.Fatalf("active work was not quarantined on node revoke: task=%+v err=%v", activeRead, err)
	}
	queuedRead, err := store.ExecutionTask(ctx, owner, "queued")
	if err != nil || queuedRead.Status != "cancelled" || !queuedRead.CancelRequested {
		t.Fatalf("queued work was not durably cancelled: task=%+v err=%v", queuedRead, err)
	}
	reportedRead, err := store.ExecutionTask(ctx, owner, "reported")
	if err != nil || reportedRead.Status != "needs_reconciliation" || reportedRead.Error == "" {
		t.Fatalf("unverified node-reported result was trusted after node revoke: task=%+v err=%v", reportedRead, err)
	}
}
