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

func TestAdapterForPreservesNativeProviderProtocols(t *testing.T) {
	responses, err := adapterFor(KindOpenAIResponses)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := responses.(openAIResponsesAdapter); !ok {
		t.Fatalf("responses adapter = %T", responses)
	}
	anthropic, err := adapterFor(KindAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := anthropic.(anthropicMessagesAdapter); !ok {
		t.Fatalf("anthropic adapter = %T", anthropic)
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
