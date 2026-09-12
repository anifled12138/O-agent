package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/provider"
)

type modelRuntime interface {
	CompleteWithTools(context.Context, string, string, []provider.ChatMessage, []provider.ToolDefinition) (provider.Completion, error)
}

type loopScope interface {
	definitions() []provider.ToolDefinition
	execute(context.Context, string, json.RawMessage) json.RawMessage
}

type loopRequest struct {
	UserID     string
	ProviderID string
	Generation domain.AgentGeneration
	Messages   []provider.ChatMessage
	Scope      loopScope
	Emit       func(string, any)
}

type loopResult struct {
	Reply   string
	Metrics domain.RunMetrics
}

func executeLoop(ctx context.Context, models modelRuntime, request loopRequest) (loopResult, error) {
	started := time.Now()
	metrics := domain.RunMetrics{}
	emit := request.Emit
	if emit == nil {
		emit = func(string, any) {}
	}
	messages := append([]provider.ChatMessage(nil), request.Messages...)
	spec := request.Generation.Definition.Spec
	if spec.Strategy == evolution.StrategyPlanReact {
		plannerMessages := make([]provider.ChatMessage, 0, len(messages)+1)
		plannerMessages = append(plannerMessages, provider.ChatMessage{Role: "system", Content: spec.PlannerPrompt})
		for _, message := range messages {
			if message.Role != "system" {
				plannerMessages = append(plannerMessages, message)
			}
		}
		emit("planner.requested", map[string]any{"generationId": request.Generation.ID, "messageCount": len(plannerMessages)})
		completion, err := models.CompleteWithTools(ctx, request.UserID, request.ProviderID, plannerMessages, nil)
		metrics.ModelCalls++
		addUsage(&metrics, completion.Usage)
		if err != nil {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			emit("planner.failed", map[string]any{"error": err.Error()})
			return loopResult{Metrics: metrics}, err
		}
		emitCompatibilityWarnings(emit, completion.Warnings, 0)
		brief := strings.TrimSpace(completion.Content)
		if brief == "" {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			return loopResult{Metrics: metrics}, fmt.Errorf("planner returned an empty execution brief")
		}
		emit("planner.completed", map[string]any{"contentBytes": len(brief), "model": completion.Model})
		messages = append(messages, provider.ChatMessage{Role: "system", Content: "Execution brief from the planning stage (advisory; verify against observations):\n" + brief})
	}

	maxSteps := spec.MaxSteps
	if maxSteps < 1 {
		maxSteps = 12
	}
	for step := 0; step < maxSteps; step++ {
		definitions := request.Scope.definitions()
		emit("model.requested", map[string]any{"step": step + 1, "generationId": request.Generation.ID, "definitionDigest": request.Generation.DefinitionDigest, "strategy": spec.Strategy, "messageCount": len(messages), "toolDefinitionCount": len(definitions)})
		completion, err := models.CompleteWithTools(ctx, request.UserID, request.ProviderID, messages, definitions)
		metrics.ModelCalls++
		addUsage(&metrics, completion.Usage)
		if err != nil {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			emit("model.failed", map[string]any{"step": step + 1, "error": err.Error()})
			return loopResult{Metrics: metrics}, err
		}
		emitCompatibilityWarnings(emit, completion.Warnings, step+1)
		emit("model.completed", map[string]any{"step": step + 1, "toolCallCount": len(completion.ToolCalls), "contentBytes": len(completion.Content), "model": completion.Model, "usage": completion.Usage})
		if len(completion.ToolCalls) == 0 {
			reply := strings.TrimSpace(completion.Content)
			if reply == "" {
				metrics.DurationMillis = time.Since(started).Milliseconds()
				return loopResult{Metrics: metrics}, fmt.Errorf("agent loop returned an empty reply")
			}
			metrics.DurationMillis = time.Since(started).Milliseconds()
			return loopResult{Reply: reply, Metrics: metrics}, nil
		}
		messages = append(messages, provider.ChatMessage{Role: "assistant", Content: completion.Content, ToolCalls: completion.ToolCalls})
		for _, call := range completion.ToolCalls {
			metrics.ToolCalls++
			toolStarted := time.Now()
			emit("tool.started", map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "argumentBytes": len(call.Function.Arguments)})
			result := request.Scope.execute(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments))
			emit("tool.completed", map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "resultBytes": len(result), "durationMillis": time.Since(toolStarted).Milliseconds(), "ok": toolResultOK(result)})
			messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: string(result)})
		}
	}
	metrics.ReachedStepLimit = true
	metrics.DurationMillis = time.Since(started).Milliseconds()
	return loopResult{Reply: "I reached this generation's execution-step limit. Completed observations are preserved; continue the mission or create a frontier challenge to test a stronger agent generation.", Metrics: metrics}, nil
}

func emitCompatibilityWarnings(emit func(string, any), warnings []provider.CompatibilityWarning, step int) {
	for _, warning := range warnings {
		emit("provider.compatibility_warning", map[string]any{"step": step, "code": warning.Code, "message": warning.Message})
	}
}

func addUsage(metrics *domain.RunMetrics, usage provider.Usage) {
	metrics.PromptTokens += usage.PromptTokens
	metrics.CompletionTokens += usage.CompletionTokens
	metrics.TotalTokens += usage.TotalTokens
}
