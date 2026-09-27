package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/permissions"
	"axiom.local/agent/internal/provider"
)

type modelRuntime interface {
	CompleteWithTools(context.Context, string, string, []provider.ChatMessage, []provider.ToolDefinition) (provider.Completion, error)
}

type loopScope interface {
	definitions() []provider.ToolDefinition
	execute(context.Context, string, json.RawMessage) json.RawMessage
}

type toolAuthorization interface {
	authorizeTool(string, json.RawMessage) (permissions.Decision, permissions.Request)
	requestToolApproval(context.Context, string, permissions.Request, permissions.Decision, json.RawMessage) (bool, error)
}

type loopRequest struct {
	UserID             string
	ProviderID         string
	Generation         domain.AgentGeneration
	Messages           []provider.ChatMessage
	Checkpoint         json.RawMessage
	RuntimeFingerprint string
	Scope              loopScope
	Emit               func(string, any) error
	PersistCheckpoint  func(string, any, loopCheckpoint) error
	DetailedTrace      bool
	TokenBudget        int
	ModelCallBudget    int
}

type loopResult struct {
	Reply   string
	Metrics domain.RunMetrics
}

type loopCheckpoint struct {
	Version              int                            `json:"version"`
	ProviderID           string                         `json:"providerId"`
	GenerationID         string                         `json:"generationId"`
	RuntimeFingerprint   string                         `json:"runtimeFingerprint"`
	ResumeAllowed        bool                           `json:"resumeAllowed"`
	Stage                string                         `json:"stage"`
	Messages             []provider.ChatMessage         `json:"messages"`
	NextStep             int                            `json:"nextStep"`
	PendingStep          int                            `json:"pendingStep,omitempty"`
	PendingCalls         []provider.ToolCall            `json:"pendingCalls,omitempty"`
	ExpectedToolBindings map[string]permissions.Request `json:"expectedToolBindings,omitempty"`
	PendingIndex         int                            `json:"pendingIndex,omitempty"`
	InFlightModelCall    bool                           `json:"inFlightModelCall,omitempty"`
	FinalReply           string                         `json:"finalReply,omitempty"`
	Metrics              domain.RunMetrics              `json:"metrics"`
}

