package agent

import (
	"context"
	"encoding/json"
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
	model := &scriptedModel{results: []provider.Completion{toolCall, toolCall}}
	result, err := executeLoop(context.Background(), model, loopRequest{UserID: "u", ProviderID: "p", Generation: testGeneration(evolution.StrategyReact, 2), Scope: &fakeScope{}})
	if err != nil || !result.Metrics.ReachedStepLimit || result.Metrics.ModelCalls != 2 {
		t.Fatalf("step budget was not enforced: %#v, %v", result, err)
	}
}
