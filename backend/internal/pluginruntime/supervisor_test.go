package pluginruntime

import (
	"path/filepath"
	"testing"
	"time"
)

func TestWithinReleaseBoundary(t *testing.T) {
	root := filepath.Join("D:\\", "plugins", "release")
	if !within(root, filepath.Join(root, "backend", "plugin.exe")) {
		t.Fatal("release artifact should be accepted")
	}
	if within(root, filepath.Join(root, "..", "other", "plugin.exe")) {
		t.Fatal("artifact path must not escape release root")
	}
}

func TestShutdownDurationIsBounded(t *testing.T) {
	if got := shutdownDuration(20); got != 10*time.Second {
		t.Fatalf("unsafe short timeout was not normalized: %s", got)
	}
	if got := shutdownDuration(5000); got != 5*time.Second {
		t.Fatalf("valid timeout changed: %s", got)
	}
}
