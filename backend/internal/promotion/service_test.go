package promotion_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"axiom.local/agent/internal/capability"
	"axiom.local/agent/internal/capsule"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginruntime"
	"axiom.local/agent/internal/promotion"
	"axiom.local/agent/internal/storage"
)

type replayExecutor struct{}

func (replayExecutor) Execute(_ context.Context, _ string, _ json.RawMessage, _ capability.Limits) (json.RawMessage, time.Duration, error) {
	return json.RawMessage(`{"value":"o"}`), time.Millisecond, nil
}

func TestPromotionBuildsVerifiedReferenceAndStopsForUserApproval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(runtime.GOROOT(), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

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
	forgeRepository, err := pluginforge.OpenRepository(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer forgeRepository.Close()
	runtimeSupervisor, err := pluginruntime.NewWithData(workspace, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	forge := pluginforge.NewService(forgeRepository, runtimeSupervisor, dataDir, workspace)
	capsules, err := capsule.Open(workspace, replayExecutor{})
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err = capsules.Save(manifest); err != nil {
		t.Fatal(err)
	}
	promotions, err := promotion.Open(ctx, dataDir, capsules, forge)
	if err != nil {
		t.Fatal(err)
	}
	job, err := promotions.Request(userID, manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	var lastStatus promotion.Status
	var lastError string
	for time.Now().Before(deadline) {
		items := promotions.List(userID)
		for _, item := range items {
			if item.ID != job.ID {
				continue
			}
			lastStatus = item.Status
			lastError = item.Error
			if item.Status == promotion.StatusAwaitingApproval {
				if item.ProjectID == "" || item.ReleaseID == "" || item.Verification == nil || !item.Verification.Passed {
					t.Fatalf("promotion lost its evidence or artifacts: %#v", item)
				}
				reopened, openErr := promotion.Open(ctx, dataDir, capsules, forge)
				if openErr != nil {
					t.Fatal(openErr)
				}
				persisted := reopened.List(userID)
				if len(persisted) != 1 || persisted[0].ID != item.ID || persisted[0].Status != promotion.StatusAwaitingApproval {
					t.Fatalf("promotion journal did not recover the terminal state: %#v", persisted)
				}
				return
			}
			if item.Status == promotion.StatusFailed || item.Status == promotion.StatusNeedsRepair || item.Status == promotion.StatusVerificationFailed {
				t.Fatalf("promotion failed in %s: %s", item.Status, item.Error)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("promotion did not reach the user approval gate in time: last status=%s, error=%s", lastStatus, lastError)
}
