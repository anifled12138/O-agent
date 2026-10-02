package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/provider"
)

func TestArchivedResponsesOutputRequiresExactProviderBinding(t *testing.T) {
	items := []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`)}
	message := provider.ChatMessage{
		Role: "assistant", Content: "displayed text", ToolCalls: []provider.ToolCall{{ID: "call_1"}},
		ProviderItems: items, ProviderID: "provider_1", ProviderModel: "model_1",
	}
	compatible, include, err := prepareArchivedResponsesMessage(message, "provider_1", "model_1", true)
	if err != nil || !include || len(compatible.ProviderItems) != 1 || compatible.Content != "" || len(compatible.ToolCalls) != 0 {
		t.Fatalf("matching Responses output was not replayed as opaque items: message=%+v include=%t err=%v", compatible, include, err)
	}
	fallback, include, err := prepareArchivedResponsesMessage(message, "provider_2", "model_2", true)
	if err != nil || !include || len(fallback.ProviderItems) != 0 || len(fallback.ToolCalls) != 1 || fallback.Content != "displayed text" {
		t.Fatalf("incompatible tool output did not fall back to its readable transcript: message=%+v include=%t err=%v", fallback, include, err)
	}
	if _, include, err := prepareArchivedResponsesMessage(message, "provider_2", "model_2", false); err != nil || include {
		t.Fatalf("incompatible final output was replayed: include=%t err=%v", include, err)
	}
}

func TestNativeResponsesOutputReplacesDuplicateTranscriptText(t *testing.T) {
	previousUser := domain.Message{ID: "user_0", Role: "user", Content: "earlier request", CreatedAt: time.Now().UTC()}
	assistant := domain.Message{ID: "assistant_1", Role: "assistant", Content: "the previous answer", CreatedAt: time.Now().UTC()}
	currentUser := domain.Message{ID: "user_1", Role: "user", Content: "continue", CreatedAt: time.Now().UTC()}
	generation := domain.AgentGeneration{ID: "gen_test", DefinitionDigest: "sha256:test", Definition: domain.AgentDefinition{Spec: domain.AgentSpec{Strategy: "react.v1", SystemPrompt: "system"}}}
	tail := provider.ChatMessage{
		Role: "assistant", ProviderItems: []json.RawMessage{json.RawMessage(`{"type":"message","content":[]}`)},
		ProviderTranscriptHash: contextContentHash(assistant.Content),
	}
	messages, _ := buildContext(domain.ConversationDetail{Messages: []domain.Message{previousUser, assistant, currentUser}}, generation, "", 8192, persistedContextMemory{Tail: []provider.ChatMessage{tail}})
	providerWindows, duplicateTexts := 0, 0
	for _, message := range messages {
		if len(message.ProviderItems) > 0 {
			providerWindows++
		}
		if message.Role == "assistant" && message.Content == assistant.Content {
			duplicateTexts++
		}
	}
	if providerWindows != 1 || duplicateTexts != 0 {
		t.Fatalf("native output and transcript were duplicated or lost: providerWindows=%d duplicateTexts=%d messages=%#v", providerWindows, duplicateTexts, messages)
	}
}

func TestResponsesItemsSurviveLoopToolCallsAndFinalReply(t *testing.T) {
	toolOutput := json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{}"}`)
	finalOutput := json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]}`)
	model := &scriptedModel{results: []provider.Completion{
		{Content: "", ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolFunction{Name: "read", Arguments: `{}`}}}, ProviderItems: []json.RawMessage{toolOutput}},
		{Content: "done", ProviderItems: []json.RawMessage{finalOutput}},
	}}
	result, err := executeLoop(context.Background(), model, loopRequest{
		UserID: "user_1", ProviderID: "provider_1", ProviderModel: "model_1",
		Generation: testGeneration("react.v1", 3), Messages: []provider.ChatMessage{{Role: "system", Content: "system"}, {Role: "user", Content: "read"}}, Scope: &fakeScope{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawToolWindow, sawFinalWindow bool
	for _, message := range model.calls[1] {
		if len(message.ProviderItems) == 1 && string(message.ProviderItems[0]) == string(toolOutput) && message.ProviderID == "provider_1" && message.ProviderModel == "model_1" {
			sawToolWindow = true
		}
	}
	for _, message := range result.Messages {
		if strings.HasPrefix(message.SourceID, "assistant_responses_output:") && len(message.ProviderItems) == 1 && string(message.ProviderItems[0]) == string(finalOutput) && message.ProviderTranscriptHash == contextContentHash("done") {
			sawFinalWindow = true
		}
	}
	if !sawToolWindow || !sawFinalWindow {
		t.Fatalf("Responses items were lost in loop history: toolWindow=%t finalWindow=%t result=%#v", sawToolWindow, sawFinalWindow, result.Messages)
	}
}
