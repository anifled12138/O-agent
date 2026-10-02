package execution

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudAdmissionFollowsLiveHostCapacityAndReleasesSlots(t *testing.T) {
	var capacity atomic.Int32
	capacity.Store(2)
	gate := newCloudResourceAdmission(8, func() (int, error) { return int(capacity.Load()), nil })
	first, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	thirdResult := make(chan func(), 1)
	thirdError := make(chan error, 1)
	go func() {
		release, acquireErr := gate.Acquire(context.Background())
		if acquireErr != nil {
			thirdError <- acquireErr
			return
		}
		thirdResult <- release
	}()
	select {
	case release := <-thirdResult:
		release()
		t.Fatal("admission exceeded the measured host capacity")
	case err := <-thirdError:
		t.Fatal(err)
	case <-time.After(2 * cloudAdmissionPollInterval):
	}

	capacity.Store(3)
	var third func()
	select {
	case third = <-thirdResult:
	case err := <-thirdError:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("queued task did not enter after host capacity increased")
	}
	first()
	second()
	third()
	first()
	gate.mu.Lock()
	active := gate.active
	gate.mu.Unlock()
	if active != 0 {
		t.Fatalf("released admission slots remain active: %d", active)
	}
}

func TestCloudAdmissionWaitIsCancelledWithoutClaimingCapacity(t *testing.T) {
	gate := newCloudResourceAdmission(4, func() (int, error) { return 0, nil })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := gate.Acquire(ctx)
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("Acquire error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled admission did not stop waiting")
	}
	gate.mu.Lock()
	active := gate.active
	gate.mu.Unlock()
	if active != 0 {
		t.Fatalf("cancelled waiter consumed capacity: %d", active)
	}
}

func TestCloudAdmissionQueuesWhileDiskReserveIsUnavailable(t *testing.T) {
	var diskSlots atomic.Int32
	diskSlots.Store(0)
	gate := newCloudResourceAdmission(8, func() (int, error) { return int(diskSlots.Load()), nil })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acquired := make(chan func(), 1)
	go func() {
		release, err := gate.Acquire(ctx)
		if err == nil {
			acquired <- release
		}
	}()
	select {
	case release := <-acquired:
		release()
		t.Fatal("task acquired cloud capacity while disk quota plus reserve did not fit")
	case <-time.After(2 * cloudAdmissionPollInterval):
	}
	diskSlots.Store(1)
	select {
	case release := <-acquired:
		release()
	case <-ctx.Done():
		t.Fatal("queued task did not start after disk capacity became available")
	}
	gate.mu.Lock()
	active := gate.active
	gate.mu.Unlock()
	if active != 0 {
		t.Fatalf("disk-admitted task slot was not released: %d", active)
	}
}
