package skills

import (
	"os"
	"path/filepath"
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
---
# Test Skill Instruction
You must do XYZ when requested.`

	if err := os.WriteFile(filepath.Join(skillsDir, "test-skill", "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatalf("failed to write skill file: %v", err)
	}

	reg := NewRegistry(tempDir)
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

	if err := reg.SetEnabled("test-skill", false); err != nil {
		t.Fatalf("failed to disable skill: %v", err)
	}
	if reg.ActivePrompts() != "" {
		t.Errorf("expected empty active prompts when disabled")
	}

	// Verify persistence across new registry instances
	reg2 := NewRegistry(tempDir)
	skills2 := reg2.List()
	if len(skills2) != 1 {
		t.Fatalf("expected 1 skill in reg2, got %d", len(skills2))
	}
	if skills2[0].Enabled {
		t.Errorf("expected skill to remain disabled after reload from persistent config")
	}
}