package agent

import (
	"fmt"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/provider"
)

const contextCharacterBudget = 48000

func buildContext(detail domain.ConversationDetail, generation domain.AgentGeneration) ([]provider.ChatMessage, int) {
	selected := make([]domain.Message, 0, len(detail.Messages))
	used := 0
	for index := len(detail.Messages) - 1; index >= 0; index-- {
		message := detail.Messages[index]
		cost := len(message.Content) + 32
		if len(selected) > 0 && used+cost > contextCharacterBudget {
			break
		}
		selected = append(selected, message)
		used += cost
	}
	omitted := len(detail.Messages) - len(selected)
	system := generation.Definition.Spec.SystemPrompt + "\n\nYou are running immutable Agent Generation " + generation.ID + " (definition " + generation.DefinitionDigest + ", strategy " + generation.Definition.Spec.Strategy + "). Capability policy: no plugin catalog is preloaded. Search compact metadata with axiom_capability_search, then load only the exact tool or skill needed. A loaded tool is pinned to this turn's release snapshot. Plugin creation actions are creator-only and must also be searched and loaded. For durable plugin work: propose, generate, inspect the source tree, read only relevant files, apply revision-checked patches, build, and use build errors to repeat the inspect/patch/build loop. Request approval only after a successful build; only the user can approve the release."
	if omitted > 0 {
		system += fmt.Sprintf("\n\nContext window note: %d older persisted messages were omitted from this request; do not invent their contents.", omitted)
	}
	messages := []provider.ChatMessage{{Role: "system", Content: system}}
	for index := len(selected) - 1; index >= 0; index-- {
		message := selected[index]
		messages = append(messages, provider.ChatMessage{Role: message.Role, Content: message.Content})
	}
	return messages, omitted
}
