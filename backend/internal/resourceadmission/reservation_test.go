package resourceadmission

import "testing"

func TestMemoryReservationIsVisibleAndIdempotentlyReleased(t *testing.T) {
	before := ReservedMemoryBytes()
	release := ReserveMemory(1234)
	if got := ReservedMemoryBytes(); got != before+1234 {
		t.Fatalf("reserved=%d, want %d", got, before+1234)
	}
	release()
	release()
	if got := ReservedMemoryBytes(); got != before {
		t.Fatalf("reserved after release=%d, want %d", got, before)
	}
}