func executeLoop(ctx context.Context, models modelRuntime, request loopRequest) (loopResult, error) {
	started := time.Now()
	metrics := domain.RunMetrics{}
	emit := request.Emit
	if emit == nil {
		emit = func(string, any) error { return nil }
	}
	spec := request.Generation.Definition.Spec
	checkpoint := loopCheckpoint{Version: 1, ProviderID: request.ProviderID, GenerationID: request.Generation.DefinitionDigest, RuntimeFingerprint: request.RuntimeFingerprint, ResumeAllowed: true, Stage: "loop", Messages: append([]provider.ChatMessage(nil), request.Messages...)}
	if spec.Strategy == evolution.StrategyPlanReact {
		checkpoint.Stage = "plan"
	}
	if len(request.Checkpoint) > 0 {
		if err := json.Unmarshal(request.Checkpoint, &checkpoint); err != nil {
			return loopResult{}, fmt.Errorf("decode agent continuation checkpoint: %w", err)
		}
		if checkpoint.Version != 1 || checkpoint.ProviderID != request.ProviderID || checkpoint.GenerationID != request.Generation.DefinitionDigest || checkpoint.RuntimeFingerprint == "" || checkpoint.RuntimeFingerprint != request.RuntimeFingerprint || !checkpoint.ResumeAllowed || checkpoint.Stage == "" {
			return loopResult{}, fmt.Errorf("agent continuation checkpoint is incompatible with the bound provider or generation")
		}
		if checkpoint.Messages == nil {
			return loopResult{}, fmt.Errorf("agent continuation checkpoint has no message transcript")
		}
		metrics = checkpoint.Metrics
	} else if request.PersistCheckpoint != nil {
		if err := persistLoopCheckpoint(request, emit, "turn.checkpointed", map[string]any{"version": checkpoint.Version, "generationId": checkpoint.GenerationID}, checkpoint); err != nil {
			return loopResult{Metrics: metrics}, err
		}
	}
	if checkpoint.Stage == "final" {
		metrics.DurationMillis = time.Since(started).Milliseconds()
		return loopResult{Reply: checkpoint.FinalReply, Metrics: metrics}, nil
	}
	messages := append([]provider.ChatMessage(nil), checkpoint.Messages...)
	if checkpoint.Stage == "plan" && spec.Strategy == evolution.StrategyPlanReact {
		plannerMessages := make([]provider.ChatMessage, 0, len(messages)+1)
		plannerMessages = append(plannerMessages, provider.ChatMessage{Role: "system", Content: spec.PlannerPrompt})
		for _, message := range messages {
			if message.Role != "system" {
				plannerMessages = append(plannerMessages, message)
			}
		}
		if modelCallBudgetReached(request, metrics) {
			return modelCallLimitResult(emit, request, metrics, started, "planner")
		}
		plannerStarted := time.Now()
		requestCtx := modelRequestContext(ctx, request, metrics)
		checkpoint.InFlightModelCall = true
		checkpoint.Metrics = metrics
		checkpoint.Metrics.ModelCalls += reservedModelAttempts(request, metrics)
		if err := persistLoopCheckpoint(request, emit, "planner.requested", map[string]any{"generationId": request.Generation.ID, "messageCount": len(plannerMessages)}, checkpoint); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		completion, err := models.CompleteWithTools(requestCtx, request.UserID, request.ProviderID, plannerMessages, nil)
		metrics.ModelCalls += modelAttemptCount(completion, err)
		checkpoint.InFlightModelCall = false
		addUsage(&metrics, completion.Usage)
		if err != nil {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			_ = emit("planner.failed", map[string]any{"attempts": modelAttemptCount(completion, err), "error": err.Error()})
			return loopResult{Metrics: metrics}, err
		}
		if request.TokenBudget > 0 && metrics.TotalTokens >= request.TokenBudget {
			metrics.ReachedTokenLimit = true
			metrics.DurationMillis = time.Since(started).Milliseconds()
			if err := emit("loop.token_limit_reached", map[string]any{"budget": request.TokenBudget, "used": metrics.TotalTokens, "stage": "planner"}); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			return loopResult{Reply: "本次运行达到模型 Token 预算上限，任务尚未进入执行阶段。", Metrics: metrics}, nil
		}
		if modelCallBudgetReached(request, metrics) {
			return modelCallLimitResult(emit, request, metrics, started, "planner")
		}
		if err := emitCompatibilityWarnings(emit, completion.Warnings, 0); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		brief := strings.TrimSpace(completion.Content)
		if brief == "" {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			return loopResult{Metrics: metrics}, fmt.Errorf("planner returned an empty execution brief")
		}
		messages = append(messages, provider.ChatMessage{Role: "system", Content: "Execution brief from the planning stage (advisory; verify against observations):\n" + brief})
		checkpoint.Stage = "loop"
		checkpoint.Messages = append([]provider.ChatMessage(nil), messages...)
		checkpoint.Metrics = metrics
		if err := persistLoopCheckpoint(request, emit, "planner.completed", map[string]any{"attempts": modelAttemptCount(completion, nil), "contentBytes": len(brief), "model": completion.Model, "durationMillis": time.Since(plannerStarted).Milliseconds()}, checkpoint); err != nil {
			return loopResult{Metrics: metrics}, err
		}
	}

	maxSteps := spec.MaxSteps
	if maxSteps < 1 {
		maxSteps = 12
	}
	for step := checkpoint.NextStep; step < maxSteps; step++ {
		var calls []provider.ToolCall
		startCall := 0
		if checkpoint.Stage == "dispatch" {
			if checkpoint.PendingStep != step+1 || checkpoint.PendingIndex < 0 || checkpoint.PendingIndex > len(checkpoint.PendingCalls) {
				return loopResult{Metrics: metrics}, fmt.Errorf("agent continuation checkpoint has an invalid pending tool step")
			}
			if modelCallBudgetReached(request, metrics) {
				return modelCallLimitResult(emit, request, metrics, started, "tool_dispatch")
			}
			calls = append([]provider.ToolCall(nil), checkpoint.PendingCalls...)
			startCall = checkpoint.PendingIndex
		} else if modelCallBudgetReached(request, metrics) {
			return modelCallLimitResult(emit, request, metrics, started, "agent")
		} else {
			definitions := request.Scope.definitions()
			sendMessages := messages
			if cp, ok := request.Scope.(interface {
				prepareTurnMessages([]provider.ChatMessage, int) ([]provider.ChatMessage, turnCompaction)
			}); ok {
				prepared, compaction := cp.prepareTurnMessages(messages, step)
				sendMessages = prepared
				if compaction.Applied {
					messages = prepared
					checkpoint.Messages = append([]provider.ChatMessage(nil), messages...)
					checkpoint.Stage = "loop"
					checkpoint.NextStep = step
					checkpoint.Metrics = metrics
					if err := persistLoopCheckpoint(request, emit, "context.compacted", map[string]any{"scope": "turn", "step": step + 1, "forced": compaction.Forced, "originalChars": compaction.OriginalChars, "compactedChars": compaction.CompactedChars}, checkpoint); err != nil {
						return loopResult{Metrics: metrics}, err
					}
				}
			}
			modelStarted := time.Now()
			requestCtx := modelRequestContext(ctx, request, metrics)
			checkpoint.Stage, checkpoint.NextStep = "loop", step
			checkpoint.Messages = append([]provider.ChatMessage(nil), messages...)
			checkpoint.InFlightModelCall = true
			checkpoint.Metrics = metrics
			checkpoint.Metrics.ModelCalls += reservedModelAttempts(request, metrics)
			requestedDetails := map[string]any{"step": step + 1, "generationId": request.Generation.ID, "definitionDigest": request.Generation.DefinitionDigest, "strategy": spec.Strategy, "messageCount": len(messages), "toolDefinitionCount": len(definitions)}
			if err := persistLoopCheckpoint(request, emit, "model.requested", requestedDetails, checkpoint); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			completion, err := models.CompleteWithTools(requestCtx, request.UserID, request.ProviderID, sendMessages, definitions)
			metrics.ModelCalls += modelAttemptCount(completion, err)
			checkpoint.InFlightModelCall = false
			addUsage(&metrics, completion.Usage)
			if err != nil {
				metrics.DurationMillis = time.Since(started).Milliseconds()
				_ = emit("model.failed", map[string]any{"step": step + 1, "attempts": modelAttemptCount(completion, err), "error": err.Error()})
				return loopResult{Metrics: metrics}, err
			}
			if err := emitCompatibilityWarnings(emit, completion.Warnings, step+1); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			toolCalls := completion.ToolCalls
			reply := strings.TrimSpace(completion.Content)
			if request.TokenBudget > 0 && metrics.TotalTokens >= request.TokenBudget {
				metrics.ReachedTokenLimit = true
				if err := emit("loop.token_limit_reached", map[string]any{"budget": request.TokenBudget, "used": metrics.TotalTokens, "stage": "agent", "step": step + 1}); err != nil {
					return loopResult{Metrics: metrics}, err
				}
				if len(toolCalls) > 0 || reply == "" {
					reply = "本次运行达到模型 Token 预算上限，工具调用未执行，任务尚未确认完成。"
				}
				checkpoint.Stage, checkpoint.FinalReply = "final", reply
				checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
				if err := persistLoopCheckpoint(request, emit, "model.completed", map[string]any{"step": step + 1, "attempts": modelAttemptCount(completion, nil), "toolCallCount": len(toolCalls), "contentBytes": len(completion.Content), "model": completion.Model, "usage": completion.Usage, "durationMillis": time.Since(modelStarted).Milliseconds()}, checkpoint); err != nil {
					return loopResult{Metrics: metrics}, err
				}
				metrics.DurationMillis = time.Since(started).Milliseconds()
				return loopResult{Reply: reply, Metrics: metrics}, nil
			}
			if len(toolCalls) > 0 && modelCallBudgetReached(request, metrics) {
				checkpoint.Stage, checkpoint.PendingStep = "dispatch", step+1
				checkpoint.PendingCalls, checkpoint.PendingIndex = append([]provider.ToolCall(nil), toolCalls...), 0
				checkpoint.ExpectedToolBindings = expectedToolBindings(request.Scope, toolCalls)
				if hasMutableToolBinding(checkpoint.ExpectedToolBindings) {
					checkpoint.ResumeAllowed = false
				}
				checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
				if err := persistLoopCheckpoint(request, emit, "model.completed", map[string]any{"step": step + 1, "attempts": modelAttemptCount(completion, nil), "toolCallCount": len(toolCalls), "contentBytes": len(completion.Content), "model": completion.Model, "usage": completion.Usage, "durationMillis": time.Since(modelStarted).Milliseconds()}, checkpoint); err != nil {
					return loopResult{Metrics: metrics}, err
				}
				return modelCallLimitResult(emit, request, metrics, started, "tool_dispatch")
			}
			if len(toolCalls) == 0 {
				if reply == "" {
					metrics.DurationMillis = time.Since(started).Milliseconds()
					return loopResult{Metrics: metrics}, fmt.Errorf("agent loop returned an empty reply")
				}
				checkpoint.Stage, checkpoint.FinalReply = "final", reply
				checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
				if err := persistLoopCheckpoint(request, emit, "model.completed", map[string]any{"step": step + 1, "attempts": modelAttemptCount(completion, nil), "toolCallCount": 0, "contentBytes": len(completion.Content), "model": completion.Model, "usage": completion.Usage, "durationMillis": time.Since(modelStarted).Milliseconds()}, checkpoint); err != nil {
					return loopResult{Metrics: metrics}, err
				}
				metrics.DurationMillis = time.Since(started).Milliseconds()
				return loopResult{Reply: reply, Metrics: metrics}, nil
			}
			messages = append(messages, provider.ChatMessage{Role: "assistant", Content: completion.Content, ToolCalls: toolCalls})
			calls = append([]provider.ToolCall(nil), toolCalls...)
			checkpoint.Stage, checkpoint.PendingStep = "dispatch", step+1
			checkpoint.PendingCalls, checkpoint.PendingIndex = append([]provider.ToolCall(nil), toolCalls...), 0
			checkpoint.ExpectedToolBindings = expectedToolBindings(request.Scope, toolCalls)
			if hasMutableToolBinding(checkpoint.ExpectedToolBindings) {
				checkpoint.ResumeAllowed = false
			}
			checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
			callIDs := make([]string, 0, len(toolCalls))
			for _, call := range toolCalls {
				callIDs = append(callIDs, call.ID)
			}
			if err := persistLoopCheckpoint(request, emit, "model.completed", map[string]any{"step": step + 1, "attempts": modelAttemptCount(completion, nil), "toolCallCount": len(toolCalls), "contentBytes": len(completion.Content), "model": completion.Model, "usage": completion.Usage, "durationMillis": time.Since(modelStarted).Milliseconds()}, checkpoint); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			if err := emit("tools.dispatched", map[string]any{"step": step + 1, "toolCallCount": len(toolCalls), "toolCallIds": callIDs}); err != nil {
				return loopResult{Metrics: metrics}, err
			}
		}
		for callIndex := startCall; callIndex < len(calls); callIndex++ {
			call := calls[callIndex]
			if err := ctx.Err(); err != nil {
				return loopResult{Metrics: metrics}, context.Cause(ctx)
			}
			metrics.ToolCalls++
			arguments := json.RawMessage(call.Function.Arguments)
			authRequest := permissions.Request{}
			if auth, ok := request.Scope.(toolAuthorization); ok {
				decision, classified := auth.authorizeTool(call.Function.Name, arguments)
				authRequest = classified
				expected, bound := checkpoint.ExpectedToolBindings[call.ID]
				if !bound || expected != authRequest {
					return loopResult{Metrics: metrics}, fmt.Errorf("tool authorization binding changed for call %q", call.ID)
				}
				if err := emit("permission.checked", map[string]any{"step": step + 1, "toolCallId": call.ID, "tool": call.Function.Name, "source": authRequest.Source, "pluginId": authRequest.PluginID, "releaseId": authRequest.ReleaseID, "effect": authRequest.Effect, "resource": authRequest.Resource, "outcome": decision.Outcome, "reason": decision.Reason, "profile": authRequest.Profile}); err != nil {
					return loopResult{Metrics: metrics}, err
				}
				if decision.Outcome == permissions.OutcomeAsk {
					approved, err := auth.requestToolApproval(ctx, call.ID, authRequest, decision, arguments)
					if err != nil {
						return loopResult{Metrics: metrics}, err
					}
					if !approved {
						result, _ := json.Marshal(map[string]any{"ok": false, "error": "用户拒绝了此工具调用", "authorization": "denied"})
						messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: string(result)})
						checkpoint.PendingIndex, checkpoint.Messages, checkpoint.Metrics = callIndex+1, append([]provider.ChatMessage(nil), messages...), metrics
						if err := persistLoopCheckpoint(request, emit, "tool.authorization_denied", map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name}, checkpoint); err != nil {
							return loopResult{Metrics: metrics}, err
						}
						continue
					}
				} else if decision.Outcome != permissions.OutcomeAllow {
					result, _ := json.Marshal(map[string]any{"ok": false, "error": decision.Reason, "authorization": "denied"})
					messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: string(result)})
					checkpoint.PendingIndex, checkpoint.Messages, checkpoint.Metrics = callIndex+1, append([]provider.ChatMessage(nil), messages...), metrics
					if err := persistLoopCheckpoint(request, emit, "tool.authorization_denied", map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "reason": decision.Reason}, checkpoint); err != nil {
						return loopResult{Metrics: metrics}, err
					}
					continue
				}
			}
			if err := ctx.Err(); err != nil {
				return loopResult{Metrics: metrics}, context.Cause(ctx)
			}
			toolStarted := time.Now()
			startedDetails := map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "source": authRequest.Source, "pluginId": authRequest.PluginID, "releaseId": authRequest.ReleaseID, "effect": authRequest.Effect, "argumentBytes": len(call.Function.Arguments)}
			if request.DetailedTrace {
				startedDetails["arguments"] = traceJSONPreview([]byte(call.Function.Arguments), 16*1024)
			}
			if err := emit("tool.started", startedDetails); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			if err := ctx.Err(); err != nil {
				return loopResult{Metrics: metrics}, context.Cause(ctx)
			}
			result := request.Scope.execute(ctx, call.Function.Name, arguments)
			switch call.Function.Name {
			case "axiom_fragment_create", "axiom_fragment_invoke", "axiom_fragment_drop", "axiom_plugin_install", "axiom_plugin_rollback", "axiom_plugin_mark_unusable", "exec_script":
				// These actions mutate turn-local or live plugin routing state. A later
				// process cannot reconstruct the exact in-memory routing snapshot.
				checkpoint.ResumeAllowed = false
			}
			if containsRunfileArtifact(arguments) || containsRunfileArtifact(result) {
				checkpoint.ResumeAllowed = false
			}
			completedDetails := map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "source": authRequest.Source, "pluginId": authRequest.PluginID, "releaseId": authRequest.ReleaseID, "resultBytes": len(result), "durationMillis": time.Since(toolStarted).Milliseconds(), "ok": toolResultOK(result)}
			if request.DetailedTrace {
				completedDetails["result"] = traceJSONPreview(result, 64*1024)
			}
			messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: string(result)})
			checkpoint.PendingIndex, checkpoint.Messages, checkpoint.Metrics = callIndex+1, append([]provider.ChatMessage(nil), messages...), metrics
			if err := persistLoopCheckpoint(request, emit, "tool.completed", completedDetails, checkpoint); err != nil {
				return loopResult{Metrics: metrics}, err
			}
		}
		checkpoint.Stage, checkpoint.NextStep = "loop", step+1
		checkpoint.PendingStep, checkpoint.PendingCalls, checkpoint.PendingIndex = 0, nil, 0
		checkpoint.ExpectedToolBindings = nil
		checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
		if err := persistLoopCheckpoint(request, emit, "tools.completed", map[string]any{"step": step + 1, "toolCallCount": len(calls)}, checkpoint); err != nil {
			return loopResult{Metrics: metrics}, err
		}
	}
	metrics.ReachedStepLimit = true
	metrics.DurationMillis = time.Since(started).Milliseconds()
	if err := emit("loop.step_limit_reached", map[string]any{"maxSteps": maxSteps}); err != nil {
		return loopResult{Metrics: metrics}, err
	}

	if modelCallBudgetReached(request, metrics) {
		return modelCallLimitResult(emit, request, metrics, started, "step_limit_summary")
	}
	summaryMessages := append(messages, provider.ChatMessage{
		Role:    "user",
		Content: "[System Notice] Your step budget has been reached. Do not invoke any tools. Summarize what you have completed so far, what was changed, and report current progress or next steps directly to the user.",
	})
	summaryStarted := time.Now()
	requestCtx := modelRequestContext(ctx, request, metrics)
	checkpoint.InFlightModelCall = true
	checkpoint.Metrics = metrics
	checkpoint.Metrics.ModelCalls += reservedModelAttempts(request, metrics)
	if err := persistLoopCheckpoint(request, emit, "summary.requested", map[string]any{"stage": "step_limit", "messageCount": len(summaryMessages)}, checkpoint); err != nil {
		return loopResult{Metrics: metrics}, err
	}
	finalSummaryComp, err := models.CompleteWithTools(requestCtx, request.UserID, request.ProviderID, summaryMessages, nil)
	metrics.ModelCalls += modelAttemptCount(finalSummaryComp, err)
	checkpoint.InFlightModelCall = false
	addUsage(&metrics, finalSummaryComp.Usage)
	if request.TokenBudget > 0 && metrics.TotalTokens >= request.TokenBudget {
		metrics.ReachedTokenLimit = true
		if err := emit("loop.token_limit_reached", map[string]any{"budget": request.TokenBudget, "used": metrics.TotalTokens, "stage": "step_limit_summary"}); err != nil {
			return loopResult{Metrics: metrics}, err
		}
	}
	finalSummary := finalSummaryComp.Content
	metrics.DurationMillis = time.Since(started).Milliseconds()
	if err != nil {
		if emitErr := emit("summary.failed", map[string]any{"attempts": modelAttemptCount(finalSummaryComp, err), "stage": "step_limit", "error": err.Error()}); emitErr != nil {
			return loopResult{Metrics: metrics}, errors.Join(err, emitErr)
		}
	}
	if err == nil && strings.TrimSpace(finalSummary) != "" {
		reply := strings.TrimSpace(finalSummary)
		checkpoint.Stage, checkpoint.FinalReply, checkpoint.Messages, checkpoint.Metrics = "final", reply, append([]provider.ChatMessage(nil), summaryMessages...), metrics
		if persistErr := persistLoopCheckpoint(request, emit, "summary.completed", map[string]any{"attempts": modelAttemptCount(finalSummaryComp, nil), "stage": "step_limit", "model": finalSummaryComp.Model, "usage": finalSummaryComp.Usage, "durationMillis": time.Since(summaryStarted).Milliseconds()}, checkpoint); persistErr != nil {
			return loopResult{Metrics: metrics}, persistErr
		}
		return loopResult{Reply: reply, Metrics: metrics}, nil
	}

	return loopResult{Reply: fmt.Sprintf("Step budget reached maximum limit (%d steps). Completed observations are preserved; please continue or refine your request.", maxSteps), Metrics: metrics}, nil
}

