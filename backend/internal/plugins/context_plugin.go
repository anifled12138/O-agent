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
	return prepareTurnMessagesWithPolicy(ctx, messages, maxContextChars, force, p)
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
