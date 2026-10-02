package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	UserID              string
	ProviderID          string
	ProviderModel       string
	Generation          domain.AgentGeneration
	Messages            []provider.ChatMessage
	Checkpoint          json.RawMessage
	RuntimeFingerprint  string
	Scope               loopScope
	Emit                func(string, any) error
	PersistCheckpoint   func(string, any, loopCheckpoint) error
	DetailedTrace       bool
	ModelCallBudget     int
	StepBudgetOverride  *int
	ModelBudgetOverride *int
	ShouldPause         func() bool
}

type loopResult struct {
	Reply         string
	Metrics       domain.RunMetrics
	Continuation  json.RawMessage
	Messages      []provider.ChatMessage
	StallReason   string
	HandoffPaused bool
}

type loopCheckpoint struct {
	Version                int                            `json:"version"`
	ProviderID             string                         `json:"providerId"`
	GenerationID           string                         `json:"generationId"`
	RuntimeFingerprint     string                         `json:"runtimeFingerprint"`
	ResumeAllowed          bool                           `json:"resumeAllowed"`
	Stage                  string                         `json:"stage"`
	Messages               []provider.ChatMessage         `json:"messages"`
	NextStep               int                            `json:"nextStep"`
	PendingStep            int                            `json:"pendingStep,omitempty"`
	PendingCalls           []provider.ToolCall            `json:"pendingCalls,omitempty"`
	ExpectedToolBindings   map[string]permissions.Request `json:"expectedToolBindings,omitempty"`
	PendingIndex           int                            `json:"pendingIndex,omitempty"`
	InFlightModelCall      bool                           `json:"inFlightModelCall,omitempty"`
	FinalReply             string                         `json:"finalReply,omitempty"`
	Metrics                domain.RunMetrics              `json:"metrics"`
	StepBudget             *int                           `json:"stepBudget,omitempty"`
	ModelCallBudget        *int                           `json:"modelCallBudget,omitempty"`
	RecentStepFingerprints []string                       `json:"recentStepFingerprints,omitempty"`
	StuckNudgeCount        int                            `json:"stuckNudgeCount,omitempty"`
	StuckNudgePending      bool                           `json:"stuckNudgePending,omitempty"`
	StuckNudgeReason       string                         `json:"stuckNudgeReason,omitempty"`
	StuckNudgeCyclePeriod  int                            `json:"stuckNudgeCyclePeriod,omitempty"`
}

var errContinuationCheckpointUnsafe = errors.New("continuation checkpoint is not at a replay-safe boundary")

