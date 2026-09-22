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
	for _, item := range catalog {
		if item.ID == "builtin:context_manager" {
			t.Fatal("internal context manager must not be exposed as a separately toggleable plugin")
		}
	}
	if err := mgr.SetEnabled("builtin:context_manager", false); err == nil {
		t.Fatal("builtin plugin toggle must not report success for a no-op")
	}

	// Shell execution is opt-in; the other five core tools are active by default.
	tools := mgr.ActiveTools()
	if len(tools) != 5 {
		t.Errorf("expected 5 active tools, got %d", len(tools))
	}

	// Disable a core tool
	if err := mgr.SetEnabled("core:fs_read", false); err != nil {
		t.Fatalf("failed to disable fs_read: %v", err)
	}
	tools = mgr.ActiveTools()
	if len(tools) != 4 {
		t.Errorf("expected 4 active tools after disabling fs_read, got %d", len(tools))
	}

	// Re-enable
	if err := mgr.SetEnabled("core:fs_read", true); err != nil {
		t.Fatalf("failed to enable fs_read: %v", err)
	}
	tools = mgr.ActiveTools()
	if len(tools) != 5 {
		t.Errorf("expected 5 active tools after re-enabling fs_read, got %d", len(tools))
	}
}

func TestRunInspectorPluginPersistsAcrossRestart(t *testing.T) {
	tempDir := t.TempDir()
	mgr := NewManager(tempDir)
	if !mgr.IsRunInspectorEnabled() {
		t.Fatal("run inspector must be enabled by default")
	}
	if err := mgr.SetEnabled("core:run_inspector", false); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(tempDir)
	if restarted.IsRunInspectorEnabled() {
		t.Fatal("persisted run inspector state was not restored")
	}
}

func TestRunInspectorToggleRollsBackWhenPersistenceFails(t *testing.T) {
	mgr := NewManager(t.TempDir())
	mgr.statePath = t.TempDir()
	if err := mgr.SetEnabled("core:run_inspector", false); err == nil {
		t.Fatal("expected persistence failure")
	}
	if !mgr.IsRunInspectorEnabled() {
		t.Fatal("failed persistence must not change the effective plugin state")
	}
}
