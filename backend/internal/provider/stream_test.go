package provider

import (
	"strings"
	"testing"
)

func TestOpenAIResponsesStreamCommitsOnlyCompletedResponse(t *testing.T) {
	fixture := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"status":"in_progress"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hel"}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"lo"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"model":"gpt-test","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`, "",
	}, "\n")
	events := []StreamEvent{}
	completion, err := decodeOpenAIResponsesStream(strings.NewReader(fixture), func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if completion.Content != "hello" || completion.Usage.TotalTokens != 3 || len(events) != 4 || events[1].Type != StreamContentDelta || events[3].Completion == nil {
		t.Fatalf("completion=%#v events=%#v", completion, events)
	}
}

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
		t.Fatalf("completion=%#v", completion)
	}
	if events[len(events)-1].Type != StreamCompleted || events[len(events)-1].Completion == nil {
		t.Fatalf("events=%#v", events)
	}
}

func TestSSEReaderRejectsOversizedFrames(t *testing.T) {
	oversized := "data: " + strings.Repeat("x", (4<<20)+1) + "\n\n"
	if err := readSSE(strings.NewReader(oversized), func(sseFrame) error { return nil }); err == nil {
		t.Fatal("oversized SSE frame was accepted")
	}
}

func TestAnthropicStreamAssemblesToolUse(t *testing.T) {
	fixture := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":5}}}`, "",
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}`, "",
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`, "",
		`event: message_delta`, `data: {"type":"message_delta","usage":{"output_tokens":3}}`, "",
		`event: message_stop`, `data: {"type":"message_stop"}`, "",
	}, "\n")
	completion, err := decodeAnthropicStream(strings.NewReader(fixture), func(StreamEvent) error { return nil })
	if err != nil || len(completion.ToolCalls) != 1 || completion.ToolCalls[0].Function.Arguments != `{"q":"x"}` || completion.Usage.TotalTokens != 8 {
		t.Fatalf("completion=%#v err=%v", completion, err)
	}
}
