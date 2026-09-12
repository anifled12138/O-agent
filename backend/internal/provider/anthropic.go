package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"axiom.local/agent/internal/domain"
)

const anthropicVersion = "2023-06-01"

type anthropicMessagesAdapter struct{}

func (anthropicMessagesAdapter) modelsRequest(ctx context.Context, provider domain.Provider, key string) (*http.Request, error) {
	req, err := jsonRequest(ctx, http.MethodGet, apiEndpoint(provider.BaseURL, "/models"), nil)
	if err == nil {
		anthropicHeaders(req, key)
	}
	return req, err
}

func (anthropicMessagesAdapter) completionRequest(ctx context.Context, provider domain.Provider, key string, messages []ChatMessage, tools []ToolDefinition) (*http.Request, error) {
	systems := []string{}
	converted := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "system":
			if message.Content != "" {
				systems = append(systems, message.Content)
			}
		case "tool":
			converted = append(converted, map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": message.Content}}})
		case "assistant":
			blocks := make([]map[string]any, 0, len(message.ToolCalls)+1)
			if message.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": message.Content})
			}
			for _, call := range message.ToolCalls {
				var input any
				if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
					return nil, err
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
			}
			converted = append(converted, map[string]any{"role": "assistant", "content": blocks})
		default:
			converted = append(converted, map[string]any{"role": "user", "content": message.Content})
		}
	}
	payload := map[string]any{"model": provider.Model, "max_tokens": 4096, "messages": converted}
	if len(systems) > 0 {
		payload["system"] = strings.Join(systems, "\n\n")
	}
	if len(tools) > 0 {
		convertedTools := make([]map[string]any, 0, len(tools))
		for _, tool := range tools {
			convertedTools = append(convertedTools, map[string]any{"name": tool.Function.Name, "description": tool.Function.Description, "input_schema": tool.Function.Parameters})
		}
		payload["tools"] = convertedTools
		payload["tool_choice"] = map[string]string{"type": "auto"}
	}
	req, err := jsonRequest(ctx, http.MethodPost, apiEndpoint(provider.BaseURL, "/messages"), payload)
	if err == nil {
		anthropicHeaders(req, key)
	}
	return req, err
}

func (anthropicMessagesAdapter) decodeCompletion(raw []byte) (Completion, error) {
	var result struct {
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
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
	var content strings.Builder
	toolCalls := []ToolCall{}
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			arguments := string(block.Input)
			if arguments == "" || arguments == "null" {
				arguments = "{}"
			}
			toolCalls = append(toolCalls, ToolCall{ID: block.ID, Type: "function", Function: ToolFunction{Name: block.Name, Arguments: arguments}})
		}
	}
	usage := Usage{PromptTokens: result.Usage.InputTokens, CompletionTokens: result.Usage.OutputTokens, TotalTokens: result.Usage.InputTokens + result.Usage.OutputTokens}
	return normalizeCompletion(Completion{Content: content.String(), ToolCalls: toolCalls, Model: result.Model, Usage: usage})
}

func anthropicHeaders(req *http.Request, key string) {
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", anthropicVersion)
}
