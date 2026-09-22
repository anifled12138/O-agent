package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"regexp"
	"strings"

	"axiom.local/agent/internal/domain"
)

var imageMarkdownRegex = regexp.MustCompile(`!\[(.*?)\]\((data:image\/[a-zA-Z0-9+]+;base64,[A-Za-z0-9+/=]+|https?:\/\/[^\s)]+)\)`)

func formatMessages(messages []ChatMessage) []any {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		matches := imageMarkdownRegex.FindAllStringSubmatchIndex(m.Content, -1)
		if len(matches) == 0 {
			out = append(out, m)
			continue
		}

		parts := make([]map[string]any, 0)
		last := 0
		for _, loc := range matches {
			if loc[0] > last {
				textChunk := strings.TrimSpace(m.Content[last:loc[0]])
				if textChunk != "" {
					parts = append(parts, map[string]any{
						"type": "text",
						"text": textChunk,
					})
				}
			}
			imgURL := m.Content[loc[4]:loc[5]]
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": imgURL,
				},
			})
			last = loc[1]
		}
		if last < len(m.Content) {
			tail := strings.TrimSpace(m.Content[last:])
			if tail != "" {
				parts = append(parts, map[string]any{
					"type": "text",
					"text": tail,
				})
			}
		}

		item := map[string]any{
			"role":    m.Role,
			"content": parts,
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			item["tool_calls"] = m.ToolCalls
		}
		out = append(out, item)
	}
	return out
}

type openAIChatAdapter struct{}

func (openAIChatAdapter) modelsRequest(ctx context.Context, provider domain.Provider, key string) (*http.Request, error) {
	req, err := jsonRequest(ctx, http.MethodGet, apiEndpoint(provider.BaseURL, "/models"), nil)
	if err == nil && key != "" {
		bearer(req, key)
	}
	return req, err
}

func (openAIChatAdapter) completionRequest(ctx context.Context, provider domain.Provider, key string, messages []ChatMessage, tools []ToolDefinition) (*http.Request, error) {
	payload := map[string]any{"model": provider.Model, "messages": formatMessages(messages)}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	req, err := jsonRequest(ctx, http.MethodPost, apiEndpoint(provider.BaseURL, "/chat/completions"), payload)
	if err == nil && key != "" {
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
				Content          string        `json:"content"`
				ReasoningContent string        `json:"reasoning_content"`
				Reasoning        string        `json:"reasoning"`
				ToolCalls        []ToolCall    `json:"tool_calls"`
				FunctionCall     *ToolFunction `json:"function_call"`
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
	if message.Content == "" {
		if message.ReasoningContent != "" {
			message.Content = message.ReasoningContent
		} else if message.Reasoning != "" {
			message.Content = message.Reasoning
		}
	}
	warnings := []CompatibilityWarning{}
	if len(message.ToolCalls) == 0 && message.FunctionCall != nil {
		message.ToolCalls = []ToolCall{{ID: "call_1", Type: "function", Function: *message.FunctionCall}}
		warnings = append(warnings, CompatibilityWarning{Code: "provider.legacy_function_call", Message: "The provider used the legacy single function_call field."})
	}
	return normalizeCompletion(Completion{Content: message.Content, ToolCalls: message.ToolCalls, Model: result.Model, Usage: Usage{PromptTokens: result.Usage.PromptTokens, CompletionTokens: result.Usage.CompletionTokens, TotalTokens: result.Usage.TotalTokens}, Warnings: warnings})
}
