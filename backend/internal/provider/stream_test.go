package provider

import (
	"strings"
	"testing"
)

func TestOpenAIChatStreamAssemblesToolArgumentsBeforeCompletion(t *testing.T) {
	fixture := strings.Join([]string{
		`data: {"model":"chat-test","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`, "",
		`data: {"model":"chat-test","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"axiom\"}"}}]}}]}`, "",
		`data: {"model":"chat-test","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`, "",
		`data: [DONE]`, "",
	}, "\n")
	events := []StreamEvent{}
	completion, err := decodeOpenAIChatStream(strings.NewReader(fixture), func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completion.ToolCalls) != 1 || completion.ToolCalls[0].Function.Arguments != `{"q":"axiom"}` || completion.Usage.TotalTokens != 7 {
		t.Fatalf("completion=#%v", completion)
	}
	if events[len(events)-1].Type != StreamCompleted || events[len(events)-1].Completion == nil {
		t.Fatalf("events=#%v", events)
	}
}

func TestSSEReaderRejectsOversizedFrames(t *testing.T) {
	oversized := "data: " + strings.Repeat("x", (4<<20)+1) + "\n\n"
	if err := readSSE(strings.NewReader(oversized), func(sseFrame) error { return nil }); err == nil {
		t.Fatalf("oversized SSE frame was accepted")
	}
}
