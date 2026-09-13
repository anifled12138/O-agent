package pluginforge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"axiom.local/agent/internal/pluginmanifest"
	"axiom.local/agent/internal/storage"
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
	for _, path := range []string{"plugin.json", ".gitignore", "backend/main.go", "backend/go.mod", "frontend/index.html", "frontend/app.js", "skills/guide/SKILL.md", "README.md"} {
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

func TestSourcePatchRejectsDestructiveAndEscapingChanges(t *testing.T) {
	for name, patch := range map[string]string{
		"delete": "diff --git a/backend/main.go b/backend/main.go\ndeleted file mode 100644\n--- a/backend/main.go\n+++ /dev/null\n",
		"rename": "diff --git a/backend/main.go b/backend/other.go\nsimilarity index 100%\nrename from backend/main.go\nrename to backend/other.go\n",
		"escape": "diff --git a/../../host.go b/../../host.go\n--- a/../../host.go\n+++ b/../../host.go\n",
		"binary": "diff --git a/frontend/blob.js b/frontend/blob.js\nGIT binary patch\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateSourcePatch(patch); err == nil {
				t.Fatal("unsafe patch was accepted")
			}
		})
	}
}

func TestSourcePatchIsRevisionedAndInspectable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "backend"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "backend", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commitGeneratedProject(ctx, root); err != nil {
		t.Fatal(err)
	}
	before, err := gitText(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/backend/main.go b/backend/main.go\n--- a/backend/main.go\n+++ b/backend/main.go\n@@ -1 +1 @@\n-package main\n+package main // generated\n"
	paths, err := validateSourcePatch(patch)
	if err != nil {
		t.Fatal(err)
	}
	if err = gitWithInput(ctx, root, patch, "apply", "--check", "--index", "--whitespace=error-all", "-"); err != nil {
		t.Fatal(err)
	}
	if err = gitWithInput(ctx, root, patch, "apply", "--index", "--whitespace=fix", "-"); err != nil {
		t.Fatal(err)
	}
	if err = verifyPatchedWorkspace(ctx, root, paths); err != nil {
		t.Fatal(err)
	}
	if err = commitSourcePatch(ctx, root, paths); err != nil {
		t.Fatal(err)
	}
	diff, err := latestSourceDiff(ctx, Project{ID: "project", SourceDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if diff.Revision == before || !strings.Contains(diff.Patch, "+package main // generated") {
		t.Fatalf("unexpected committed diff: %#v", diff)
	}
	entries, err := inspectSourceTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "backend/main.go" || entries[1].Path != "plugin.json" {
		t.Fatalf("unexpected source tree: %#v", entries)
	}
	if err := os.MkdirAll(filepath.Join(root, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build", "plugin.exe"), []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureSourceRepositoryClean(ctx, root); err != nil {
		t.Fatalf("ephemeral build output made the source dirty: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("external change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureSourceRepositoryClean(ctx, root); err == nil {
		t.Fatal("uncommitted source change was accepted")
	}
}

func TestServiceRunsRevisionCheckedAuthoringLoop(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	userID, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	service := &Service{repo: repository, dataDir: dataDir, workspaceRoot: dataDir}

	project, err := service.Create(ctx, userID, CreateInput{Name: "Patch loop", Description: "Verify the authoring loop", Shape: ShapeAgentTool})
	if err != nil {
		t.Fatal(err)
	}
	project, err = service.Generate(ctx, userID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := service.SourceTree(ctx, userID, project.ID)
	if err != nil || len(tree.Files) == 0 {
		t.Fatalf("source tree failed: tree=%#v err=%v", tree, err)
	}
	patch := "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1,5 +1,7 @@\n # Patch loop\n \n Verify the authoring loop\n \n Generated by Axiom Plugin Forge as a agent-tool plugin.\n+\n+Revision checked.\n"
	if _, _, err = service.ApplySourcePatch(ctx, userID, project.ID, PatchInput{ExpectedRevision: "stale", Patch: patch}); err == nil {
		t.Fatal("stale patch revision was accepted")
	}
	_, diff, err := service.ApplySourcePatch(ctx, userID, project.ID, PatchInput{ExpectedRevision: tree.Revision, Patch: patch})
	if err != nil {
		t.Fatal(err)
	}
	file, err := service.ReadSourceFile(ctx, userID, project.ID, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if file.Revision != diff.Revision || !strings.Contains(file.Content, "Revision checked.") {
		t.Fatalf("patch result is inconsistent: file=%#v diff=%#v", file, diff)
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

func TestStrictUISandboxRejectsRemoteAndActiveEscapePatterns(t *testing.T) {
	for _, source := range []string{`<script src="https://evil.example/x.js"></script>`, `<a href="javascript:steal()">x</a>`, `navigator.serviceWorker.register('/worker.js')`, `new Function('return secret')`} {
		if uiPolicyViolation([]byte(source)) == "" {
			t.Fatalf("unsafe UI source was accepted: %s", source)
		}
	}
	if violation := uiPolicyViolation([]byte(`<script>parent.postMessage({type:'axiom.ui.ready'}, '*')</script>`)); violation != "" {
		t.Fatalf("reference bridge rejected: %s", violation)
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
