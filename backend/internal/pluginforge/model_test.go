package pluginforge

import (
	"encoding/json"
	"testing"
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
	for _, path := range []string{"plugin.json", "backend/main.go", "backend/go.mod", "frontend/index.html", "frontend/app.js", "README.md"} {
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
