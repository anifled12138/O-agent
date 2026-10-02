package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillsRegistry(t *testing.T) {
	tempDir := t.TempDir()
	skillsDir := filepath.Join(tempDir, ".skills")
	if err := os.MkdirAll(filepath.Join(skillsDir, "test-skill"), 0755); err != nil {
		t.Fatalf("failed to create test skill dir: %v", err)
	}

	skillContent := `---
name: Test Skill Name
description: A skill for testing
tags: testing, mock
triggers: [unit testing, go test]
---
# Test Skill Instruction
You must do XYZ when requested.`

	if err := os.WriteFile(filepath.Join(skillsDir, "test-skill", "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatalf("failed to write skill file: %v", err)
	}

	reg, err := NewRegistry(tempDir)
	if err != nil {
		t.Fatalf("failed to initialize skill registry: %v", err)
	}
	skills := reg.List()
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Name != "Test Skill Name" {
		t.Errorf("expected skill name %q, got %q", "Test Skill Name", skills[0].Name)
	}
	if skills[0].Description != "A skill for testing" {
		t.Errorf("expected skill description %q, got %q", "A skill for testing", skills[0].Description)
	}
	if !skills[0].Enabled {
		t.Errorf("expected skill to be enabled by default")
	}

	prompts := reg.ActivePrompts()
	if prompts == "" {
		t.Errorf("expected non-empty active prompts")
	}
	if !strings.Contains(prompts, "triggers: unit testing, go test") || strings.Contains(prompts, "You must do XYZ") {
		t.Fatalf("catalog should include triggers without loading the skill body: %s", prompts)
	}
	selected := reg.SelectForTask("Please run go test ./internal/skills")
	if len(selected) != 1 || selected[0].Prompt == "" {
		t.Fatalf("explicit trigger did not proactively select skill body: %#v", selected)
	}
	if selected[0].ContentHash == "" || selected[0].SourceContent != skillContent {
		t.Fatalf("skill version was not pinned from the full source file: %#v", selected[0])
	}

	if err := reg.SetEnabled("test-skill", false); err != nil {
		t.Fatalf("failed to disable skill: %v", err)
	}
	if reg.ActivePrompts() != "" {
		t.Errorf("expected empty active prompts when disabled")
	}

	// Verify persistence across new registry instances
	reg2, err := NewRegistry(tempDir)
	if err != nil {
		t.Fatalf("failed to reload skill registry: %v", err)
	}
	skills2 := reg2.List()
	if len(skills2) != 1 {
		t.Fatalf("expected 1 skill in reg2, got %d", len(skills2))
	}
	if skills2[0].Enabled {
		t.Errorf("expected skill to remain disabled after reload from persistent config")
	}
}

func TestRegistrySurfacesUnreadablePersistedState(t *testing.T) {
	tempDir := t.TempDir()
	configDir := filepath.Join(tempDir, ".axiom")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "skills.json"), []byte("{"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRegistry(tempDir); err == nil {
		t.Fatal("expected malformed persisted skill state to be reported")
	}
}

func TestReloadFailureKeepsPreviousSkillSnapshot(t *testing.T) {
	tempDir := t.TempDir()
	skillDir := filepath.Join(tempDir, ".skills", "good")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(path, []byte("---\nname: Good\n---\nbody"), 0644); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	before := registry.List()[0].ContentHash
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4*1024*1024+1)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := registry.Reload(); err == nil {
		t.Fatal("expected oversized line parse error")
	}
	after := registry.List()
	if len(after) != 1 || after[0].ContentHash != before {
		t.Fatalf("failed reload replaced the prior snapshot: before=%q after=%#v", before, after)
	}
}
