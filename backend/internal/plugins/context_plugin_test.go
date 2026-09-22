package plugins

import (
	"context"
	"strings"
	"testing"

	"axiom.local/agent/internal/provider"
)

func TestContextManagerPlugin_SanitizeToolOutput(t *testing.T) {
	p := NewDefaultContextManagerPlugin()
	p.MaxToolOutputChars = 50

	small := []byte("hello world")
	if string(p.SanitizeToolOutput("test", small)) != "hello world" {
		t.Fatalf("expected small output untouched")
	}

	large := []byte(strings.Repeat("a", 100))
	sanitized := p.SanitizeToolOutput("test", large)
	if !strings.Contains(string(sanitized), "Tool output truncated") {
		t.Fatalf("expected truncation notice")
	}
	if !strings.Contains(string(sanitized), "aaaa") {
		t.Fatalf("expected retained output content")
	}
}

func TestContextManagerPlugin_SanitizeToolOutputKeepsValidUTF8AndTail(t *testing.T) {
	p := NewDefaultContextManagerPlugin()
	p.MaxToolOutputChars = 80
	raw := []byte(strings.Repeat("开始中文", 30) + "FATAL_END")
	sanitized := string(p.SanitizeToolOutput("test", raw))
	if !strings.Contains(sanitized, "FATAL_END") || strings.Contains(sanitized, "�") {
		t.Fatalf("expected valid UTF-8 with diagnostic tail retained: %q", sanitized)
	}
}

func TestContextManagerPlugin_PrepareTurnMessages(t *testing.T) {
	p := NewDefaultContextManagerPlugin()
	p.MaxContextChars = 1000
	p.ReservedChars = 200
	p.MaxToolOutputChars = 500
	p.MaxRecentFullSteps = 2

	msgs := []provider.ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "user prompt"},
		{Role: "tool", Content: strings.Repeat("A", 400)},
		{Role: "tool", Content: strings.Repeat("B", 400)},
		{Role: "tool", Content: strings.Repeat("C", 400)},
	}

	processed := p.PrepareTurnMessages(context.Background(), msgs)
	if len(processed) != len(msgs) {
		t.Fatalf("expected same message count")
	}
	if !strings.Contains(processed[2].Content, "Older tool output pruned") {
		t.Fatalf("expected oldest tool output to be pruned, got: %s", processed[2].Content)
	}
	if strings.Contains(processed[4].Content, "Older tool output pruned") {
		t.Fatalf("expected recent tool output to be preserved, got: %s", processed[4].Content)
	}
}

func TestContextManagerPlugin_ForcedCompactionPrunesOlderToolTrace(t *testing.T) {
	p := NewDefaultContextManagerPlugin()
	p.MaxRecentFullSteps = 4
	msgs := []provider.ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "task"},
		{Role: "tool", Content: strings.Repeat("old", 300)},
		{Role: "tool", Content: strings.Repeat("new", 300)},
	}
	processed := p.PrepareTurnMessagesWithBudget(context.Background(), msgs, 100000, true)
	if !strings.Contains(processed[2].Content, "Older tool output compacted") {
		t.Fatalf("forced compaction did not affect the older trace: %q", processed[2].Content)
	}
	if processed[3].Content != msgs[3].Content {
		t.Fatal("forced compaction should retain the newest tool trace")
	}
}
