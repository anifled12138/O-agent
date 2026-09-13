package capsule

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/capability"
)

type testExecutor struct{}

func (testExecutor) Execute(_ context.Context, _ string, input json.RawMessage, _ capability.Limits) (json.RawMessage, time.Duration, error) {
	var value struct {
		Value int `json:"value"`
	}
	_ = json.Unmarshal(input, &value)
	output, _ := json.Marshal(map[string]int{"value": value.Value * 2})
	return output, time.Millisecond, nil
}

func TestCapsuleSaveReloadAndVerify(t *testing.T) {
	workspace := t.TempDir()
	contract := capability.Contract{
		APIVersion: capability.APIVersion, ID: "fragment.double", Name: "Double value", Summary: "Double an integer", Intent: "Double integer inputs",
		Tier: capability.TierFragment, Scope: capability.ScopeConversation, Runtime: capability.RuntimeJavaScript,
		InputSchema:  json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"integer"}},"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","required":["value"],"properties":{"value":{"type":"integer"}},"additionalProperties":false}`),
		Fallback:     "Return control to the agent",
	}
	contract.Normalize()
	input := json.RawMessage(`{"value":3}`)
	output := json.RawMessage(`{"value":6}`)
	fragment := capability.Fragment{ID: "frag_test", ConversationID: "conversation", TurnID: "turn", Contract: contract, Program: `return {value: input.value * 2};`, Evidence: []capability.EvidenceCase{capability.NewEvidence("case_1", input, output, time.Millisecond)}}
	manifest, err := FromFragment(fragment, "", "")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := Open(workspace, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(workspace, testExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	if report := reloaded.Verify(context.Background(), manifest.ID); !report.Passed {
		t.Fatalf("verification failed: %#v", report)
	}
	if len(reloaded.List()) != 1 {
		t.Fatal("capsule was not listed after reload")
	}
	if _, err := filepath.Abs(reloaded.Root()); err != nil {
		t.Fatal(err)
	}
}
