package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/capability"
	"axiom.local/agent/internal/capsule"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/promotion"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/scriptruntime"
	"axiom.local/agent/internal/storage"
)

type Service struct {
	hostCtx    context.Context
	store      *storage.Store
	providers  *provider.Service
	forge      *pluginforge.Service
	evolution  *evolution.Service
	fragments  *capability.Registry
	capsules   *capsule.Repository
	promotions *promotion.Service
	runningMu  sync.Mutex
	running    map[string]context.CancelCauseFunc
	events     *eventBroker
}

func New(hostCtx context.Context, store *storage.Store, providers *provider.Service, forge *pluginforge.Service, evolutionService *evolution.Service, workspaceRoot, dataDir string) (*Service, error) {
	scripts := scriptruntime.New()
	capsules, err := capsule.Open(workspaceRoot, scripts)
	if err != nil {
		return nil, err
	}
	promotions, err := promotion.Open(hostCtx, dataDir, capsules, forge)
	if err != nil {
		return nil, err
	}
	return &Service{hostCtx: hostCtx, store: store, providers: providers, forge: forge, evolution: evolutionService, fragments: capability.NewRegistry(scripts), capsules: capsules, promotions: promotions, running: map[string]context.CancelCauseFunc{}, events: newEventBroker()}, nil
}

func (s *Service) Fragments(userID, conversationID string) []capability.Fragment {
	return s.fragments.List(userID, conversationID)
}

func (s *Service) Capsules() []capsule.Summary { return s.capsules.List() }

func (s *Service) VerifyCapsule(ctx context.Context, id string) capsule.VerificationReport {
	return s.capsules.Verify(ctx, id)
}

func (s *Service) Promotions(userID string) []promotion.Job {
	return s.promotions.List(userID)
}

func (s *Service) PromoteCapsule(userID, id string) (promotion.Job, error) {
	return s.promotions.Request(userID, id)
}

