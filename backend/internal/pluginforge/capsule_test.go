package pluginforge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/capability"
	"axiom.local/agent/internal/capsule"
	"axiom.local/agent/internal/storage"
)

func testCapsule(t *testing.T) capsule.Manifest {
	t.Helper()
	contract := capability.Contract{
		ID: "fragment.normalize", Name: "normalize", Summary: "Normalize a value", Intent: "Normalize repeated records",
		Tier: capability.TierFragment, Scope: capability.ScopeConversation, Runtime: capability.RuntimeJavaScript,
		InputSchema:  json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`),
		Fallback:     "return to the agent",
	}
	contract.Normalize()
	fragment := capability.Fragment{
		ID: "frag_test", ConversationID: "conversation_test", TurnID: "turn_test", Contract: contract,
		Program:  "return { value: input.value.trim().toLowerCase() };",
		Evidence: []capability.EvidenceCase{capability.NewEvidence("case_1", json.RawMessage(`{"value":" O "}`), json.RawMessage(`{"value":"o"}`), time.Millisecond)},
	}
	manifest, err := capsule.FromFragment(fragment, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestCapsuleReferenceProjectIsPermissionlessAndReplayable(t *testing.T) {
	source := testCapsule(t)
	project := Project{ID: "prj_test", Name: "Normalize", Slug: "normalize", Description: "Normalize records", SourceDir: t.TempDir()}
	manifest := capsulePluginManifest(project, source)
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Permissions.Filesystem.Read) != 0 || len(manifest.Permissions.Filesystem.Write) != 0 || len(manifest.Permissions.Network) != 0 || manifest.Permissions.Process {
		t.Fatalf("reference plugin unexpectedly requests permissions: %#v", manifest.Permissions)
	}
	if err := writeCapsuleProject(project, manifest, source); err != nil {
		t.Fatal(err)
	}
	backend, err := os.ReadFile(filepath.Join(project.SourceDir, "backend", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backend), source.Program) || !strings.Contains(string(backend), `delete(input, "_axiomCapabilityId")`) {
		t.Fatal("generated backend does not preserve the capsule or strip Host metadata")
	}
	testSource, err := os.ReadFile(filepath.Join(project.SourceDir, "backend", "main_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(testSource), "TestCapsuleEvidenceReplay") || !strings.Contains(string(testSource), "case_1") {
		t.Fatal("generated reference candidate is missing evidence replay tests")
	}
}

func TestCapsuleReferenceBuildReplaysEvidence(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	userID, err := store.EnsureLocalWorkspaceOwner(ctx)
	if closeErr := store.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	t.Setenv("PATH", filepath.Join(runtime.GOROOT(), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	service := &Service{repo: repository, dataDir: dataDir, workspaceRoot: dataDir}
	project, err := service.Create(ctx, userID, CreateInput{Name: "Capsule build", Description: "Build the verified reference", Shape: ShapeAgentTool})
	if err != nil {
		t.Fatal(err)
	}
	project, err = service.GenerateFromCapsule(ctx, userID, project.ID, testCapsule(t))
	if err != nil {
		t.Fatal(err)
	}
	project, release, err := service.BuildAndTest(ctx, userID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if project.State != StateTested || release.ID == "" || len(release.TestReport) == 0 {
		t.Fatalf("reference release was not tested: project=%#v release=%#v", project, release)
	}
}
