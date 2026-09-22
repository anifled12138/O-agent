package plugins

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

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
	trimmed := balancedUTF8(string(raw), p.MaxToolOutputChars)
	note := fmt.Sprintf("\n... [Tool output truncated: %d bytes total; retained head and tail]", len(raw))
	return []byte(trimmed + note)
}

func (p *ContextManagerPlugin) PrepareTurnMessages(ctx context.Context, messages []provider.ChatMessage) []provider.ChatMessage {
	return p.PrepareTurnMessagesWithBudget(ctx, messages, p.MaxContextChars, false)
}

// PrepareTurnMessagesWithBudget returns a protocol-valid transcript with old
// tool observations reduced first. Head+tail retention keeps command identity
// and final errors, which are commonly at opposite ends of tool output.
func (p *ContextManagerPlugin) PrepareTurnMessagesWithBudget(ctx context.Context, messages []provider.ChatMessage, maxContextChars int, force bool) []provider.ChatMessage {
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

	reserved := p.ReservedChars
	if quarter := maxContextChars / 4; reserved > quarter {
		reserved = quarter
	}
	targetBudget := maxContextChars - reserved
	if targetBudget < 256 {
		targetBudget = 256
	}

	if totalChars <= targetBudget && !force {
		return cleaned
	}

	toolIndices := make([]int, 0)
	for i, m := range cleaned {
		if m.Role == "tool" {
			toolIndices = append(toolIndices, i)
		}
	}

	keepFull := p.MaxRecentFullSteps
	if force && len(toolIndices) > 0 && len(toolIndices) <= keepFull {
		keepFull = 1
	}
	pruneCount := len(toolIndices) - keepFull
	if pruneCount < 0 {
		pruneCount = 0
	}
	pruneLimit := pruneCount
	if totalChars > targetBudget && len(toolIndices) > 1 {
		pruneLimit = len(toolIndices) - 1
	}
	for i := 0; i < pruneLimit; i++ {
		idx := toolIndices[i]
		originalLen := len(cleaned[idx].Content)
		if originalLen > 320 {
			preview := balancedUTF8(cleaned[idx].Content, 240)
			label := "pruned"
			if force {
				label = "compacted"
			}
			cleaned[idx].Content = fmt.Sprintf("%s\n... [Older tool output %s: %d bytes total; retained head and tail]", preview, label, originalLen)
			totalChars -= originalLen - len(cleaned[idx].Content)
		}
	}

	// A single current observation can itself exceed a small model's usable
	// budget. Keep more of the newest result than older ones, but still bound it.
	if totalChars > targetBudget && len(toolIndices) > 0 {
		idx := toolIndices[len(toolIndices)-1]
		originalLen := len(cleaned[idx].Content)
		newestBudget := targetBudget / 2
		if newestBudget < 1024 {
			newestBudget = 1024
		}
		if originalLen > newestBudget {
			preview := balancedUTF8(cleaned[idx].Content, newestBudget)
			cleaned[idx].Content = fmt.Sprintf("%s\n... [Newest tool output bounded: %d bytes total; retained head and tail]", preview, originalLen)
		}
	}

	return cleaned
}

func balancedUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	if maxBytes < 16 {
		return string([]rune(value)[:min(utf8.RuneCountInString(value), maxBytes)])
	}
	runes := []rune(value)
	headRunes := len(runes) / 2
	for headRunes > 0 && len(string(runes[:headRunes])) > maxBytes/2 {
		headRunes--
	}
	tailRunes := len(runes) - headRunes
	for tailRunes > 0 && len(string(runes[len(runes)-tailRunes:])) > maxBytes-len(string(runes[:headRunes])) {
		tailRunes--
	}
	return strings.TrimSpace(string(runes[:headRunes])) + "\n...\n" + strings.TrimSpace(string(runes[len(runes)-tailRunes:]))
}