func (s *Service) RetryPromotion(userID, id string) (promotion.Job, error) {
	return s.promotions.Retry(userID, id)
}
func (s *Service) List(ctx context.Context, userID string) ([]domain.Conversation, error) {
	return s.store.ListConversations(ctx, userID)
}
func (s *Service) Get(ctx context.Context, userID, id string) (domain.ConversationDetail, error) {
	return s.store.Conversation(ctx, userID, id)
}
func (s *Service) Trace(ctx context.Context, userID, id string) ([]domain.TraceEvent, error) {
	return s.store.TraceEvents(ctx, userID, id)
}
func (s *Service) Turns(ctx context.Context, userID, conversationID string) ([]domain.AgentTurn, error) {
	return s.store.AgentTurns(ctx, userID, conversationID)
}
func (s *Service) TurnEvents(ctx context.Context, userID, turnID string, after int) ([]domain.TraceEvent, error) {
	return s.store.TurnEvents(ctx, userID, turnID, after)
}
func (s *Service) SubscribeTurn(turnID string) (<-chan struct{}, func()) {
	return s.events.subscribe(turnID)
}
func (s *Service) Cancel(ctx context.Context, userID, turnID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "user_requested"
	}
	if err := s.store.RequestAgentTurnCancel(ctx, userID, turnID, reason); err != nil {
		return err
	}
	s.events.notify(turnID)
	s.runningMu.Lock()
	cancel := s.running[turnID]
	s.runningMu.Unlock()
	if cancel != nil {
		cancel(errors.New(reason))
	}
	return nil
}
func (s *Service) Create(ctx context.Context, userID, title, providerID string) (domain.Conversation, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "New mission"
	}
	if providerID == "" {
		return domain.Conversation{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	c := domain.Conversation{ID: id("run"), UserID: userID, Title: title, ProviderID: providerID, CreatedAt: now, UpdatedAt: now}
	generation, err := s.evolution.Stable(ctx, userID)
	if err != nil {
		return domain.Conversation{}, err
	}
	if err := s.store.CreateConversationWithGeneration(ctx, c, generation); err != nil {
		return domain.Conversation{}, err
	}
	c.AgentGenerationID = generation.ID
	c.AgentDefinitionDigest = generation.DefinitionDigest
	return c, nil
}
func (s *Service) Turn(ctx context.Context, userID, conversationID, content string) (domain.Message, error) {
	return s.runTurn(ctx, userID, conversationID, content, nil)
}

type turnStart struct {
	receipt domain.TurnReceipt
	err     error
}

func (s *Service) Submit(_ context.Context, userID, conversationID, content string) (domain.TurnReceipt, error) {
	started := make(chan turnStart, 1)
	go func() {
		_, _ = s.runTurn(s.hostCtx, userID, conversationID, content, started)
	}()
	result := <-started
	return result.receipt, result.err
}

func (s *Service) runTurn(ctx context.Context, userID, conversationID, content string, started chan<- turnStart) (domain.Message, error) {
	signalStart := func(receipt domain.TurnReceipt, err error) {
		if started != nil {
			started <- turnStart{receipt: receipt, err: err}
			started = nil
		}
	}
	content = strings.TrimSpace(content)
	if content == "" {
		signalStart(domain.TurnReceipt{}, domain.ErrInvalid)
		return domain.Message{}, domain.ErrInvalid
	}
	detail, err := s.store.Conversation(ctx, userID, conversationID)
	if err != nil {
		signalStart(domain.TurnReceipt{}, err)
		return domain.Message{}, err
	}
	generation, err := s.evolution.ConversationGeneration(ctx, userID, conversationID)
	if err != nil {
		signalStart(domain.TurnReceipt{}, err)
		return domain.Message{}, err
	}
	now := time.Now().UTC()
	userMessage := domain.Message{ID: id("msg"), ConversationID: conversationID, Role: "user", Content: content, CreatedAt: now}
	turn := domain.AgentTurn{ID: id("turn"), ConversationID: conversationID, UserID: userID, InputMessageID: userMessage.ID, ProviderID: detail.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, Status: "running", StartedAt: now, UpdatedAt: now}
	runCtx, cancel := context.WithCancelCause(ctx)
	s.runningMu.Lock()
	s.running[turn.ID] = cancel
	s.runningMu.Unlock()
	defer func() {
		s.runningMu.Lock()
		delete(s.running, turn.ID)
		s.runningMu.Unlock()
		cancel(nil)
	}()
	scope := newTurnScope(s, userID, conversationID, turn.ID)
	defer scope.Close()
	startedDetails, _ := json.Marshal(map[string]any{"messageCount": len(detail.Messages) + 1, "pinnedTools": len(scope.tools), "pinnedSkills": len(scope.skills), "generationId": generation.ID, "definitionDigest": generation.DefinitionDigest, "strategy": generation.Definition.Spec.Strategy})
	if err := s.store.StartAgentTurn(ctx, userID, turn, userMessage, startedDetails); err != nil {
		signalStart(domain.TurnReceipt{}, err)
		return domain.Message{}, err
	}
	s.events.notify(turn.ID)
	signalStart(domain.TurnReceipt{TurnID: turn.ID, ConversationID: conversationID, InputMessageID: userMessage.ID, Status: "running"}, nil)
	detail.Messages = append(detail.Messages, userMessage)
	messages, omitted := buildContext(detail, generation)
	trace := newTraceRecorder(runCtx, s.store, userID, turn.ID, s.events.notify)
	if omitted > 0 {
		if err := trace.emit("context.compacted", map[string]any{"omittedMessages": omitted}); err != nil {
			return domain.Message{}, s.failTurn(ctx, userID, turn.ID, err, "journal_error")
		}
	}
	result, err := executeLoop(runCtx, s.providers, loopRequest{UserID: userID, ProviderID: detail.ProviderID, Generation: generation, Messages: messages, Scope: scope, Emit: trace.emit})
	if err != nil {
		status, reason := "failed", "runtime_error"
		if errors.Is(runCtx.Err(), context.Canceled) {
			status, reason = "cancelled", "cancelled"
		}
		return domain.Message{}, s.finishFailedTurn(ctx, userID, turn.ID, status, reason, err, result.Metrics)
	}
	assistant := domain.Message{ID: id("msg"), ConversationID: conversationID, Role: "assistant", Content: result.Reply, CreatedAt: time.Now().UTC()}
	details, _ := json.Marshal(map[string]any{"replyBytes": len(result.Reply), "metrics": result.Metrics, "generationId": generation.ID})
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	if err := s.store.FinishAgentTurn(finishCtx, userID, turn.ID, "completed", "assistant_response", &assistant, details); err != nil {
		return domain.Message{}, err
	}
	s.events.notify(turn.ID)
	return assistant, nil
}

func (s *Service) failTurn(ctx context.Context, userID, turnID string, cause error, reason string) error {
	return s.finishFailedTurn(ctx, userID, turnID, "failed", reason, cause, domain.RunMetrics{})
}

func (s *Service) finishFailedTurn(ctx context.Context, userID, turnID, status, reason string, cause error, metrics domain.RunMetrics) error {
	details, _ := json.Marshal(map[string]any{"error": cause.Error(), "metrics": metrics})
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if journalErr := s.store.FinishAgentTurn(finishCtx, userID, turnID, status, reason, nil, details); journalErr != nil {
		return errors.Join(cause, journalErr)
	}
	s.events.notify(turnID)
	return cause
}

// RunEvaluation executes one immutable generation without writing conversation
// messages. The evaluation scope excludes creator actions and any capability
// that is not declared workspace-readonly.
func (s *Service) RunEvaluation(ctx context.Context, userID, providerID string, generation domain.AgentGeneration, prompt string) (string, domain.RunMetrics, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", domain.RunMetrics{}, domain.ErrInvalid
	}
	scope := newEvaluationScope(s, userID)
	defer scope.Close()
	messages := []provider.ChatMessage{
		{Role: "system", Content: generation.Definition.Spec.SystemPrompt + "\n\nEvaluation mode: work only through the exposed read-only capabilities. Return the actual task result, not a description of this evaluation."},
		{Role: "user", Content: prompt},
	}
	result, err := executeLoop(ctx, s.providers, loopRequest{UserID: userID, ProviderID: providerID, Generation: generation, Messages: messages, Scope: scope})
	return result.Reply, result.Metrics, err
}

