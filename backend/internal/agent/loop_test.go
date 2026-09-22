package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/provider"
)

type scriptedModel struct {
	results []provider.Completion
	calls   [][]provider.ChatMessage
}

func (m *scriptedModel) CompleteWithTools(_ context.Context, _, _ string, messages []provider.ChatMessage, _ []provider.ToolDefinition) (provider.Completion, error) {
	m.calls = append(m.calls, append([]provider.ChatMessage(nil), messages...))
	if len(m.results) == 0 {
		return provider.Completion{}, errors.New("no more scripted responses")
	}
	result := m.results[0]
	m.results = m.results[1:]
	return result, nil
}

type fakeScope struct{ executions int }

func (s *fakeScope) definitions() []provider.ToolDefinition {
	return []provider.ToolDefinition{tool("read", "read", `{}`)}
}
func (s *fakeScope) execute(context.Context, string, json.RawMessage) json.RawMessage {
	s.executions++
	return json.RawMessage(`{"ok":true,"value":"observed"}`)
}

type compactingScope struct{ fakeScope }

func (s *compactingScope) prepareTurnMessages(messages []provider.ChatMessage, step int) ([]provider.ChatMessage, turnCompaction) {
	original := messageChars(messages)
	if step != 1 {
		return messages, turnCompaction{OriginalChars: original, CompactedChars: original}
	}
	compacted := append([]provider.ChatMessage(nil), messages...)
	for i := range compacted {
		if compacted[i].Role == "tool" {
			compacted[i].Content = `{"ok":true,"compacted":true}`
		}
	}
	return compacted, turnCompaction{Applied: true, OriginalChars: original, CompactedChars: messageChars(compacted)}
}

func testGeneration(strategy string, maxSteps int) domain.AgentGeneration {
	return domain.AgentGeneration{ID: "gen_test", DefinitionDigest: "sha256:test", Definition: domain.AgentDefinition{Spec: domain.AgentSpec{Strategy: strategy, SystemPrompt: "system", PlannerPrompt: "plan", MaxSteps: maxSteps}}}
}

func TestReactLoopExecutesToolAndAccumulatesMetrics(t *testing.T) {
	model := &scriptedModel{results: []provider.Completion{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolFunction{Name: "read", Arguments: `{}`}}}, Usage: provider.Usage{TotalTokens: 10}},
		{Content: "done", Usage: provider.Usage{TotalTokens: 4}},
	}}
	scope := &fakeScope{}
	result, err := executeLoop(context.Background(), model, loopRequest{UserID: "u", ProviderID: "p", Generation: testGeneration(evolution.StrategyReact, 3), Messages: []provider.ChatMessage{{Role: "user", Content: "go"}}, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply != "done" || result.Metrics.ModelCalls != 2 || result.Metrics.ToolCalls != 1 || result.Metrics.TotalTokens != 14 || scope.executions != 1 {
		t.Fatalf("unexpected loop result: %#v, executions=%d", result, scope.executions)
	}
	if model.calls[1][len(model.calls[1])-1].Role != "tool" {
		t.Fatalf("tool observation was not returned to model: %#v", model.calls[1])
	}
}

func TestReactLoopAdoptsCompactedTranscript(t *testing.T) {
	model := &scriptedModel{results: []provider.Completion{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolFunction{Name: "read", Arguments: `{}`}}}},
		{Content: "done"},
	}}
	events := []string{}
	result, err := executeLoop(context.Background(), model, loopRequest{
		UserID: "u", ProviderID: "p", Generation: testGeneration(evolution.StrategyReact, 3),
		Messages: []provider.ChatMessage{{Role: "user", Content: "go"}}, Scope: &compactingScope{},
		Emit: func(kind string, _ any) error { events = append(events, kind); return nil },
	})
	if err != nil || result.Reply != "done" {
		t.Fatalf("unexpected compacted loop result: %#v, %v", result, err)
	}
	if got := model.calls[1][len(model.calls[1])-1].Content; !strings.Contains(got, "compacted") {
		t.Fatalf("second request did not use canonical compacted transcript: %q", got)
	}
	if !containsString(events, "context.compacted") {
		t.Fatalf("missing compaction trace event: %#v", events)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestPlanReactAddsAdvisoryBriefBeforeExecution(t *testing.T) {
	model := &scriptedModel{results: []provider.Completion{{Content: "inspect then answer"}, {Content: "result"}}}
	result, err := executeLoop(context.Background(), model, loopRequest{UserID: "u", ProviderID: "p", Generation: testGeneration(evolution.StrategyPlanReact, 2), Messages: []provider.ChatMessage{{Role: "system", Content: "system"}, {Role: "user", Content: "task"}}, Scope: &fakeScope{}})
	if err != nil || result.Reply != "result" || result.Metrics.ModelCalls != 2 {
		t.Fatalf("unexpected plan-react result: %#v, %v", result, err)
	}
	last := model.calls[1][len(model.calls[1])-1]
	if last.Role != "system" || !strings.Contains(last.Content, "inspect then answer") {
		t.Fatalf("planner brief missing from executor context: %#v", model.calls[1])
	}
}

func TestLoopStopsAtGenerationStepBudget(t *testing.T) {
	toolCall := provider.Completion{ToolCalls: []provider.ToolCall{{ID: "call", Type: "function", Function: provider.ToolFunction{Name: "read", Arguments: `{}`}}}}
	model := &scriptedModel{results: []provider.Completion{toolCall, toolCall, {Content: "partial summary", Usage: provider.Usage{TotalTokens: 7}}}}
	result, err := executeLoop(context.Background(), model, loopRequest{UserID: "u", ProviderID: "p", Generation: testGeneration(evolution.StrategyReact, 2), Scope: &fakeScope{}})
	if err != nil || !result.Metrics.ReachedStepLimit || result.Metrics.ModelCalls != 3 || result.Metrics.TotalTokens != 7 {
		t.Fatalf("step budget was not enforced: %#v, %v", result, err)
	}
}

func TestLoopDoesNotExecuteToolWhenDurableStartEventFails(t *testing.T) {
	model := &scriptedModel{results: []provider.Completion{{ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolFunction{Name: "read", Arguments: `{}`}}}}}}
	scope := &fakeScope{}
	journalFailure := errors.New("journal unavailable")
	result, err := executeLoop(context.Background(), model, loopRequest{
		UserID: "u", ProviderID: "p", Generation: testGeneration(evolution.StrategyReact, 2), Scope: scope,
		Emit: func(kind string, _ any) error {
			if kind == "tool.started" {
				return journalFailure
			}
			return nil
		},
	})
	if !errors.Is(err, journalFailure) || scope.executions != 0 || result.Metrics.ToolCalls != 1 {
		t.Fatalf("loop crossed the durable boundary: result=%#v executions=%d err=%v", result, scope.executions, err)
	}
}

