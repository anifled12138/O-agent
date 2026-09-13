package capability

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type testExecutor struct{}

func (testExecutor) Execute(_ context.Context, _ string, input json.RawMessage, _ Limits) (json.RawMessage, time.Duration, error) {
	var value struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(input, &value); err != nil {
		return nil, 0, err
	}
	output, _ := json.Marshal(map[string]any{"length": len(value.Text)})
	return output, 2 * time.Millisecond, nil
}

func fragmentContract(scope Scope) Contract {
	return Contract{
		ID:           "fragment.text-length",
		Name:         "text_length",
		Summary:      "Count the bytes in text",
		Intent:       "Repeated deterministic text length calculation",
		Tier:         TierFragment,
		Scope:        scope,
		Runtime:      RuntimeJavaScript,
		InputSchema:  json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`),
		OutputSchema: json.RawMessage(`{"type":"object","required":["length"],"properties":{"length":{"type":"integer"}},"additionalProperties":false}`),
		Fallback:     "Return to the general agent",
	}
}

func TestRegistryRecordsEvidenceAndProtectsConversation(t *testing.T) {
	registry := NewRegistry(testExecutor{})
	fragment, err := registry.Register("user-a", "conversation-a", "turn-a", fragmentContract(ScopeConversation), "return { length: input.text.length };")
	if err != nil {
		t.Fatal(err)
	}
	output, err := registry.Invoke(context.Background(), "user-a", "conversation-a", fragment.ID, json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != `{"length":5}` {
		t.Fatalf("unexpected output: %s", output)
	}
	stored, err := registry.Get("user-a", "conversation-a", fragment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Evidence) != 1 || stored.Evidence[0].OutputDigest == "" {
		t.Fatalf("successful invocation was not captured as evidence: %#v", stored.Evidence)
	}
	if _, err = registry.Invoke(context.Background(), "user-a", "conversation-b", fragment.ID, json.RawMessage(`{"text":"hello"}`)); err == nil {
		t.Fatal("fragment leaked into another conversation")
	}
}

func TestRegistryDropsOnlyTurnScopedFragments(t *testing.T) {
	registry := NewRegistry(testExecutor{})
	turnFragment, err := registry.Register("user-a", "conversation-a", "turn-a", fragmentContract(ScopeTurn), "return input;")
	if err != nil {
		t.Fatal(err)
	}
	conversationFragment, err := registry.Register("user-a", "conversation-a", "turn-a", fragmentContract(ScopeConversation), "return input;")
	if err != nil {
		t.Fatal(err)
	}
	registry.DropTurn("user-a", "turn-a")
	if _, err = registry.Get("user-a", "conversation-a", turnFragment.ID); err == nil {
		t.Fatal("turn-scoped fragment survived its turn")
	}
	if _, err = registry.Get("user-a", "conversation-a", conversationFragment.ID); err != nil {
		t.Fatalf("conversation-scoped fragment was dropped: %v", err)
	}
}
