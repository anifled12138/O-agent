package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMCPManagerConfigLifecycle(t *testing.T) {
	m := NewManager()

	cfg := ServerConfig{
		ID:          "mock-server",
		Name:        "Mock MCP Server",
		Description: "Test mock server",
		Command:     "echo",
		Enabled:     false,
	}

	if err := m.AddOrUpdateConfig(cfg); err != nil {
		t.Fatalf("failed to add config: %v", err)
	}

	configs := m.ListConfigs()
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if configs[0].ID != "mock-server" {
		t.Errorf("expected ID mock-server, got %s", configs[0].ID)
	}

	if err := m.SetEnabled("mock-server", false); err != nil {
		t.Fatalf("failed to set disabled: %v", err)
	}

	if err := m.Remove("mock-server"); err != nil {
		t.Fatalf("failed to remove config: %v", err)
	}

	if len(m.ListConfigs()) != 0 {
		t.Errorf("expected 0 configs after remove")
	}
}

func TestMCPManagerWorkspacePersistence(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "mcp-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	m1 := NewManagerWithWorkspace(tempDir)
	cfg := ServerConfig{
		ID:          "test-fs",
		Name:        "Filesystem Server",
		Description: "Filesystem MCP",
		Command:     "echo",
		Args:        []string{"hello"},
		Enabled:     false,
	}

	if err := m1.AddOrUpdateConfig(cfg); err != nil {
		t.Fatalf("failed to add config: %v", err)
	}

	configFile := filepath.Join(tempDir, ".axiom", "mcp.json")
	if _, err := os.Stat(configFile); err != nil {
		t.Fatalf("expected config file %s to exist: %v", configFile, err)
	}

	// Create a new manager for the same workspace, ensure configs are reloaded
	m2 := NewManagerWithWorkspace(tempDir)
	configs := m2.ListConfigs()
	if len(configs) != 1 {
		t.Fatalf("expected 1 config reloaded, got %d", len(configs))
	}
	if configs[0].ID != "test-fs" || configs[0].Command != "echo" {
		t.Errorf("unexpected reloaded config: %+v", configs[0])
	}

	// Test Remove persistence
	if err := m2.Remove("test-fs"); err != nil {
		t.Fatalf("failed to remove config: %v", err)
	}

	m3 := NewManagerWithWorkspace(tempDir)
	if len(m3.ListConfigs()) != 0 {
		t.Errorf("expected 0 configs after removal and reload, got %d", len(m3.ListConfigs()))
	}
}