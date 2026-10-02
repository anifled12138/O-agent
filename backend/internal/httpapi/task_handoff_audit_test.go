package httpapi

import (
	"encoding/json"
	"testing"

	"axiom.local/agent/internal/domain"
)

func TestSummarizeTaskHandoffEffectsClassifiesCompletedAndUncertainEffects(t *testing.T) {
	trace := []domain.TraceEvent{
		{ConversationID: "conversation", TurnID: "turn", Sequence: 1, Kind: "permission.checked", Details: json.RawMessage(`{"toolCallId":"read_call","tool":"read_file","effect":"read","outcome":"allow"}`)},
		{ConversationID: "conversation", TurnID: "turn", Sequence: 2, Kind: "tool.started", Details: json.RawMessage(`{"toolCallId":"read_call","name":"read_file","effect":"read"}`)},
		{ConversationID: "conversation", TurnID: "turn", Sequence: 3, Kind: "tool.completed", Details: json.RawMessage(`{"toolCallId":"read_call","name":"read_file","ok":true}`)},
		{ConversationID: "conversation", TurnID: "turn", Sequence: 4, Kind: "tool.started", Details: json.RawMessage(`{"toolCallId":"write_call","name":"publish","effect":"external_write"}`)},
		{ConversationID: "conversation", TurnID: "turn", Sequence: 5, Kind: "tool.completed", Details: json.RawMessage(`{"toolCallId":"write_call","name":"publish","ok":true}`)},
		{ConversationID: "conversation", TurnID: "turn", Sequence: 6, Kind: "tool.started", Details: json.RawMessage(`{"toolCallId":"shell_call","name":"exec_command","effect":"shell"}`)},
	}
	effects, err := summarizeTaskHandoffEffects(trace, "conversation", "turn")
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 2 {
		t.Fatalf("effect audit entries = %#v, want read-only omitted and two writes retained", effects)
	}
	if effects[0].ToolCallID != "write_call" || effects[0].State != "completed" || effects[0].Effect != "external_write" {
		t.Fatalf("completed effect = %#v", effects[0])
	}
	if effects[1].ToolCallID != "shell_call" || effects[1].State != "result_unknown" {
		t.Fatalf("unresolved effect = %#v", effects[1])
	}
}

func TestSummarizeTaskHandoffEffectsFailsClosedOnMalformedOrMismatchedTrace(t *testing.T) {
	tests := []struct {
		name  string
		trace []domain.TraceEvent
	}{
		{name: "malformed", trace: []domain.TraceEvent{{ConversationID: "conversation", TurnID: "turn", Kind: "tool.started", Details: json.RawMessage(`{`)}}},
		{name: "cross-conversation", trace: []domain.TraceEvent{{ConversationID: "other", TurnID: "turn", Kind: "tool.started", Details: json.RawMessage(`{"toolCallId":"call","effect":"external_write"}`)}}},
		{name: "unmatched-completion", trace: []domain.TraceEvent{{ConversationID: "conversation", TurnID: "turn", Kind: "tool.completed", Details: json.RawMessage(`{"toolCallId":"call","ok":true}`)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := summarizeTaskHandoffEffects(test.trace, "conversation", "turn"); err == nil {
				t.Fatal("malformed or cross-conversation trace was accepted")
			}
		})
	}
}

func TestSummarizeTaskHandoffEffectsIgnoresEarlierConversationTurns(t *testing.T) {
	trace := []domain.TraceEvent{
		{ConversationID: "conversation", TurnID: "previous", Sequence: 1, Kind: "tool.started", Details: json.RawMessage(`{"toolCallId":"old_call","name":"publish","effect":"external_write"}`)},
		{ConversationID: "conversation", TurnID: "previous", Sequence: 2, Kind: "tool.completed", Details: json.RawMessage(`{"toolCallId":"old_call","name":"publish","ok":true}`)},
		{ConversationID: "conversation", TurnID: "current", Sequence: 3, Kind: "tool.started", Details: json.RawMessage(`{"toolCallId":"current_call","name":"read_file","effect":"read"}`)},
		{ConversationID: "conversation", TurnID: "current", Sequence: 4, Kind: "tool.completed", Details: json.RawMessage(`{"toolCallId":"current_call","name":"read_file","ok":true}`)},
	}
	effects, err := summarizeTaskHandoffEffects(trace, "conversation", "current")
	if err != nil {
		t.Fatalf("summarize multi-turn handoff trace: %v", err)
	}
	if len(effects) != 0 {
		t.Fatalf("earlier-turn side effects leaked into current turn audit: %+v", effects)
	}
}

func TestValidateTaskHandoffEffectResolutionsRequiresExactDurableEffectSet(t *testing.T) {
	effects := []taskHandoffExternalEffect{
		{ToolCallID: "publish_1", Tool: "publish", Effect: "external_write", State: "completed"},
		{ToolCallID: "shell_2", Tool: "exec_command", Effect: "shell", State: "result_unknown"},
	}
	valid := []taskHandoffEffectResolution{
		{ToolCallID: "publish_1", Outcome: "confirmed_applied"},
		{ToolCallID: "shell_2", Outcome: "unknown"},
	}
	got, err := validateTaskHandoffEffectResolutions(effects, valid)
	if err != nil || len(got) != 2 || got[0].Outcome != "confirmed_applied" {
		t.Fatalf("valid per-effect resolutions = %#v, err=%v", got, err)
	}
	invalid := [][]taskHandoffEffectResolution{
		{{ToolCallID: "publish_1", Outcome: "confirmed_applied"}},
		{{ToolCallID: "publish_1", Outcome: "confirmed_applied"}, {ToolCallID: "publish_1", Outcome: "unknown"}},
		{{ToolCallID: "publish_1", Outcome: "confirmed_applied"}, {ToolCallID: "other", Outcome: "unknown"}},
		{{ToolCallID: "publish_1", Outcome: "maybe"}, {ToolCallID: "shell_2", Outcome: "unknown"}},
	}
	for index, candidate := range invalid {
		if _, err := validateTaskHandoffEffectResolutions(effects, candidate); err == nil {
			t.Fatalf("invalid resolution set %d was accepted: %#v", index, candidate)
		}
	}
	if _, err := validateTaskHandoffEffectResolutions(nil, valid); err == nil {
		t.Fatal("resolutions for effects absent from the trace were accepted")
	}
}