func decodeSafeContinuationCheckpoint(raw []byte) (loopCheckpoint, error) {
	var checkpoint loopCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return loopCheckpoint{}, fmt.Errorf("decode continuation checkpoint: %w", err)
	}
	if checkpoint.Version != 1 || !checkpoint.ResumeAllowed || checkpoint.ProviderID == "" || checkpoint.GenerationID == "" || checkpoint.RuntimeFingerprint == "" || checkpoint.InFlightModelCall || len(checkpoint.PendingCalls) != 0 || checkpoint.PendingStep != 0 || checkpoint.PendingIndex != 0 || (checkpoint.Stage != "loop" && checkpoint.Stage != "plan") || len(checkpoint.Messages) == 0 {
		return loopCheckpoint{}, errContinuationCheckpointUnsafe
	}
	return checkpoint, nil
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
	stepBudget := spec.MaxSteps
	if request.StepBudgetOverride != nil {
		stepBudget = *request.StepBudgetOverride
	}
	checkpoint.StepBudget = &stepBudget
	modelCallBudget := request.ModelCallBudget
	if request.ModelBudgetOverride != nil {
		modelCallBudget = *request.ModelBudgetOverride
	}
	checkpoint.ModelCallBudget = &modelCallBudget
	request.ModelCallBudget = modelCallBudget
	request.StepBudgetOverride = checkpoint.StepBudget
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
		if checkpoint.StepBudget != nil {
			request.StepBudgetOverride = checkpoint.StepBudget
		}
		if checkpoint.ModelCallBudget != nil {
			request.ModelCallBudget = *checkpoint.ModelCallBudget
		}
	} else if request.PersistCheckpoint != nil {
		if err := persistLoopCheckpoint(request, emit, "turn.checkpointed", map[string]any{"version": checkpoint.Version, "generationId": checkpoint.GenerationID}, checkpoint); err != nil {
			return loopResult{Metrics: metrics}, err
		}
	}
	if checkpoint.Stage == "final" {
		metrics.DurationMillis = time.Since(started).Milliseconds()
		return loopResult{Reply: checkpoint.FinalReply, Metrics: metrics, Messages: append([]provider.ChatMessage(nil), checkpoint.Messages...)}, nil
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
		if cp, ok := request.Scope.(interface {
			prepareTurnMessages(context.Context, modelRuntime, loopRequest, []provider.ChatMessage, []provider.ToolDefinition, int, domain.RunMetrics) ([]provider.ChatMessage, turnCompaction, domain.RunMetrics, error)
		}); ok {
			prepared, compaction, updatedMetrics, err := cp.prepareTurnMessages(ctx, models, request, plannerMessages, nil, 0, metrics)
			metrics = updatedMetrics
			if err != nil {
				return loopResult{Metrics: metrics}, err
			}
			if compaction.Applied {
				plannerMessages = prepared
				messages = []provider.ChatMessage{messages[0]}
				messages = append(messages, prepared[1:]...)
				checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
				if err := persistLoopCheckpoint(request, emit, "context.compacted", map[string]any{"scope": "unified", "stage": "planner", "forced": compaction.Forced, "originalChars": compaction.OriginalChars, "compactedChars": compaction.CompactedChars, "modelCalls": metrics.ModelCalls}, checkpoint); err != nil {
					return loopResult{Metrics: metrics}, err
				}
			}
		}
		if modelCallBudgetReached(request, metrics) {
			return modelCallLimitResult(emit, request, metrics, started, "planner", checkpoint)
		}
		plannerStarted := time.Now()
		requestCtx := modelRequestContext(ctx, request, metrics)
		checkpoint.InFlightModelCall = true
		checkpoint.Metrics = metrics
		checkpoint.Metrics.ModelCalls += reservedModelAttempts(request, metrics)
		if err := persistLoopCheckpoint(request, emit, "planner.requested", map[string]any{"generationId": request.Generation.ID, "messageCount": len(plannerMessages)}, checkpoint); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		completion, _, err := completeWithContextPlan(requestCtx, models, request.Scope, request, "planner", plannerMessages, nil, emit)
		metrics.ModelCalls += modelAttemptCount(completion, err)
		checkpoint.InFlightModelCall = false
		addUsage(&metrics, completion.Usage)
		if err != nil {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			_ = emit("planner.failed", map[string]any{"attempts": modelAttemptCount(completion, err), "error": err.Error()})
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
		briefMessage := provider.ChatMessage{Role: "assistant", Content: "Execution brief from the planning stage (advisory; verify against observations):\n" + brief}
		briefMessage.SourceID = contextSourceID("planner_brief", []byte(briefMessage.Content))
		if err := persistLoopContextSource(request.Scope, briefMessage.SourceID, "planner_brief", []byte(briefMessage.Content)); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		messages = append(messages, briefMessage)
		checkpoint.Stage = "loop"
		checkpoint.Messages = append([]provider.ChatMessage(nil), messages...)
		checkpoint.Metrics = metrics
		if err := persistLoopCheckpoint(request, emit, "planner.completed", map[string]any{"attempts": modelAttemptCount(completion, nil), "contentBytes": len(brief), "model": completion.Model, "durationMillis": time.Since(plannerStarted).Milliseconds()}, checkpoint); err != nil {
			return loopResult{Metrics: metrics}, err
		}
	}

	maxSteps := spec.MaxSteps
	if request.StepBudgetOverride != nil {
		maxSteps = *request.StepBudgetOverride
	}
	if maxSteps < 0 {
		return loopResult{Metrics: metrics}, fmt.Errorf("agent step budget must be zero (unlimited) or positive")
	}
	lastFailureFingerprint := ""
	consecutiveIdenticalFailures := 0
	stalled := false
	stallReason := "same_tool_failure_repeated"
	stallPeriod := 1
runSteps:
	for step := checkpoint.NextStep; maxSteps == 0 || step < maxSteps; step++ {
		// Honor handoff only after prior work is durably checkpointed and no
		// model request or tool call is in flight.
		if request.ShouldPause != nil && request.ShouldPause() && checkpoint.ResumeAllowed && checkpoint.Stage == "loop" && !checkpoint.InFlightModelCall && len(checkpoint.PendingCalls) == 0 && checkpoint.PendingStep == 0 && checkpoint.PendingIndex == 0 {
			metrics.DurationMillis = time.Since(started).Milliseconds()
			checkpoint.NextStep = step
			checkpoint.Messages = append([]provider.ChatMessage(nil), messages...)
			checkpoint.Metrics = metrics
			if err := persistLoopCheckpoint(request, emit, "turn.handoff_paused", map[string]any{"step": step + 1, "resumeAllowed": checkpoint.ResumeAllowed}, checkpoint); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			continuation, err := json.Marshal(checkpoint)
			if err != nil {
				return loopResult{Metrics: metrics}, fmt.Errorf("encode safe handoff checkpoint: %w", err)
			}
			return loopResult{Reply: "任务已在安全边界暂停，可以继续在云端执行。", Metrics: metrics, Continuation: continuation, Messages: append([]provider.ChatMessage(nil), messages...), HandoffPaused: true}, nil
		}
		var calls []provider.ToolCall
		startCall := 0
		if checkpoint.Stage == "dispatch" {
			if checkpoint.PendingStep != step+1 || checkpoint.PendingIndex < 0 || checkpoint.PendingIndex > len(checkpoint.PendingCalls) {
				return loopResult{Metrics: metrics}, fmt.Errorf("agent continuation checkpoint has an invalid pending tool step")
			}
			calls = append([]provider.ToolCall(nil), checkpoint.PendingCalls...)
			startCall = checkpoint.PendingIndex
		} else if modelCallBudgetReached(request, metrics) {
			checkpoint.Stage, checkpoint.NextStep = "loop", step
			checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
			return modelCallLimitResult(emit, request, metrics, started, "agent", checkpoint)
		} else {
			definitions := request.Scope.definitions()
			sendMessages := messages
			if cp, ok := request.Scope.(interface {
				prepareTurnMessages(context.Context, modelRuntime, loopRequest, []provider.ChatMessage, []provider.ToolDefinition, int, domain.RunMetrics) ([]provider.ChatMessage, turnCompaction, domain.RunMetrics, error)
			}); ok {
				prepared, compaction, updatedMetrics, prepareErr := cp.prepareTurnMessages(ctx, models, request, messages, definitions, step, metrics)
				metrics = updatedMetrics
				if prepareErr != nil {
					metrics.DurationMillis = time.Since(started).Milliseconds()
					return loopResult{Metrics: metrics}, prepareErr
				}
				sendMessages = prepared
				if compaction.Applied {
					messages = prepared
					checkpoint.Messages = append([]provider.ChatMessage(nil), messages...)
					checkpoint.Stage = "loop"
					checkpoint.NextStep = step
					checkpoint.Metrics = metrics
					if err := persistLoopCheckpoint(request, emit, "context.compacted", map[string]any{"scope": "unified", "step": step + 1, "forced": compaction.Forced, "originalChars": compaction.OriginalChars, "compactedChars": compaction.CompactedChars, "modelCalls": metrics.ModelCalls, "promptTokens": metrics.PromptTokens}, checkpoint); err != nil {
						return loopResult{Metrics: metrics}, err
					}
				}
			}
			modelStarted := time.Now()
			requestCtx := modelRequestContext(ctx, request, metrics)
			checkpoint.Stage, checkpoint.NextStep = "loop", step
			checkpoint.Messages = append([]provider.ChatMessage(nil), messages...)
			checkpoint.InFlightModelCall = true
			checkpoint.StuckNudgePending = false
			checkpoint.Metrics = metrics
			checkpoint.Metrics.ModelCalls += reservedModelAttempts(request, metrics)
			requestedDetails := map[string]any{"step": step + 1, "generationId": request.Generation.ID, "definitionDigest": request.Generation.DefinitionDigest, "strategy": spec.Strategy, "messageCount": len(messages), "toolDefinitionCount": len(definitions)}
			if err := persistLoopCheckpoint(request, emit, "model.requested", requestedDetails, checkpoint); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			completion, _, err := completeWithContextPlan(requestCtx, models, request.Scope, request, "agent_loop", sendMessages, definitions, emit)
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
			if len(toolCalls) == 0 {
				if reply == "" {
					metrics.DurationMillis = time.Since(started).Milliseconds()
					return loopResult{Metrics: metrics}, fmt.Errorf("agent loop returned an empty reply")
				}
				checkpoint.Stage, checkpoint.FinalReply = "final", reply
				if len(completion.ProviderItems) > 0 {
					transcriptHash := sha256.Sum256([]byte(reply))
					assistantMessage := provider.ChatMessage{Role: "assistant", ProviderItems: providerItemsCopy(completion.ProviderItems), ProviderID: request.ProviderID, ProviderModel: request.ProviderModel, ProviderTranscriptHash: hex.EncodeToString(transcriptHash[:])}
					archived, marshalErr := json.Marshal(assistantMessage)
					if marshalErr != nil {
						return loopResult{Metrics: metrics}, marshalErr
					}
					assistantMessage.SourceID = contextSourceID("assistant_responses_output", archived)
					if err := persistLoopContextSource(request.Scope, assistantMessage.SourceID, "responses_output", archived); err != nil {
						return loopResult{Metrics: metrics}, err
					}
					messages = append(messages, assistantMessage)
				}
				checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
				if err := persistLoopCheckpoint(request, emit, "model.completed", map[string]any{"step": step + 1, "attempts": modelAttemptCount(completion, nil), "toolCallCount": 0, "contentBytes": len(completion.Content), "model": completion.Model, "usage": completion.Usage, "durationMillis": time.Since(modelStarted).Milliseconds()}, checkpoint); err != nil {
					return loopResult{Metrics: metrics}, err
				}
				metrics.DurationMillis = time.Since(started).Milliseconds()
				return loopResult{Reply: reply, Metrics: metrics, Messages: append([]provider.ChatMessage(nil), messages...)}, nil
			}
			assistantMessage := provider.ChatMessage{Role: "assistant", Content: completion.Content, ToolCalls: toolCalls, ProviderItems: providerItemsCopy(completion.ProviderItems), ProviderID: request.ProviderID, ProviderModel: request.ProviderModel}
			assistantSourceBytes, marshalErr := json.Marshal(assistantMessage)
			if marshalErr != nil {
				return loopResult{Metrics: metrics}, marshalErr
			}
			assistantMessage.SourceID = contextSourceID("assistant_tool_calls", assistantSourceBytes)
			if err := persistLoopContextSource(request.Scope, assistantMessage.SourceID, "assistant_tool_calls", assistantSourceBytes); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			messages = append(messages, assistantMessage)
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
						sourceID := "tool_result:" + call.ID
						if err := persistLoopContextSource(request.Scope, sourceID, "tool_result", result); err != nil {
							return loopResult{Metrics: metrics}, err
						}
						messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: string(result), SourceID: sourceID})
						checkpoint.PendingIndex, checkpoint.Messages, checkpoint.Metrics = callIndex+1, append([]provider.ChatMessage(nil), messages...), metrics
						if err := persistLoopCheckpoint(request, emit, "tool.authorization_denied", map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name}, checkpoint); err != nil {
							return loopResult{Metrics: metrics}, err
						}
						continue
					}
				} else if decision.Outcome != permissions.OutcomeAllow {
					result, _ := json.Marshal(map[string]any{"ok": false, "error": decision.Reason, "authorization": "denied"})
					sourceID := "tool_result:" + call.ID
					if err := persistLoopContextSource(request.Scope, sourceID, "tool_result", result); err != nil {
						return loopResult{Metrics: metrics}, err
					}
					messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: string(result), SourceID: sourceID})
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
			if toolResultFailure(result) {
				digest := sha256.Sum256(append(append([]byte(call.Function.Name+"\x00"), arguments...), append([]byte("\x00"), result...)...))
				fingerprint := hex.EncodeToString(digest[:])
				if fingerprint == lastFailureFingerprint {
					consecutiveIdenticalFailures++
				} else {
					lastFailureFingerprint = fingerprint
					consecutiveIdenticalFailures = 1
				}
				if consecutiveIdenticalFailures >= 3 {
					stalled = true
					stallReason = "same_tool_failure_repeated"
					stallPeriod = 1
				}
			} else {
				lastFailureFingerprint = ""
				consecutiveIdenticalFailures = 0
			}
			switch call.Function.Name {
			case "axiom_fragment_create", "axiom_fragment_invoke", "axiom_fragment_drop", "axiom_plugin_install", "axiom_plugin_rollback", "axiom_plugin_mark_unusable", "exec_script":
				// These actions mutate turn-local or live plugin routing state. A later
				// process cannot reconstruct the exact in-memory routing snapshot.
				checkpoint.ResumeAllowed = false
			}
			if containsRunfileArtifact(arguments) || containsRunfileArtifact(result) {
				checkpoint.ResumeAllowed = false
			}
			sourceID := "tool_result:" + call.ID
			if err := persistLoopContextSource(request.Scope, sourceID, "tool_result", result); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			completedDetails := map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "sourceRef": sourceID, "source": authRequest.Source, "pluginId": authRequest.PluginID, "releaseId": authRequest.ReleaseID, "resultBytes": len(result), "durationMillis": time.Since(toolStarted).Milliseconds(), "ok": toolResultOK(result)}
			if request.DetailedTrace {
				completedDetails["result"] = traceJSONPreview(result, 64*1024)
			}
			messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: string(result), SourceID: sourceID})
			checkpoint.PendingIndex, checkpoint.Messages, checkpoint.Metrics = callIndex+1, append([]provider.ChatMessage(nil), messages...), metrics
			if err := persistLoopCheckpoint(request, emit, "tool.completed", completedDetails, checkpoint); err != nil {
				return loopResult{Metrics: metrics}, err
			}
		}
		checkpoint.Stage, checkpoint.NextStep = "loop", step+1
		checkpoint.PendingStep, checkpoint.PendingCalls, checkpoint.PendingIndex = 0, nil, 0
		checkpoint.ExpectedToolBindings = nil
		if fingerprint, ok := completedToolStepFingerprint(calls, messages); ok {
			checkpoint.RecentStepFingerprints = append(checkpoint.RecentStepFingerprints, fingerprint)
			if len(checkpoint.RecentStepFingerprints) > 12 {
				checkpoint.RecentStepFingerprints = append([]string(nil), checkpoint.RecentStepFingerprints[len(checkpoint.RecentStepFingerprints)-12:]...)
			}
			if period := repeatedToolCyclePeriod(checkpoint.RecentStepFingerprints); period > 0 && !stalled {
				stalled = true
				stallReason = "repeated_tool_cycle"
				stallPeriod = period
			}
		}
		checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
		if err := persistLoopCheckpoint(request, emit, "tools.completed", map[string]any{"step": step + 1, "toolCallCount": len(calls)}, checkpoint); err != nil {
			return loopResult{Metrics: metrics}, err
		}
		if stalled && checkpoint.StuckNudgeCount == 0 {
			nudgeReason, nudgeCyclePeriod := stallReason, stallPeriod
			if injectStuckNudge(messages, stallReason, stallPeriod) {
				checkpoint.StuckNudgeCount++
				checkpoint.StuckNudgePending = true
				checkpoint.StuckNudgeReason = stallReason
				checkpoint.StuckNudgeCyclePeriod = stallPeriod
				lastFailureFingerprint = ""
				consecutiveIdenticalFailures = 0
				stallPeriod = 1
				stalled = false
				checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
				if err := persistLoopCheckpoint(request, emit, "loop.nudged", map[string]any{"step": step + 1, "reason": nudgeReason, "cyclePeriod": nudgeCyclePeriod, "attempt": checkpoint.StuckNudgeCount}, checkpoint); err != nil {
					return loopResult{Metrics: metrics}, err
				}
			}
		}
		if stalled {
			repetitions := consecutiveIdenticalFailures
			if stallReason == "repeated_tool_cycle" {
				repetitions = 3
			}
			if err := emit("loop.stalled", map[string]any{"step": step + 1, "reason": stallReason, "cyclePeriod": stallPeriod, "repetitions": repetitions}); err != nil {
				return loopResult{Metrics: metrics}, err
			}
			break runSteps
		}
	}
	metrics.ReachedStall = stalled
	metrics.ReachedStepLimit = maxSteps > 0 && !stalled
	metrics.DurationMillis = time.Since(started).Milliseconds()
	if metrics.ReachedStepLimit {
		if err := emit("loop.step_limit_reached", map[string]any{"maxSteps": maxSteps}); err != nil {
			return loopResult{Metrics: metrics}, err
		}
	}
	checkpoint.PendingStep, checkpoint.PendingCalls, checkpoint.PendingIndex = 0, nil, 0
	checkpoint.ExpectedToolBindings = nil
	checkpoint.InFlightModelCall = false
	checkpoint.Messages, checkpoint.Metrics = append([]provider.ChatMessage(nil), messages...), metrics
	continuation, err := json.Marshal(checkpoint)
	if err != nil {
		return loopResult{Metrics: metrics}, err
	}
	reply := "本段执行预算已用完，任务尚未完成。已保留当前进度；点击“继续”后会从最近一次完成的工具结果接着执行。"
	if stalled {
		if stallReason == "repeated_tool_cycle" {
			reply = "Agent 反复执行了相同的工具操作并得到相同结果，系统已暂停以避免空转。任务尚未完成；补充指令后可以从已保存进度继续。"
		} else {
			reply = "Agent 连续重复了相同的工具失败，系统已暂停以避免继续空转。任务尚未完成；检查失败原因并补充指令后，可以从已保存进度继续。"
		}
	}
	if !stalled {
		stallReason = ""
	}
	return loopResult{Reply: reply, Metrics: metrics, Continuation: continuation, Messages: append([]provider.ChatMessage(nil), messages...), StallReason: stallReason}, nil
}

