package execution

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const cloudAdmissionPollInterval = 500 * time.Millisecond

type cloudResourceCapacity func() (int, error)

type cloudResourceAdmission struct {
	mu       sync.Mutex
	active   int
	max      int
	capacity cloudResourceCapacity
}

func newCloudResourceAdmission(max int, capacity cloudResourceCapacity) *cloudResourceAdmission {
	return &cloudResourceAdmission{max: max, capacity: capacity}
}

func (g *cloudResourceAdmission) Acquire(ctx context.Context) (func(), error) {
	if g == nil || g.max < 1 || g.capacity == nil {
		return nil, errors.New("cloud resource admission is not configured")
	}
	ticker := time.NewTicker(cloudAdmissionPollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		capacity, err := g.capacity()
		if err != nil {
			return nil, fmt.Errorf("measure cloud host capacity: %w", err)
		}
		if capacity < 0 {
			capacity = 0
		}
		if capacity > g.max {
			capacity = g.max
		}
		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		if g.active < capacity {
			g.active++
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { g.mu.Lock(); g.active--; g.mu.Unlock() }) }, nil
		}
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
