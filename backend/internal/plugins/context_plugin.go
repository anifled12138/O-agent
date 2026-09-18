package plugins

import (
	"context"
	"fmt"

	"axiom.local/agent/internal/provider"
)

type ContextManagerPlugin struct {
	ID                 string
	Name               string
	MaxContextChars    int
	ReservedChars      int
	MaxToolOutputChars int
	MaxLineChars       int
	MaxRecentFullSteps int
}

func NewDefaultContextManagerPlugin() *ContextManagerPlugin {
	return &ContextManagerPlugin{
		ID:                 "builtin:context_manager",
		Name:               "Context Window Guard & Pruner",
		MaxContextChars:    320000,
		ReservedChars:      16000,
		MaxToolOutputChars: 24000,
		MaxLineChars:       1000,
		MaxRecentFullSteps: 4,
	}
}

func (p *ContextManagerPlugin) SanitizeToolOutput(toolName string, raw []byte) []byte {
	if len(raw) <= p.MaxToolOutputChars {
		return raw
	}
	trimmed := raw[:p.MaxToolOutputChars]
	note := fmt.Sprintf("\n... [Tool output truncated: %d bytes total; kept first %d bytes]", len(raw), p.MaxToolOutputChars)
	return append(trimmed, []byte(note)...)
}

func (p *ContextManagerPlugin) PrepareTurnMessages(ctx context.Context, messages []provider.ChatMessage) []provider.ChatMessage {
	if len(messages) <= 2 {
		return messages
	}

	cleaned := make([]provider.ChatMessage, len(messages))
	copy(cleaned, messages)

	totalChars := 0
	for i := range cleaned {
		if cleaned[i].Role == "tool" && len(cleaned[i].Content) > p.MaxToolOutputChars {
			cleaned[i].Content = string(p.SanitizeToolOutput("tool", []byte(cleaned[i].Content)))
		}
		totalChars += len(cleaned[i].Content)
		for _, tc := range cleaned[i].ToolCalls {
			totalChars += len(tc.Function.Arguments)
		}
	}

	targetBudget := p.MaxContextChars - p.ReservedChars
	if targetBudget <= 0 {
		targetBudget = 100000
	}

	if totalChars <= targetBudget {
		return cleaned
	}

	toolIndices := make([]int, 0)
	for i, m := range cleaned {
		if m.Role == "tool" {
			toolIndices = append(toolIndices, i)
		}
	}

	if len(toolIndices) > p.MaxRecentFullSteps {
		pruneCount := len(toolIndices) - p.MaxRecentFullSteps
		for i := 0; i < pruneCount; i++ {
			idx := toolIndices[i]
			originalLen := len(cleaned[idx].Content)
			if originalLen > 200 {
				preview := cleaned[idx].Content
				if len(preview) > 100 {
					preview = preview[:100]
				}
				cleaned[idx].Content = fmt.Sprintf("%s\n... [Older tool output pruned: %d bytes total; summarized]", preview, originalLen)
			}
		}
	}

	return cleaned
}
