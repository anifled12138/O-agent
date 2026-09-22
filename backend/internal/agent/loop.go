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
	UserID        string
	ProviderID    string
	Generation    domain.AgentGeneration
	Messages      []provider.ChatMessage
	Scope         loopScope
	Emit          func(string, any) error
	DetailedTrace bool
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
		emit = func(string, any) error { return nil }
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
		if err := emit("planner.requested", map[string]any{"generationId": request.Generation.ID, "messageCount": len(plannerMessages)}); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		plannerStarted := time.Now()
		completion, err := models.CompleteWithTools(ctx, request.UserID, request.ProviderID, plannerMessages, nil)
		metrics.ModelCalls++
		addUsage(&metrics, completion.Usage)
		if err != nil {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			_ = emit("planner.failed", map[string]any{"error": err.Error()})
			return loopResult{Metrics: metrics}, err
		}
		if err := emitCompatibilityWarnings(emit, completion.Warnings, 0); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		brief := strings.TrimSpace(completion.Content)
		if brief == "" {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			return loopResult{Metrics: metrics}, fmt.Errorf("planner returned an empty execution brief")
		}
		if err := emit("planner.completed", map[string]any{"contentBytes": len(brief), "model": completion.Model, "durationMillis": time.Since(plannerStarted).Milliseconds()}); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		messages = append(messages, provider.ChatMessage{Role: "system", Content: "Execution brief from the planning stage (advisory; verify against observations):\n" + brief})
	}

	maxSteps := spec.MaxSteps
	if maxSteps < 1 {
		maxSteps = 12
	}
	for step := 0; step < maxSteps; step++ {
		definitions := request.Scope.definitions()
		if err := emit("model.requested", map[string]any{"step": step + 1, "generationId": request.Generation.ID, "definitionDigest": request.Generation.DefinitionDigest, "strategy": spec.Strategy, "messageCount": len(messages), "toolDefinitionCount": len(definitions)}); err != nil {
			return loopResult{Metrics: metrics}, err
		}

		sendMessages := messages
		if cp, ok := request.Scope.(interface {
			prepareTurnMessages([]provider.ChatMessage, int) ([]provider.ChatMessage, turnCompaction)
		}); ok {
			prepared, compaction := cp.prepareTurnMessages(messages, step)
			sendMessages = prepared
			if compaction.Applied {
				// Adopt the compacted transcript as canonical for the remainder of
				// this turn; otherwise every step would re-send the discarded bytes.
				messages = prepared
				if err := emit("context.compacted", map[string]any{
					"scope":          "turn",
					"step":           step + 1,
					"forced":         compaction.Forced,
					"originalChars":  compaction.OriginalChars,
					"compactedChars": compaction.CompactedChars,
				}); err != nil {
					return loopResult{Metrics: metrics}, err
				}
			}
		}

		modelStarted := time.Now()
		completion, err := models.CompleteWithTools(ctx, request.UserID, request.ProviderID, sendMessages, definitions)
		metrics.ModelCalls++
		addUsage(&metrics, completion.Usage)
		if err != nil {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			_ = emit("model.failed", map[string]any{"step": step + 1, "error": err.Error()})
			return loopResult{Metrics: metrics}, err
		}
		if err := emitCompatibilityWarnings(emit, completion.Warnings, step+1); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		if err := emit("model.completed", map[string]any{"step": step + 1, "toolCallCount": len(completion.ToolCalls), "contentBytes": len(completion.Content), "model": completion.Model, "usage": completion.Usage, "durationMillis": time.Since(modelStarted).Milliseconds()}); err != nil {
			return loopResult{Metrics: metrics}, err
		}
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
		callIDs := make([]string, 0, len(completion.ToolCalls))
		for _, call := range completion.ToolCalls {
			callIDs = append(callIDs, call.ID)
		}
		if err := emit("tools.dispatched", map[string]any{"step": step + 1, "toolCallCount": len(completion.ToolCalls), "toolCallIds": callIDs}); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		for _, call := range completion.ToolCalls {
			metrics.ToolCalls++
			toolStarted := time.Now()
			startedDetails := map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "argumentBytes": len(call.Function.Arguments)}
			if request.DetailedTrace {
				startedDetails["arguments"] = traceJSONPreview([]byte(call.Function.Arguments), 16*1024)
			}
			if err := emit("tool.started", startedDetails); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			result := request.Scope.execute(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments))
			completedDetails := map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "resultBytes": len(result), "durationMillis": time.Since(toolStarted).Milliseconds(), "ok": toolResultOK(result)}
			if request.DetailedTrace {
				completedDetails["result"] = traceJSONPreview(result, 64*1024)
			}
			if err := emit("tool.completed", completedDetails); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: string(result)})
		}
		if err := emit("tools.completed", map[string]any{"step": step + 1, "toolCallCount": len(completion.ToolCalls)}); err != nil {
			return loopResult{Metrics: metrics}, err
		}
	}
	metrics.ReachedStepLimit = true
	metrics.DurationMillis = time.Since(started).Milliseconds()
	if err := emit("loop.step_limit_reached", map[string]any{"maxSteps": maxSteps}); err != nil {
		return loopResult{Metrics: metrics}, err
	}

	summaryMessages := append(messages, provider.ChatMessage{
		Role:    "user",
		Content: "[System Notice] Your step budget has been reached. Do not invoke any tools. Summarize what you have completed so far, what was changed, and report current progress or next steps directly to the user.",
	})
	finalSummaryComp, err := models.CompleteWithTools(ctx, request.UserID, request.ProviderID, summaryMessages, nil)
	metrics.ModelCalls++
	addUsage(&metrics, finalSummaryComp.Usage)
	finalSummary := finalSummaryComp.Content
	metrics.DurationMillis = time.Since(started).Milliseconds()
	if err == nil && strings.TrimSpace(finalSummary) != "" {
		return loopResult{Reply: strings.TrimSpace(finalSummary), Metrics: metrics}, nil
	}

	return loopResult{Reply: fmt.Sprintf("Step budget reached maximum limit (%d steps). Completed observations are preserved; please continue or refine your request.", maxSteps), Metrics: metrics}, nil
}

func emitCompatibilityWarnings(emit func(string, any) error, warnings []provider.CompatibilityWarning, step int) error {
	for _, warning := range warnings {
		if err := emit("provider.compatibility_warning", map[string]any{"step": step, "code": warning.Code, "message": warning.Message}); err != nil {
			return err
		}
	}
	return nil
}

func addUsage(target *domain.RunMetrics, usage provider.Usage) {
	target.PromptTokens += usage.PromptTokens
	target.CompletionTokens += usage.CompletionTokens
	target.TotalTokens += usage.TotalTokens
}

func traceJSONPreview(raw []byte, maxBytes int) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		value = redactTraceValue(value)
		encoded, _ := json.Marshal(value)
		if maxBytes > 0 && len(encoded) > maxBytes {
			return string(encoded[:maxBytes]) + "\n…（已截断）"
		}
		return value
	}
	if maxBytes > 0 && len(raw) > maxBytes {
		return string(raw[:maxBytes]) + "\n…（已截断）"
	}
	return string(raw)
}

func redactTraceValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(item))
		for key, child := range item {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "apikey") || strings.Contains(lower, "api_key") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") || strings.Contains(lower, "credential") {
				redacted[key] = "[已隐藏]"
				continue
			}
			redacted[key] = redactTraceValue(child)
		}
		return redacted
	case []any:
		redacted := make([]any, len(item))
		for index, child := range item {
			redacted[index] = redactTraceValue(child)
		}
		return redacted
	default:
		return value
	}
}