func completedToolStepFingerprint(calls []provider.ToolCall, messages []provider.ChatMessage) (string, bool) {
	if len(calls) == 0 {
		return "", false
	}
	results := make(map[string]string, len(calls))
	for _, message := range messages {
		if message.Role == "tool" {
			results[message.ToolCallID] = canonicalJSON([]byte(message.Content))
		}
	}
	type fingerprintCall struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Result    string `json:"result"`
	}
	step := make([]fingerprintCall, 0, len(calls))
	for _, call := range calls {
		result, exists := results[call.ID]
		if !exists {
			return "", false
		}
		step = append(step, fingerprintCall{Name: call.Function.Name, Arguments: canonicalJSON([]byte(call.Function.Arguments)), Result: result})
	}
	encoded, err := json.Marshal(step)
	if err != nil {
		return "", false
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), true
}

func canonicalJSON(raw []byte) string {
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return strings.TrimSpace(string(raw))
}

// repeatedToolCyclePeriod requires three identical repeats of a tool-step
// pattern (period 1-4). Calls and observed results are part of each
// fingerprint, so repeated calls with changing results can continue.
func repeatedToolCyclePeriod(fingerprints []string) int {
	for period := 1; period <= 4; period++ {
		if len(fingerprints) < period*3 {
			continue
		}
		start := len(fingerprints) - period*3
		equal := true
		for index := 0; index < period; index++ {
			if fingerprints[start+index] != fingerprints[start+period+index] || fingerprints[start+index] != fingerprints[start+2*period+index] {
				equal = false
				break
			}
		}
		if equal {
			return period
		}
	}
	return 0
}

