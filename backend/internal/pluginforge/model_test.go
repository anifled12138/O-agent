package pluginforge

import (
	"encoding/json"
	"testing"

	"axiom.local/agent/internal/pluginmanifest"
)

func TestLifecycleRequiresApproval(t *testing.T) {
	valid := []struct{ from, to State }{
		{StateProposed, StateGenerating},
		{StateGenerating, StateGenerated},
		{StateGenerated, StateBuilding},
		{StateBuilding, StateTested},
		{StateTested, StateAwaitingApproval},
		{StateAwaitingApproval, StateApproved},
		{StateApproved, StateInstalled},
		{StateInstalled, StateActive},
		{StateActive, StateInactive},
		{StateInactive, StateActive},
	}
	for _, transition := range valid {
		if !CanTransition(transition.from, transition.to) {
			t.Fatalf("expected %s -> %s to be valid", transition.from, transition.to)
		}
	}
	if CanTransition(StateTested, StateInstalled) || CanTransition(StateAwaitingApproval, StateInstalled) {
		t.Fatal("installation must not bypass explicit approval")
	}
}

func TestPermissionHashIsCanonical(t *testing.T) {
	a := PermissionSet{WorkspaceRead: true, Network: []string{"b.example", "a.example"}, Secrets: []string{"B", "A"}}
	b := PermissionSet{WorkspaceRead: true, Network: []string{"a.example", "b.example"}, Secrets: []string{"A", "B"}}
	if PermissionHash(a) != PermissionHash(b) {
		t.Fatal("permission hash should not depend on declaration order")
	}
}

func TestManifestRejectsInvalidProtocol(t *testing.T) {
	manifest := Manifest{
		ID: "workspace.test", Name: "Test", Version: "0.1.0", APIVersion: "axiom.plugin/v1",
		Backend:      BackendEntry{Artifact: "backend/plugin.exe", Protocol: "other"},
		Frontend:     FrontendEntry{Entry: "frontend/index.html"},
		Capabilities: []Capability{{ID: "workspace.test.run", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	if manifest.Validate() == nil {
		t.Fatal("invalid sidecar protocol should be rejected")
	}
}

func TestSourceEditsStayInsidePluginWorkspace(t *testing.T) {
	for _, path := range []string{"plugin.json", "backend/main.go", "backend/go.mod", "frontend/index.html", "frontend/app.js", "skills/guide/SKILL.md", "README.md"} {
		if !allowedSourcePath(path) {
			t.Fatalf("expected %q to be allowed", path)
		}
	}
	for _, path := range []string{"../main.go", "backend/plugin.exe", "../../frontend/index.html", "host.go", "frontend/secret.exe"} {
		if allowedSourcePath(path) {
			t.Fatalf("expected %q to be rejected", path)
		}
	}
}

func TestReferenceV2ShapesDeclareOnlyRequestedSurfaces(t *testing.T) {
	project := Project{Name: "Inspector", Slug: "inspector", Description: "Inspect workspace"}
	tests := []struct {
		shape                    string
		backend, ui, tool, skill bool
	}{
		{ShapeHybrid, true, true, true, false},
		{ShapeAgentTool, true, false, true, false},
		{ShapeUI, false, true, false, false},
		{ShapeService, true, false, false, false},
		{ShapeSkill, false, false, false, true},
	}
	for _, test := range tests {
		manifest := referenceManifestV2(project, test.shape)
		if err := manifest.Validate(); err != nil {
			t.Fatalf("shape %s is invalid: %v", test.shape, err)
		}
		if (manifest.Runtime != nil) != test.backend || (manifest.UI != nil) != test.ui || (len(manifest.Exports.Tools) > 0) != test.tool || (len(manifest.Exports.Skills) > 0) != test.skill {
			t.Fatalf("shape %s declared unexpected surfaces: %#v", test.shape, manifest)
		}
	}
}

func TestReferenceManifestPassesV2CompatibilityProjection(t *testing.T) {
	legacy := referenceManifest(Project{Name: "Inspector", Slug: "inspector", Description: "Inspect workspace"})
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	document, err := pluginmanifest.Decode(raw)
	if err != nil {
		t.Fatalf("reference manifest compatibility failed: %v", err)
	}
	if document.SourceVersion != pluginmanifest.SourceV1 || len(document.Manifest.Exports.Tools) != 1 {
		t.Fatalf("unexpected compatibility projection: %#v", document)
	}
}
