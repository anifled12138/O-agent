package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"axiom.local/agent/internal/domain"
)

type openAIChatAdapter struct{}

func (openAIChatAdapter) modelsRequest(ctx context.Context, provider domain.Provider, key string) (*http.Request, error) {
	req, err := jsonRequest(ctx, http.MethodGet, apiEndpoint(provider.BaseURL, "/models"), nil)
	if err == nil {
		bearer(req, key)
	}
	return req, err
}

func (openAIChatAdapter) completionRequest(ctx context.Context, provider domain.Provider, key string, messages []ChatMessage, tools []ToolDefinition) (*http.Request, error) {
	payload := map[string]any{"model": provider.Model, "messages": messages}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	req, err := jsonRequest(ctx, http.MethodPost, apiEndpoint(provider.BaseURL, "/chat/completions"), payload)
	if err == nil {
		bearer(req, key)
	}
	return req, err
}

func (openAIChatAdapter) decodeCompletion(raw []byte) (Completion, error) {
	var result struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content      string        `json:"content"`
				ToolCalls    []ToolCall    `json:"tool_calls"`
				FunctionCall *ToolFunction `json:"function_call"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return Completion{}, err
	}
	if result.Error != nil {
		return Completion{}, errors.New(result.Error.Message)
	}
	if len(result.Choices) == 0 {
		return Completion{}, errors.New("provider returned no assistant choice")
	}
	message := result.Choices[0].Message
	warnings := []CompatibilityWarning{}
	if len(message.ToolCalls) == 0 && message.FunctionCall != nil {
		message.ToolCalls = []ToolCall{{ID: "call_1", Type: "function", Function: *message.FunctionCall}}
		warnings = append(warnings, CompatibilityWarning{Code: "provider.legacy_function_call", Message: "The provider used the legacy single function_call field."})
	}
	return normalizeCompletion(Completion{Content: message.Content, ToolCalls: message.ToolCalls, Model: result.Model, Usage: Usage{PromptTokens: result.Usage.PromptTokens, CompletionTokens: result.Usage.CompletionTokens, TotalTokens: result.Usage.TotalTokens}, Warnings: warnings})
}
