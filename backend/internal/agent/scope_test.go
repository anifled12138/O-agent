package agent

import (
	"strings"
	"testing"

	"axiom.local/agent/internal/domain"
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
