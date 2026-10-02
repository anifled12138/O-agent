// Package resourceadmission coordinates in-process reservations that are not
// represented by an active Agent task sandbox, such as an interactive browser.
package resourceadmission

import (
	"math"
	"sync"
)

var reservations struct {
	sync.Mutex
	bytes int64
}

// ReserveMemory reserves a positive memory envelope for another runtime in
// the current host process. The returned release function is idempotent.
func ReserveMemory(bytes int64) func() {
	if bytes <= 0 {
		panic("memory reservation must be positive")
	}
	reservations.Lock()
	if reservations.bytes > math.MaxInt64-bytes {
		reservations.Unlock()
		panic("memory reservation overflow")
	}
	reservations.bytes += bytes
	reservations.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			reservations.Lock()
			reservations.bytes -= bytes
			reservations.Unlock()
		})
	}
}

// ReservedMemoryBytes returns memory currently reserved by this process.
func ReservedMemoryBytes() int64 {
	reservations.Lock()
	defer reservations.Unlock()
	return reservations.bytes
}
