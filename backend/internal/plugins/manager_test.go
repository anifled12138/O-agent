package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPluginsManager(t *testing.T) {
	tempDir := t.TempDir()

	// Create a skill
	skillsDir := filepath.Join(tempDir, ".skills", "mock-skill")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}
	skillFile := filepath.Join(skillsDir, "SKILL.md")
	skillContent := `---
name: Mock Skill
description: Mock Skill Desc
tags: test, helper
---
Instructions for mock skill`
	if err := os.WriteFile(skillFile, []byte(skillContent), 0644); err != nil {
		t.Fatalf("failed to write skill file: %v", err)
	}

	mgr := NewManager(tempDir)
	catalog := mgr.Catalog()
	if len(catalog) < 6 {
		t.Errorf("expected at least 6 plugins (6 core tools + 1 skill), got %d", len(catalog))
	}

	// Test active tools (6 core tools)
	tools := mgr.ActiveTools()
	if len(tools) != 6 {
		t.Errorf("expected 6 active tools, got %d", len(tools))
	}

	// Disable a core tool
	if err := mgr.SetEnabled("core:fs_read", false); err != nil {
		t.Fatalf("failed to disable fs_read: %v", err)
	}
	tools = mgr.ActiveTools()
	if len(tools) != 5 {
		t.Errorf("expected 5 active tools after disabling fs_read, got %d", len(tools))
	}

	// Re-enable
	if err := mgr.SetEnabled("core:fs_read", true); err != nil {
		t.Fatalf("failed to enable fs_read: %v", err)
	}
	tools = mgr.ActiveTools()
	if len(tools) != 6 {
		t.Errorf("expected 6 active tools after re-enabling fs_read, got %d", len(tools))
	}
}
