//go:build windows

package sandbox

import (
	"strings"
	"testing"
)

func TestLSACleanupStatusPropagatesFailure(t *testing.T) {
	if err := lsaCallError("close LSA policy handle", 0); err != nil {
		t.Fatalf("successful LSA cleanup returned an error: %v", err)
	}

	err := lsaCallError("close LSA policy handle", 0xC0000002)
	if err == nil || !strings.Contains(err.Error(), "close LSA policy handle") {
		t.Fatalf("LSA cleanup failure was not preserved: %v", err)
	}
}
