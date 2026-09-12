package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"axiom.local/agent/internal/domain"
)

type openAIResponsesAdapter struct{}

func (openAIResponsesAdapter) modelsRequest(ctx context.Context, provider domain.Provider, key string) (*http.Request, error) {
	req, err := jsonRequest(ctx, http.MethodGet, apiEndpoint(provider.BaseURL, "/models"), nil)
	if err == nil {
		bearer(req, key)
	}
	return req, err
}

func (openAIResponsesAdapter) completionRequest(ctx context.Context, provider domain.Provider, key string, messages []ChatMessage, tools []ToolDefinition) (*http.Request, error) {
	input := make([]any, 0, len(messages)*2)
	for _, message := range messages {
		switch message.Role {
		case "tool":
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": message.Content})
		default:
			if message.Content != "" {
				input = append(input, map[string]any{"role": message.Role, "content": message.Content})
			}
			for _, call := range message.ToolCalls {
				input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
			}
		}
	}
	payload := map[string]any{"model": provider.Model, "input": input}
	if len(tools) > 0 {
		converted := make([]map[string]any, 0, len(tools))
		for _, tool := range tools {
			converted = append(converted, map[string]any{"type": "function", "name": tool.Function.Name, "description": tool.Function.Description, "parameters": tool.Function.Parameters})
		}
		payload["tools"] = converted
		payload["tool_choice"] = "auto"
	}
	req, err := jsonRequest(ctx, http.MethodPost, apiEndpoint(provider.BaseURL, "/responses"), payload)
	if err == nil {
		bearer(req, key)
	}
	return req, err
}

func (openAIResponsesAdapter) decodeCompletion(raw []byte) (Completion, error) {
	var result struct {
		Model  string `json:"model"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		OutputText string `json:"output_text"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
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
	content := ""
	toolCalls := []ToolCall{}
	for _, item := range result.Output {
		switch item.Type {
		case "function_call":
			toolCalls = append(toolCalls, ToolCall{ID: item.CallID, Type: "function", Function: ToolFunction{Name: item.Name, Arguments: item.Arguments}})
		case "message":
			for _, block := range item.Content {
				if block.Type == "output_text" || block.Type == "text" {
					content += block.Text
				}
			}
		}
	}
	if content == "" {
		content = result.OutputText
	}
	return normalizeCompletion(Completion{Content: content, ToolCalls: toolCalls, Model: result.Model, Usage: Usage{PromptTokens: result.Usage.InputTokens, CompletionTokens: result.Usage.OutputTokens, TotalTokens: result.Usage.TotalTokens}})
}
