package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"axiom.local/agent/internal/domain"
)

type wireAdapter interface {
	modelsRequest(context.Context, domain.Provider, string) (*http.Request, error)
	completionRequest(context.Context, domain.Provider, string, []ChatMessage, []ToolDefinition) (*http.Request, error)
	decodeCompletion([]byte) (Completion, error)
}

func adapterFor(kind string) (wireAdapter, error) {
	normalized, ok := normalizeKind(kind)
	if !ok {
		return nil, fmt.Errorf("unsupported provider kind %q", kind)
	}
	switch normalized {
	case KindOpenAIResponses:
		return openAIResponsesAdapter{}, nil
	case KindAnthropicMessages:
		return anthropicMessagesAdapter{}, nil
	case KindNewAPI, KindOneAPI, KindOpenAICompatible, KindDeepSeekChat, KindOllama, KindVLLM:
		return openAIChatAdapter{}, nil
	default:
		return openAIChatAdapter{}, nil
	}
}

func jsonRequest(ctx context.Context, method, endpoint string, payload any) (*http.Request, error) {
	var body *bytes.Reader
	if payload == nil {
		body = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err == nil && payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, err
}

func bearer(req *http.Request, key string) {
	trimmed := strings.TrimSpace(key)
	if trimmed != "" {
		req.Header.Set("Authorization", "Bearer "+trimmed)
	}
}

func normalizeCompletion(completion Completion) (Completion, error) {
	for index := range completion.ToolCalls {
		call := &completion.ToolCalls[index]
		if strings.TrimSpace(call.ID) == "" {
			call.ID = fmt.Sprintf("call_%d", index+1)
			completion.Warnings = append(completion.Warnings, CompatibilityWarning{Code: "provider.missing_tool_call_id", Message: "The provider omitted a tool call ID; Axiom generated an attempt-local ID."})
		}
		if call.Function.normalizedObjectArguments {
			completion.Warnings = append(completion.Warnings, CompatibilityWarning{Code: "provider.object_tool_arguments", Message: "The provider returned tool arguments as an object; Axiom normalized them to a JSON string."})
		}
		if call.Type == "" {
			call.Type = "function"
		}
		if call.Type != "function" || strings.TrimSpace(call.Function.Name) == "" {
			return Completion{}, errors.New("provider returned an invalid function call")
		}
		if strings.TrimSpace(call.Function.Arguments) == "" {
			call.Function.Arguments = "{}"
		}
		if !json.Valid([]byte(call.Function.Arguments)) {
			return Completion{}, fmt.Errorf("provider returned invalid JSON arguments for tool %q", call.Function.Name)
		}
	}
	if strings.TrimSpace(completion.Content) == "" && len(completion.ToolCalls) == 0 {
		return Completion{}, errors.New("provider returned neither content nor tool calls")
	}
	return completion, nil
}
