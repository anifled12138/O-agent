package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestOpenAIChatRequestUsesConservativePayload(t *testing.T) {
	adapter := openAIChatAdapter{}
	provider := domain.Provider{BaseURL: "https://api.example.com/v1", Model: "reasoning-model"}
	req, err := adapter.completionRequest(context.Background(), provider, "secret", []ChatMessage{{Role: "user", Content: "hello"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["temperature"]; exists {
		t.Fatal("temperature must not be sent unless the model profile requests it")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("authorization = %q", got)
	}
}

func TestOpenAIChatAcceptsObjectArgumentsAndMissingID(t *testing.T) {
	raw := []byte(`{"model":"custom","choices":[{"message":{"content":null,"tool_calls":[{"type":"function","function":{"name":"lookup","arguments":{"q":"axiom"}}}]}}]}`)
	completion, err := (openAIChatAdapter{}).decodeCompletion(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(completion.ToolCalls) != 1 || completion.ToolCalls[0].ID != "call_1" {
		t.Fatalf("unexpected tool calls: %#v", completion.ToolCalls)
	}
	if got := completion.ToolCalls[0].Function.Arguments; got != `{"q":"axiom"}` {
		t.Fatalf("arguments = %q", got)
	}
	if len(completion.Warnings) != 2 || completion.Warnings[0].Code != "provider.missing_tool_call_id" || completion.Warnings[1].Code != "provider.object_tool_arguments" {
		t.Fatalf("warnings = %#v", completion.Warnings)
	}
}

func TestProviderErrorsClassifyRateLimitAndHideHTML(t *testing.T) {
	req, err := jsonRequest(context.Background(), "POST", "https://api.example.com/v1/responses", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Header: http.Header{"Retry-After": []string{"3"}}, Request: req}
	providerErr := httpProviderError(response, []byte(`{"error":{"message":"slow down"}}`)).(*ProviderError)
	if providerErr.Class != ErrorRateLimit || providerErr.RetryAfter != 3*time.Second {
		t.Fatalf("provider error = %#v", providerErr)
	}
	if preview := bodyPreview([]byte("<!doctype html><title>proxy login</title>")); preview != "HTML response" {
		t.Fatalf("HTML preview = %q", preview)
	}
}

func TestOpenAIResponsesRoundTripShape(t *testing.T) {
	definition := ToolDefinition{Type: "function"}
	definition.Function.Name = "lookup"
	definition.Function.Description = "Find a value"
	definition.Function.Parameters = json.RawMessage(`{"type":"object"}`)
	provider := domain.Provider{BaseURL: "https://api.openai.com/v1", Model: "gpt-test"}
	req, err := (openAIResponsesAdapter{}).completionRequest(context.Background(), provider, "key", []ChatMessage{{Role: "system", Content: "system"}, {Role: "user", Content: "hello"}}, []ToolDefinition{definition})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.URL.String(); got != "https://api.openai.com/v1/responses" {
		t.Fatalf("URL = %q", got)
	}
	body, _ := io.ReadAll(req.Body)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	tools := payload["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "lookup" {
		t.Fatalf("tools = %#v", tools)
	}

	raw := []byte(`{"model":"gpt-test","output":[{"type":"function_call","call_id":"fc_1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
	completion, err := (openAIResponsesAdapter{}).decodeCompletion(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(completion.ToolCalls) != 1 || completion.ToolCalls[0].ID != "fc_1" || completion.Usage.TotalTokens != 5 {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestAnthropicMapsSystemAndToolBlocks(t *testing.T) {
	provider := domain.Provider{BaseURL: "https://api.anthropic.com/v1", Model: "claude-test"}
	messages := []ChatMessage{
		{Role: "system", Content: "system rules"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Type: "function", Function: ToolFunction{Name: "lookup", Arguments: `{"q":"x"}`}}}},
		{Role: "tool", ToolCallID: "toolu_1", Content: `{"ok":true}`},
	}
	req, err := (anthropicMessagesAdapter{}).completionRequest(context.Background(), provider, "anthropic-key", messages, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("x-api-key") != "anthropic-key" || req.Header.Get("anthropic-version") == "" {
		t.Fatalf("missing Anthropic headers: %#v", req.Header)
	}
	body, _ := io.ReadAll(req.Body)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["system"] != "system rules" {
		t.Fatalf("system = %#v", payload["system"])
	}

	raw := []byte(`{"model":"claude-test","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"toolu_2","name":"lookup","input":{"q":"y"}}],"usage":{"input_tokens":4,"output_tokens":3}}`)
	completion, err := (anthropicMessagesAdapter{}).decodeCompletion(raw)
	if err != nil {
		t.Fatal(err)
	}
	if completion.Content != "checking" || len(completion.ToolCalls) != 1 || completion.Usage.TotalTokens != 7 {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestNormalizeKindPreservesLegacyDefault(t *testing.T) {
	if got, ok := normalizeKind(""); !ok || got != KindOpenAICompatible {
		t.Fatalf("normalizeKind = %q, %v", got, ok)
	}
	if _, ok := normalizeKind("made-up-provider"); ok {
		t.Fatal("unknown provider kind was accepted")
	}
}