func injectStuckNudge(messages []provider.ChatMessage, reason string, cyclePeriod int) bool {
	for index := range messages {
		if messages[index].Role != "system" {
			continue
		}
		pattern := "工具调用及结果反复重复"
		if reason == "same_tool_failure_repeated" {
			pattern = "相同工具调用连续失败"
		}
		messages[index].Content += fmt.Sprintf("\n\n## 运行时纠偏提示（系统检测）\n检测到%s（循环长度 %d）。这是运行时纠偏，不改变用户目标。请先重新检查已有工具结果和已完成工作，不要再次发出相同调用序列；选择一个有明确理由、且副作用安全的不同做法。如果任务已完成，请直接答复；如果无法继续，请如实说明阻碍，不要声称未验证的工作已经完成。", pattern, cyclePeriod)
		return true
	}
	return false
}

func toolResultFailure(result []byte) bool {
	var value map[string]any
	if json.Unmarshal(result, &value) != nil {
		return false
	}
	if ok, exists := value["ok"].(bool); exists && !ok {
		return true
	}
	if message, exists := value["error"].(string); exists && strings.TrimSpace(message) != "" {
		return true
	}
	return false
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

func modelCallLimitResult(emit func(string, any) error, request loopRequest, metrics domain.RunMetrics, started time.Time, stage string, checkpoint loopCheckpoint) (loopResult, error) {
	metrics.ReachedModelCallLimit = true
	metrics.DurationMillis = time.Since(started).Milliseconds()
	if err := emit("loop.model_call_limit_reached", map[string]any{"budget": request.ModelCallBudget, "used": metrics.ModelCalls, "stage": stage}); err != nil {
		return loopResult{Metrics: metrics}, err
	}
	if stage != "planner" {
		checkpoint.Stage = "loop"
	}
	checkpoint.PendingStep, checkpoint.PendingCalls, checkpoint.PendingIndex = 0, nil, 0
	checkpoint.ExpectedToolBindings = nil
	checkpoint.InFlightModelCall = false
	checkpoint.Metrics = metrics
	continuation, err := json.Marshal(checkpoint)
	if err != nil {
		return loopResult{Metrics: metrics}, err
	}
	return loopResult{Reply: "本段模型调用预算已用完，任务尚未完成。已保留当前进度；点击“继续”后可以接着执行。", Metrics: metrics, Continuation: continuation, Messages: append([]provider.ChatMessage(nil), checkpoint.Messages...)}, nil
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
