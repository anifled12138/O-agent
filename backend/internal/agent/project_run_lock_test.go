package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestProjectRunLocksSerializeSharedWorkspaceAndAllowIndependentProjects(t *testing.T) {
	var locks projectRunLockSet
	releaseFirst, err := locks.Acquire(context.Background(), "project-a")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()

	independent, err := locks.Acquire(context.Background(), "project-b")
	if err != nil {
		t.Fatalf("independent project was blocked: %v", err)
	}
	independent()
	taskA, err := locks.Acquire(context.Background(), ProjectDeltaProjectID("project-a", "cloud-task-a"))
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := locks.Acquire(context.Background(), ProjectDeltaProjectID("project-a", "cloud-task-b"))
	if err != nil {
		t.Fatalf("independent cloud task worktree was serialized behind a sibling task: %v", err)
	}
	taskB()
	taskA()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued := make(chan error, 1)
	go func() {
		_, acquireErr := locks.Acquire(ctx, "project-a")
		queued <- acquireErr
	}()
	select {
	case err := <-queued:
		t.Fatalf("same-project run acquired a busy workspace: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled workspace wait returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled workspace wait did not return")
	}
}

func TestProjectRunLockReleaseAllowsNextTurn(t *testing.T) {
	var locks projectRunLockSet
	releaseFirst, err := locks.Acquire(context.Background(), "project-a")
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst()
	releaseSecond, err := locks.Acquire(context.Background(), "project-a")
	if err != nil {
		t.Fatalf("next same-project turn did not acquire released workspace: %v", err)
	}
	releaseSecond()
}
