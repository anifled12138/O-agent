package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginmanifest"
	"axiom.local/agent/internal/provider"
)

func TestSearchUsesCompactTerms(t *testing.T) {
	terms := searchTerms("Find workspace TODO inspection")
	if got := relevance(terms, "workspace inspector scans todo markers"); got < 20 {
		t.Fatalf("expected relevant capability to score, got %d", got)
	}
	if got := relevance(terms, "calendar weather"); got != 0 {
		t.Fatalf("irrelevant capability scored %d", got)
	}
}

func TestSearchCardsExcludeHiddenCapabilitiesAndFullSchemas(t *testing.T) {
	scope := &turnScope{tools: map[string]pluginforge.CapabilityBinding{
		"visible.tool": {ToolExport: pluginmanifest.ToolExport{ID: "visible.tool", Summary: "Inspect workspace", Visibility: "discoverable", InputSchema: json.RawMessage(`{"type":"object","properties":{"secret":{"type":"string"}}}`)}, ReleaseID: "rel_visible"},
		"hidden.tool":  {ToolExport: pluginmanifest.ToolExport{ID: "hidden.tool", Summary: "Inspect workspace secretly", Visibility: "none"}, ReleaseID: "rel_hidden"},
	}, skills: map[string]pluginforge.SkillBinding{}, creator: map[string]provider.ToolDefinition{}}
	results := scope.search("workspace inspect", 10)
	if len(results) != 1 || results[0].ID != "visible.tool" {
		t.Fatalf("unexpected search projection: %#v", results)
	}
	raw, _ := json.Marshal(results)
	if strings.Contains(string(raw), "inputSchema") || strings.Contains(string(raw), "secret") {
		t.Fatalf("search card leaked a full schema: %s", raw)
	}
}

func TestContextWindowKeepsNewestMessages(t *testing.T) {
	detail := domain.ConversationDetail{}
	for index := 0; index < 10; index++ {
		detail.Messages = append(detail.Messages, domain.Message{Role: "user", Content: strings.Repeat(string(rune('a'+index)), 7000)})
	}
	generation := domain.AgentGeneration{ID: "gen_test", DefinitionDigest: "sha256:test", Definition: domain.AgentDefinition{Spec: domain.AgentSpec{Strategy: "react.v1", SystemPrompt: "test system"}}}
	messages, omitted := buildContext(detail, generation, "", 16000)
	if omitted == 0 {
		t.Fatal("expected older context to be omitted")
	}
	if !strings.Contains(messages[len(messages)-1].Content, strings.Repeat("j", 100)) {
		t.Fatal("newest message was not retained")
	}
}

func TestCompactedMemoryIsBoundedAndValidUTF8(t *testing.T) {
	messages := make([]domain.Message, 0, 30)
	for i := 0; i < 30; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		messages = append(messages, domain.Message{Role: role, Content: strings.Repeat("中文目标与验证结果", 200)})
	}
	memory := compactMessages(messages, 512)
	if !strings.Contains(memory, "User Request") || !strings.Contains(memory, "有损摘要") {
		t.Fatalf("compacted memory lost its provenance markers: %q", memory)
	}
	if strings.Contains(memory, "�") {
		t.Fatalf("compacted memory contains invalid UTF-8 replacement characters: %q", memory)
	}
	if runes := len([]rune(memory)); runes > 540 {
		t.Fatalf("compacted memory exceeded its bounded envelope: %d runes", runes)
	}
}

func TestCreatorActionsAreNotPartOfTheBootstrapTools(t *testing.T) {
	for id, definition := range creatorTools() {
		if id == "axiom_capability_search" || id == "axiom_capability_load" || definition.Function.Name != id {
			t.Fatalf("unexpected creator definition %q", id)
		}
	}
}

func TestCreatorActionsExposeLazySourceDevelopmentLoop(t *testing.T) {
	tools := creatorTools()
	for _, name := range []string{"axiom_plugin_source_tree", "axiom_plugin_read_source", "axiom_plugin_source_diff", "axiom_plugin_apply_patch", "axiom_plugin_build"} {
		if _, ok := tools[name]; !ok {
			t.Fatalf("missing creator action %s", name)
		}
	}
	if !strings.Contains(string(tools["axiom_plugin_apply_patch"].Function.Parameters), "expectedRevision") {
		t.Fatal("patch action must require optimistic revision control")
	}
}
