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
	messages, omitted := buildContext(detail)
	if omitted == 0 {
		t.Fatal("expected older context to be omitted")
	}
	if !strings.Contains(messages[len(messages)-1].Content, strings.Repeat("j", 100)) {
		t.Fatal("newest message was not retained")
	}
}

func TestCreatorActionsAreNotPartOfTheBootstrapTools(t *testing.T) {
	for id, definition := range creatorTools() {
		if id == "axiom_capability_search" || id == "axiom_capability_load" || definition.Function.Name != id {
			t.Fatalf("unexpected creator definition %q", id)
		}
	}
}
