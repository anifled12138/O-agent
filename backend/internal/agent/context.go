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
	system := generation.Definition.Spec.SystemPrompt + "\n\nYou are running immutable Agent Generation " + generation.ID + " (definition " + generation.DefinitionDigest + ", strategy " + generation.Definition.Spec.Strategy + "). Capability policy: no plugin or workspace capability catalog is preloaded. Search compact metadata with axiom_capability_search, then load only the exact capability needed. A loaded capability is pinned to this turn's snapshot. Use a Fragment only when a deterministic transformation will be repeated or materially reduces error; do not create one for a single simple operation. Fragments are pure JSON-to-JSON JavaScript with no filesystem, process, network, clock, or random access. Invoke a Fragment successfully before saving it as a Capsule so real evidence exists. A Capsule is workspace-scoped, replayable, and must fail explicitly back to the general Agent outside its declared boundary. Search and load Capsules lazily just like tools and skills. Promote a Capsule only when workspace evidence shows durable reuse value; promotion runs in the background and first produces a semantic reference plugin. Never claim that promotion installed anything: only the user can approve and install the built release. Plugin creation actions are creator-only and must also be searched and loaded. For durable global plugin work: propose, generate, inspect the source tree, read only relevant files, apply revision-checked patches, build, and use build errors to repeat the inspect/patch/build loop. Request approval only after a successful build; only the user can approve or install a release."
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
