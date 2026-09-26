package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentTempDirConfiguration(t *testing.T) {
	for _, key := range []string{"O_AGENT_TEMP_DIR", "AXIOM_AGENT_TEMP_DIR"} {
		t.Setenv(key, "")
	}
	if got, want := Load().AgentTempDir, filepath.Join(os.TempDir(), "Axiom", "agent-runs"); got != want {
		t.Fatalf("default AgentTempDir = %q, want %q", got, want)
	}
	t.Setenv("AXIOM_AGENT_TEMP_DIR", filepath.Join(t.TempDir(), "legacy"))
	if got := Load().AgentTempDir; got != os.Getenv("AXIOM_AGENT_TEMP_DIR") {
		t.Fatalf("legacy AgentTempDir = %q, want %q", got, os.Getenv("AXIOM_AGENT_TEMP_DIR"))
	}
	t.Setenv("O_AGENT_TEMP_DIR", filepath.Join(t.TempDir(), "current"))
	if got := Load().AgentTempDir; got != os.Getenv("O_AGENT_TEMP_DIR") {
		t.Fatalf("AgentTempDir = %q, want %q", got, os.Getenv("O_AGENT_TEMP_DIR"))
	}
}
