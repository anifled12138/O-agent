package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"axiom.local/agent/internal/domain"
)

type openAIResponsesAdapter struct{}

const responsesInlineImageMaxBytes = 6 << 20

// responsesInputContent converts the Markdown image convention shared with the
// Chat Completions adapter into Responses input content parts. This is also how
// screenshots returned by tools reach vision-capable Responses models.
func responsesInputContent(content string) (any, error) {
	matches := imageMarkdownRegex.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		if strings.Contains(content, "](data:image/") {
			return nil, errors.New("Responses inline image Markdown is malformed")
		}
		return content, nil
	}
	parts := make([]map[string]any, 0, len(matches)*2+1)
	last := 0
	for _, match := range matches {
		if match[0] > last {
			chunk := content[last:match[0]]
			if strings.Contains(chunk, "](data:image/") {
				return nil, errors.New("Responses inline image Markdown is malformed")
			}
			if text := strings.TrimSpace(chunk); text != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": text})
			}
		}
		imageURL := content[match[4]:match[5]]
		if strings.HasPrefix(imageURL, "data:image/") {
			comma := strings.IndexByte(imageURL, ',')
			if comma < 0 || !strings.Contains(imageURL[:comma], ";base64") {
				return nil, errors.New("Responses inline image must be a base64 data URL")
			}
			encoded := imageURL[comma+1:]
			if len(encoded) > base64.StdEncoding.EncodedLen(responsesInlineImageMaxBytes) {
				return nil, fmt.Errorf("Responses inline image exceeds the %d-byte limit", responsesInlineImageMaxBytes)
			}
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("decode Responses inline image: %w", err)
			}
			if len(decoded) > responsesInlineImageMaxBytes {
				return nil, fmt.Errorf("Responses inline image exceeds the %d-byte limit", responsesInlineImageMaxBytes)
			}
		}
		parts = append(parts, map[string]any{"type": "input_image", "image_url": imageURL, "detail": "auto"})
		last = match[1]
	}
	if last < len(content) {
		chunk := content[last:]
		if strings.Contains(chunk, "](data:image/") {
			return nil, errors.New("Responses inline image Markdown is malformed")
		}
		if text := strings.TrimSpace(chunk); text != "" {
			parts = append(parts, map[string]any{"type": "input_text", "text": text})
		}
	}
	return parts, nil
}

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
		if len(message.ProviderItems) > 0 {
			if message.Role != "assistant" {
				return nil, errors.New("provider-native Responses state must be sent as a standalone opaque window")
			}
			for _, item := range message.ProviderItems {
				if !json.Valid(item) {
					return nil, errors.New("provider-native Responses state contains invalid JSON")
				}
				input = append(input, json.RawMessage(append([]byte(nil), item...)))
			}
			continue
		}
		switch message.Role {
		case "tool":
			output, err := responsesInputContent(message.Content)
			if err != nil {
				return nil, err
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": output})
		default:
			if message.Content != "" {
				content, err := responsesInputContent(message.Content)
				if err != nil {
					return nil, err
				}
				input = append(input, map[string]any{"role": message.Role, "content": content})
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

func (openAIResponsesAdapter) compactionRequest(ctx context.Context, provider domain.Provider, key string, messages []ChatMessage) (*http.Request, error) {
	input := make([]any, 0, len(messages)*2)
	for _, message := range messages {
		if message.Role == "system" || message.Role == "developer" {
			continue
		}
		if len(message.ProviderItems) > 0 {
			for _, item := range message.ProviderItems {
				if !json.Valid(item) {
					return nil, errors.New("provider-native Responses state contains invalid JSON")
				}
				input = append(input, json.RawMessage(append([]byte(nil), item...)))
			}
			continue
		}
		switch message.Role {
		case "tool":
			output, err := responsesInputContent(message.Content)
			if err != nil {
				return nil, err
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": output})
		default:
			if message.Content != "" {
				content, err := responsesInputContent(message.Content)
				if err != nil {
					return nil, err
				}
				input = append(input, map[string]any{"role": message.Role, "content": content})
			}
			for _, call := range message.ToolCalls {
				input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
			}
		}
	}
	if len(input) == 0 {
		return nil, errors.New("Responses compact request has no historical input")
	}
	req, err := jsonRequest(ctx, http.MethodPost, apiEndpoint(provider.BaseURL, "/responses/compact"), map[string]any{"model": provider.Model, "input": input})
	if err == nil {
		bearer(req, key)
	}
	return req, err
}

func (openAIResponsesAdapter) decodeCompletion(raw []byte) (Completion, error) {
	var result struct {
		Model      string            `json:"model"`
		Output     []json.RawMessage `json:"output"`
		OutputText string            `json:"output_text"`
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
	items := cloneRawMessages(result.Output)
	for _, rawItem := range result.Output {
		var item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return Completion{}, fmt.Errorf("decode Responses output item: %w", err)
		}
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
	return normalizeCompletion(Completion{Content: content, ToolCalls: toolCalls, ProviderItems: items, Model: result.Model, Usage: Usage{PromptTokens: result.Usage.InputTokens, CompletionTokens: result.Usage.OutputTokens, TotalTokens: result.Usage.TotalTokens}})
}

func (openAIResponsesAdapter) decodeCompaction(raw []byte) (Completion, error) {
	var result struct {
		Model  string            `json:"model"`
		Output []json.RawMessage `json:"output"`
		Usage  struct {
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
	if len(result.Output) == 0 {
		return Completion{}, errors.New("Responses compact response did not contain a complete output window")
	}
	for index, item := range result.Output {
		if len(item) == 0 || !json.Valid(item) {
			return Completion{}, fmt.Errorf("Responses compact output item %d is invalid", index)
		}
	}
	return Completion{ProviderItems: cloneRawMessages(result.Output), Model: result.Model, Usage: Usage{PromptTokens: result.Usage.InputTokens, CompletionTokens: result.Usage.OutputTokens, TotalTokens: result.Usage.TotalTokens}}, nil
}

func cloneRawMessages(items []json.RawMessage) []json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	cloned := make([]json.RawMessage, len(items))
	for index := range items {
		cloned[index] = append(json.RawMessage(nil), items[index]...)
	}
	return cloned
}