func TestLoopTraceIncludesReadableToolDetails(t *testing.T) {
	model := &scriptedModel{results: []provider.Completion{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolFunction{Name: "exec_command", Arguments: `{"cmd":"go test ./...","workdir":"backend"}`}}}},
		{Content: "done"},
	}}
	events := map[string]map[string]any{}
	_, err := executeLoop(context.Background(), model, loopRequest{
		UserID: "u", ProviderID: "p", Generation: testGeneration(evolution.StrategyReact, 3), Scope: &fakeScope{}, DetailedTrace: true,
		Emit: func(kind string, details any) error {
			if item, ok := details.(map[string]any); ok {
				events[kind] = item
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments, ok := events["tool.started"]["arguments"].(map[string]any)
	if !ok || arguments["cmd"] != "go test ./..." {
		t.Fatalf("tool arguments were not retained as readable trace details: %#v", events["tool.started"])
	}
	if _, ok := events["tool.completed"]["result"]; !ok {
		t.Fatalf("tool result preview missing: %#v", events["tool.completed"])
	}
	preview := traceJSONPreview([]byte(`{"apiKey":"private","nested":{"password":"hidden","value":"visible"}}`), 4096).(map[string]any)
	if preview["apiKey"] != "[已隐藏]" || preview["nested"].(map[string]any)["password"] != "[已隐藏]" || preview["nested"].(map[string]any)["value"] != "visible" {
		t.Fatalf("trace secret redaction failed: %#v", preview)
	}
}

func TestLoopTraceOmitsToolPayloadsWhenRunInspectorIsDisabled(t *testing.T) {
	model := &scriptedModel{results: []provider.Completion{
		{ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolFunction{Name: "exec_command", Arguments: `{"cmd":"go test ./..."}`}}}},
		{Content: "done"},
	}}
	events := map[string]map[string]any{}
	_, err := executeLoop(context.Background(), model, loopRequest{
		UserID: "u", ProviderID: "p", Generation: testGeneration(evolution.StrategyReact, 3), Scope: &fakeScope{},
		Emit: func(kind string, details any) error {
			if item, ok := details.(map[string]any); ok {
				events[kind] = item
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := events["tool.started"]["arguments"]; ok {
		t.Fatalf("disabled run inspector retained tool arguments: %#v", events["tool.started"])
	}
	if _, ok := events["tool.completed"]["result"]; ok {
		t.Fatalf("disabled run inspector retained tool result: %#v", events["tool.completed"])
	}
}
