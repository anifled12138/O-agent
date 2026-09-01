package pluginruntime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginmanifest"
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

func TestPureUISurfaceActivatesWithoutBackend(t *testing.T) {
	supervisor, err := New(filepath.Clean("D:\\agent-harness"))
	if err != nil {
		t.Fatal(err)
	}
	release := pluginforge.Release{ID: "rel_ui", PluginID: "example.ui", Version: "1.0.0", BundleDir: filepath.Clean("D:\\agent-harness\\work\\ui"), Manifest: pluginmanifest.Manifest{
		SpecVersion: pluginmanifest.SpecV2, ID: "example.ui", Name: "UI", Version: "1.0.0", Description: "Pure UI",
		UI: &pluginmanifest.UI{Entry: "frontend/index.html", Assets: "frontend/**", Slots: []string{"workspace.main"}, Sandbox: "strict"},
	}}
	if err := supervisor.Activate(context.Background(), "user", release); err != nil {
		t.Fatal(err)
	}
	if _, ok := supervisor.UI("user", "example.ui"); !ok {
		t.Fatal("UI registry did not publish the active surface")
	}
	states := supervisor.SurfaceStates("user", "example.ui")
	if len(states) != 1 || states[0].Kind != "ui" {
		t.Fatalf("unexpected surface states: %#v", states)
	}
	if err := supervisor.Deactivate(context.Background(), "user", "example.ui"); err != nil {
		t.Fatal(err)
	}
	if _, ok := supervisor.UI("user", "example.ui"); ok {
		t.Fatal("UI surface remained registered after deactivation")
	}
}

func TestSkillSurfaceIsRegisteredWithoutEnteringToolCatalog(t *testing.T) {
	supervisor, err := New(filepath.Clean("D:\\agent-harness"))
	if err != nil {
		t.Fatal(err)
	}
	release := pluginforge.Release{ID: "rel_skill", PluginID: "example.skill", Version: "1.0.0", BundleDir: filepath.Clean("D:\\agent-harness\\work\\skill"), Manifest: pluginmanifest.Manifest{
		SpecVersion: pluginmanifest.SpecV2, ID: "example.skill", Name: "Skill", Version: "1.0.0", Description: "Lazy skill",
		Exports: pluginmanifest.Exports{Skills: []pluginmanifest.SkillExport{{ID: "example.skill.guide", Summary: "Guide work", Visibility: "discoverable", Entry: "skills/guide/SKILL.md"}}},
	}}
	if err := supervisor.Activate(context.Background(), "user", release); err != nil {
		t.Fatal(err)
	}
	if len(supervisor.Skills("user")) != 1 || len(supervisor.Capabilities("user")) != 0 {
		t.Fatal("skill must stay in the lazy skill registry, outside the tool catalog")
	}
	lease := supervisor.BeginTurn("user")
	if err := supervisor.Deactivate(context.Background(), "user", "example.skill"); err != nil {
		t.Fatal(err)
	}
	if len(lease.Skills()) != 1 {
		t.Fatal("turn snapshot lost its pinned skill after hot deactivation")
	}
	lease.Close()
}

func TestShutdownDurationIsBounded(t *testing.T) {
	if got := shutdownDuration(20); got != 10*time.Second {
		t.Fatalf("unsafe short timeout was not normalized: %s", got)
	}
	if got := shutdownDuration(5000); got != 5*time.Second {
		t.Fatalf("valid timeout changed: %s", got)
	}
}
