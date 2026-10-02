package agent

import (
	"context"
	"sync"
)

// projectRunLockSet serializes Agent turns that share a writable project
// workspace while leaving unrelated projects free to execute concurrently.
// The lock is process-local because each O data directory is owned by one
// service process; durable task leases remain the cross-node ownership guard.
type projectRunLockSet struct {
	locks sync.Map // project ID -> buffered channel semaphore
}

func (set *projectRunLockSet) Acquire(ctx context.Context, projectID string) (func(), error) {
	if projectID == "" {
		return func() {}, nil
	}
	value, _ := set.locks.LoadOrStore(projectID, make(chan struct{}, 1))
	semaphore := value.(chan struct{})
	select {
	case semaphore <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-semaphore }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