func tool(name, description, schema string) provider.ToolDefinition {
	definition := provider.ToolDefinition{Type: "function"}
	definition.Function.Name = name
	definition.Function.Description = description
	definition.Function.Parameters = json.RawMessage(schema)
	return definition
}

func (s *Service) runCreatorTool(ctx context.Context, userID, name string, arguments json.RawMessage) json.RawMessage {
	var input struct {
		ProjectID        string          `json:"projectId"`
		CapabilityID     string          `json:"capabilityId"`
		Input            json.RawMessage `json:"input"`
		Name             string          `json:"name"`
		Description      string          `json:"description"`
		Path             string          `json:"path"`
		Content          string          `json:"content"`
		Patch            string          `json:"patch"`
		ExpectedRevision string          `json:"expectedRevision"`
		ReleaseID        string          `json:"releaseId"`
		Shape            string          `json:"shape"`
	}
	if len(arguments) == 0 || json.Unmarshal(arguments, &input) != nil {
		return toolError("invalid tool arguments")
	}
	var value any
	var err error
	switch name {
	case "axiom_plugin_projects":
		value, err = s.forge.ListProjects(ctx, userID)
	case "axiom_plugin_propose":
		value, err = s.forge.Create(ctx, userID, pluginforge.CreateInput{Name: input.Name, Description: input.Description, Shape: input.Shape})
	case "axiom_plugin_generate":
		value, err = s.forge.Generate(ctx, userID, input.ProjectID)
	case "axiom_plugin_source_tree":
		value, err = s.forge.SourceTree(ctx, userID, input.ProjectID)
	case "axiom_plugin_read_source":
		value, err = s.forge.ReadSourceFile(ctx, userID, input.ProjectID, input.Path)
	case "axiom_plugin_source_diff":
		value, err = s.forge.SourceDiff(ctx, userID, input.ProjectID)
	case "axiom_plugin_write_source":
		value, err = s.forge.WriteSourceFile(ctx, userID, input.ProjectID, pluginforge.SourceFileInput{Path: input.Path, Content: input.Content, ExpectedRevision: input.ExpectedRevision})
	case "axiom_plugin_apply_patch":
		var project pluginforge.Project
		var diff pluginforge.SourceDiff
		project, diff, err = s.forge.ApplySourcePatch(ctx, userID, input.ProjectID, pluginforge.PatchInput{ExpectedRevision: input.ExpectedRevision, Patch: input.Patch})
		value = map[string]any{"project": project, "diff": diff}
	case "axiom_plugin_begin_revision":
		value, err = s.forge.BeginRevision(ctx, userID, input.ProjectID)
	case "axiom_plugin_build":
		var project pluginforge.Project
		var release pluginforge.Release
		project, release, err = s.forge.BuildAndTest(ctx, userID, input.ProjectID)
		value = map[string]any{"project": project, "release": release}
	case "axiom_plugin_request_approval":
		var project pluginforge.Project
		var release pluginforge.Release
		project, release, err = s.forge.RequestApproval(ctx, userID, input.ProjectID)
		value = map[string]any{"project": project, "release": release, "requiresUserApproval": true}
	case "axiom_plugin_install":
		var project pluginforge.Project
		var installation pluginforge.Installation
		project, installation, err = s.forge.Install(ctx, userID, input.ProjectID)
		value = map[string]any{"project": project, "installation": installation}
	case "axiom_plugin_rollback":
		var project pluginforge.Project
		var installation pluginforge.Installation
		project, installation, err = s.forge.Rollback(ctx, userID, input.ProjectID, input.ReleaseID)
		value = map[string]any{"project": project, "installation": installation}
	default:
		err = fmt.Errorf("unknown tool %q", name)
	}
	if err != nil {
		return toolError(err.Error())
	}
	raw, marshalErr := json.Marshal(map[string]any{"ok": true, "result": value})
	if marshalErr != nil {
		return toolError(marshalErr.Error())
	}
	return raw
}

func toolError(message string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"ok": false, "error": message})
	return raw
}
func toolResultOK(raw json.RawMessage) bool {
	var result struct {
		OK bool `json:"ok"`
	}
	return json.Unmarshal(raw, &result) == nil && result.OK
}
func id(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + "_" + hex.EncodeToString(raw)
}
