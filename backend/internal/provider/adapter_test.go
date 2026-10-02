package provider

import (
	"context"
	"encoding/base64"
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

func TestOpenAIResponsesReplayPreservesRawOutputItemsInOrder(t *testing.T) {
	adapter := openAIResponsesAdapter{}
	provider := domain.Provider{BaseURL: "https://api.example.com/v1", Model: "gpt-6-astra"}
	items := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"}`),
		json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]}`),
	}
	req, err := adapter.completionRequest(context.Background(), provider, "secret", []ChatMessage{
		{Role: "user", Content: "start"},
		{Role: "assistant", ProviderItems: items, ProviderID: "provider_1", ProviderModel: "gpt-6-astra"},
		{Role: "user", Content: "continue"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Input) != 4 {
		t.Fatalf("input item count = %d, want user + 2 raw output items + user: %s", len(payload.Input), body)
	}
	var first, second, third, fourth map[string]any
	for index, target := range []*map[string]any{&first, &second, &third, &fourth} {
		if err := json.Unmarshal(payload.Input[index], target); err != nil {
			t.Fatal(err)
		}
	}
	if first["role"] != "user" || first["content"] != "start" || second["type"] != "reasoning" || second["encrypted_content"] != "opaque" || third["type"] != "message" || fourth["role"] != "user" || fourth["content"] != "continue" {
		t.Fatalf("raw Responses window changed or moved in input: %s", body)
	}
}

func TestOpenAIResponsesSendsToolScreenshotAsImageInput(t *testing.T) {
	adapter := openAIResponsesAdapter{}
	provider := domain.Provider{BaseURL: "https://api.example.com/v1", Model: "gpt-6-astra"}
	const screenshot = "data:image/png;base64,aGVsbG8="
	req, err := adapter.completionRequest(context.Background(), provider, "secret", []ChatMessage{
		{Role: "tool", ToolCallID: "call_browser", Content: "Rendered page:\n![browser screenshot](" + screenshot + ")"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL string `json:"image_url"`
			} `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Input) != 1 || payload.Input[0].Type != "function_call_output" || payload.Input[0].CallID != "call_browser" {
		t.Fatalf("tool output envelope = %s", body)
	}
	parts := payload.Input[0].Output
	if len(parts) != 2 || parts[0].Type != "input_text" || parts[0].Text != "Rendered page:" || parts[1].Type != "input_image" || parts[1].ImageURL != screenshot {
		t.Fatalf("tool screenshot was not converted to Responses multimodal output: %s", body)
	}
}

func TestOpenAIResponsesRejectsMalformedOrOversizedInlineImage(t *testing.T) {
	if _, err := responsesInputContent("![bad](data:image/png;base64,not-base64!!!)"); err == nil {
		t.Fatal("malformed inline image was accepted")
	}
	encoded := base64.StdEncoding.EncodeToString(make([]byte, responsesInlineImageMaxBytes+1))
	if _, err := responsesInputContent("![large](data:image/png;base64," + encoded + ")"); err == nil {
		t.Fatal("oversized inline image was accepted")
	}
}

func TestOpenAIResponsesCompactionPreservesToolScreenshotInput(t *testing.T) {
	adapter := openAIResponsesAdapter{}
	provider := domain.Provider{BaseURL: "https://api.example.com/v1", Model: "gpt-6-astra"}
	req, err := adapter.compactionRequest(context.Background(), provider, "secret", []ChatMessage{
		{Role: "system", Content: "ignored by compaction"},
		{Role: "tool", ToolCallID: "call_browser", Content: "![screenshot](data:image/png;base64,aGVsbG8=)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []struct {
			Output []map[string]any `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Input) != 1 || len(payload.Input[0].Output) != 1 || payload.Input[0].Output[0]["type"] != "input_image" {
		t.Fatalf("compaction dropped or flattened screenshot input: %s", body)
	}
}

func TestOpenAIResponsesDecodeKeepsEveryRawOutputItem(t *testing.T) {
	raw := []byte(`{"model":"gpt-6-astra","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"output_text":"done"}`)
	completion, err := (openAIResponsesAdapter{}).decodeCompletion(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(completion.ProviderItems) != 2 || string(completion.ProviderItems[0]) != `{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"}` || completion.Content != "done" {
		t.Fatalf("Responses output was not retained completely: content=%q items=%s", completion.Content, completion.ProviderItems)
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