func containsRunfileArtifact(raw []byte) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(current any) bool {
		switch item := current.(type) {
		case map[string]any:
			for key, child := range item {
				lower := strings.ToLower(key)
				if strings.Contains(lower, "artifactid") {
					if text, ok := child.(string); ok && text != "" {
						return true
					}
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range item {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func expectedToolBindings(scope loopScope, calls []provider.ToolCall) map[string]permissions.Request {
	authorizer, ok := scope.(toolAuthorization)
	if !ok {
		return map[string]permissions.Request{}
	}
	result := make(map[string]permissions.Request, len(calls))
	for _, call := range calls {
		_, binding := authorizer.authorizeTool(call.Function.Name, json.RawMessage(call.Function.Arguments))
		result[call.ID] = binding
	}
	return result
}

func hasMutableToolBinding(bindings map[string]permissions.Request) bool {
	for _, binding := range bindings {
		if binding.Source == "mcp_or_plugin" || ((binding.Source == "plugin" || binding.Source == "capsule") && binding.ReleaseID == "") {
			return true
		}
	}
	return false
}

func persistLoopCheckpoint(request loopRequest, emit func(string, any) error, kind string, details any, checkpoint loopCheckpoint) error {
	if request.PersistCheckpoint == nil {
		return emit(kind, details)
	}
	return request.PersistCheckpoint(kind, details, checkpoint)
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

func modelAttemptCount(completion provider.Completion, err error) int {
	if completion.Attempts > 0 {
		return completion.Attempts
	}
	var providerErr *provider.ProviderError
	if errors.As(err, &providerErr) && providerErr.Attempts > 0 {
		return providerErr.Attempts
	}
	return 1
}

func modelCallBudgetReached(request loopRequest, metrics domain.RunMetrics) bool {
	return request.ModelCallBudget > 0 && metrics.ModelCalls >= request.ModelCallBudget
}

func reservedModelAttempts(request loopRequest, metrics domain.RunMetrics) int {
	reserve := provider.MaxCompletionAttempts
	if request.ModelCallBudget > 0 {
		remaining := request.ModelCallBudget - metrics.ModelCalls
		if remaining < reserve {
			reserve = remaining
		}
	}
	if reserve < 1 {
		return 1
	}
	return reserve
}

func modelRequestContext(ctx context.Context, request loopRequest, metrics domain.RunMetrics) context.Context {
	if request.ModelCallBudget <= 0 {
		return ctx
	}
	return provider.WithAttemptBudget(ctx, request.ModelCallBudget-metrics.ModelCalls)
}

func modelCallLimitResult(emit func(string, any) error, request loopRequest, metrics domain.RunMetrics, started time.Time, stage string) (loopResult, error) {
	metrics.ReachedModelCallLimit = true
	metrics.DurationMillis = time.Since(started).Milliseconds()
	if err := emit("loop.model_call_limit_reached", map[string]any{"budget": request.ModelCallBudget, "used": metrics.ModelCalls, "stage": stage}); err != nil {
		return loopResult{Metrics: metrics}, err
	}
	return loopResult{Reply: "本次运行达到模型调用次数上限，任务尚未确认完成。", Metrics: metrics}, nil
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
