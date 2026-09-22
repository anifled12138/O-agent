package agent

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/provider"
)

var imageMarkdownRegexClean = regexp.MustCompile(`!\[(.*?)\]\((data:image\/[a-zA-Z0-9+]+;base64,[A-Za-z0-9+/=]+|https?:\/\/[^\s)]+)\)`)

func EstimateModelContextTokens(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "1m") || strings.Contains(m, "1000k"):
		return 1000000
	case strings.Contains(m, "200k"):
		return 200000
	case strings.Contains(m, "claude-3-5") || strings.Contains(m, "claude-3-haiku"):
		return 200000
	case strings.Contains(m, "128k") || strings.Contains(m, "gpt-4o") || strings.Contains(m, "deepseek"):
		return 131072
	case strings.Contains(m, "64k"):
		return 65536
	case strings.Contains(m, "32k"):
		return 32768
	case strings.Contains(m, "16k"):
		return 16384
	case strings.Contains(m, "8k"):
		return 8192
	default:
		return 131072 // 默认标准 128K
	}
}

func sanitizeForMemory(content string) string {
	content = imageMarkdownRegexClean.ReplaceAllString(content, "[图片附件: $1]")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimSpace(content)

	return truncateMemoryText(content, 600)
}

func truncateMemoryText(content string, maxRunes int) string {
	content = strings.TrimSpace(content)
	if maxRunes <= 0 || utf8.RuneCountInString(content) <= maxRunes {
		return content
	}
	runes := []rune(content)
	head := maxRunes * 2 / 3
	tail := maxRunes - head
	return string(runes[:head]) + "\n... [省略中间冗余细节] ...\n" + string(runes[len(runes)-tail:])
}

// estimateTextTokens intentionally errs on the conservative side for mixed
// Chinese/English agent transcripts. Byte/character budgets systematically
// undercount CJK text and can let a request overflow before compaction runs.
func estimateTextTokens(content string) int {
	ascii, nonASCII := 0, 0
	for _, r := range content {
		if r <= 0x7f {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII + 8
}

func compactMessages(messages []domain.Message, maxTokens int) string {
	if len(messages) == 0 {
		return ""
	}
	if maxTokens < 256 {
		maxTokens = 256
	}
	var sb strings.Builder
	sb.WriteString("=== [Compacted Conversation Memory / 自动压缩提炼的历史上下文记忆] ===\n")
	sb.WriteString("以下是较早对话的有损摘要。以近期完整消息为准；不要把被截断的细节当作已验证事实。\n\n")

	// Preserve the initial user objective, then prefer the newest omitted turns.
	indices := make([]int, 0, len(messages))
	for i, m := range messages {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			indices = append(indices, i)
			break
		}
	}
	startRecent := len(messages) - 8
	if startRecent < 0 {
		startRecent = 0
	}
	for i := startRecent; i < len(messages); i++ {
		if len(indices) == 0 || indices[len(indices)-1] != i {
			indices = append(indices, i)
		}
	}

	for _, i := range indices {
		m := messages[i]
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		clean := sanitizeForMemory(content)
		line := ""
		if m.Role == "user" {
			line = fmt.Sprintf("• [User Request #%d]: %s\n", i+1, clean)
		} else {
			line = fmt.Sprintf("  ↳ [Assistant Result #%d]: %s\n", i+1, clean)
		}
		if estimateTextTokens(sb.String()+line) > maxTokens {
			remaining := maxTokens - estimateTextTokens(sb.String()) - 32
			if remaining >= 64 {
				clean = truncateMemoryText(clean, remaining)
				if m.Role == "user" {
					line = fmt.Sprintf("• [User Request #%d]: %s\n", i+1, clean)
				} else {
					line = fmt.Sprintf("  ↳ [Assistant Result #%d]: %s\n", i+1, clean)
				}
				sb.WriteString(line)
			}
			break
		}
		sb.WriteString(line)
	}
	sb.WriteString("\n=== [End of Compacted Memory — 压缩记忆结束，以下为近期完整上下文] ===\n")
	return truncateMemoryText(sb.String(), maxTokens)
}

func buildContext(detail domain.ConversationDetail, generation domain.AgentGeneration, extraSkillsPrompt string, contextTokens int) ([]provider.ChatMessage, int) {
	if contextTokens <= 0 {
		contextTokens = 131072
	}
	// Keep substantial headroom for system instructions, tool schemas, tool
	// results, reasoning and the next response. This budget is token-based so
	// CJK-heavy conversations are not badly underestimated.
	conversationBudget := contextTokens * 55 / 100
	if conversationBudget < 2048 {
		conversationBudget = 2048
	}

	selected := make([]domain.Message, 0, len(detail.Messages))
	usedTokens := 0
	for index := len(detail.Messages) - 1; index >= 0; index-- {
		message := detail.Messages[index]
		cost := estimateTextTokens(message.Content) + 16
		if usedTokens+cost > conversationBudget {
			if len(selected) == 0 {
				message.Content = truncateMemoryText(message.Content, conversationBudget-32)
				selected = append(selected, message)
				usedTokens += estimateTextTokens(message.Content) + 16
			}
			break
		}
		selected = append(selected, message)
		usedTokens += cost
	}
	omitted := len(detail.Messages) - len(selected)

	system := generation.Definition.Spec.SystemPrompt + "\n\nYou are running immutable Agent Generation " + generation.ID + " (definition " + generation.DefinitionDigest + ", strategy " + generation.Definition.Spec.Strategy + "). Capability policy: core baseline tools and active plugins/skills are enabled directly.\n\nAutonomous Plugin Authoring: You possess the full lifecycle tools to develop and install plugins autonomously when a requested capability or tool is missing. Workflow: (1) axiom_plugin_propose to create the project, (2) axiom_plugin_generate to generate template source, (3) axiom_plugin_write_source / axiom_plugin_apply_patch to write implementation, (4) axiom_plugin_build to compile and verify, (5) axiom_plugin_install to grant the tested release's declared permissions and activate it. Once installed, the new tool is immediately available in the runtime."
	system += fmt.Sprintf("\n\nContext Window & Active Compaction: the configured context window is approximately %d tokens; persisted conversation history is capped near %d tokens to reserve room for tools and output. axiom_compact_context performs lossy compaction of older in-turn tool traces. Use it after a tool-heavy milestone or when logs become noisy; do not claim that every omitted detail is retained.", contextTokens, conversationBudget)

	if extraSkillsPrompt != "" {
		system += "\n\n" + extraSkillsPrompt
	}

	messages := []provider.ChatMessage{{Role: "system", Content: system}}

	if omitted > 0 {
		memoryBudget := contextTokens * 10 / 100
		if memoryBudget > 4096 {
			memoryBudget = 4096
		}
		compactedMemory := compactMessages(detail.Messages[:omitted], memoryBudget)
		if compactedMemory != "" {
			messages = append(messages, provider.ChatMessage{
				Role:    "system",
				Content: compactedMemory,
			})
		}
	}

	for index := len(selected) - 1; index >= 0; index-- {
		message := selected[index]
		messages = append(messages, provider.ChatMessage{Role: message.Role, Content: message.Content})
	}
	return messages, omitted
}
