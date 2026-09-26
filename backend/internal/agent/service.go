package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/capability"
	"axiom.local/agent/internal/capsule"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/mcp"
	"axiom.local/agent/internal/permissions"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/plugins"
	"axiom.local/agent/internal/promotion"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/runfiles"
	"axiom.local/agent/internal/sandbox"
	"axiom.local/agent/internal/scriptruntime"
	"axiom.local/agent/internal/storage"
)

type Service struct {
	hostCtx       context.Context
	store         *storage.Store
	providers     *provider.Service
	forge         *pluginforge.Service
	evolution     *evolution.Service
	fragments     *capability.Registry
	capsules      *capsule.Repository
	promotions    *promotion.Service
	workspaceRoot string
	runfiles      *runfiles.Manager
	runLimits     RunLimits
	runSlots      chan struct{}
	plugins       *plugins.Manager
	runningMu     sync.Mutex
	running       map[string]context.CancelCauseFunc
	queueResumeMu sync.Mutex
	events        *eventBroker
	approvalMu    sync.Mutex
	approvals     map[string]chan bool
}

func turnRuntimeFingerprint(providerConfig domain.Provider, generation domain.AgentGeneration, profile domain.PermissionProfile, project domain.Project, projectEnabled bool, scope *turnScope) (string, error) {
	buildRevision, buildModified := "", ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				buildRevision = setting.Value
			case "vcs.modified":
				buildModified = setting.Value
			}
		}
	}
	type projectBinding struct {
		ID                  string
		Name                string
		Workdir             string
		Instructions        string
		InstructionsEnabled bool
		RemoteRepoURL       string
		RemoteBranch        string
	}
	defs := scope.definitions()
	var mcpConfigs []mcp.ServerConfig
	if scope.owner != nil && scope.owner.plugins != nil {
		mcpConfigs = scope.owner.plugins.MCPManager().ListConfigs()
		sort.Slice(mcpConfigs, func(i, j int) bool { return mcpConfigs[i].ID < mcpConfigs[j].ID })
	}
	var pinnedReleases []string
	if scope.lease != nil {
		for _, binding := range scope.lease.Capabilities() {
			pinnedReleases = append(pinnedReleases, "tool:"+binding.PluginID+":"+binding.ToolExport.ID+":"+binding.ReleaseID)
		}
		for _, binding := range scope.lease.Skills() {
			pinnedReleases = append(pinnedReleases, "skill:"+binding.PluginID+":"+binding.Skill.ID+":"+binding.ReleaseID)
		}
	}
	for _, manifest := range scope.loadedCapsules {
		pinnedReleases = append(pinnedReleases, "capsule:"+manifest.ID+":"+manifest.Digest)
	}
	sort.Strings(pinnedReleases)
	value := struct {
		Version           int
		BuildRevision     string
		BuildModified     string
		ProviderID        string
		ProviderKind      string
		ProviderBaseURL   string
		ProviderModel     string
		ContextWindow     int
		GenerationID      string
		GenerationDigest  string
		PermissionProfile domain.PermissionProfile
		ProjectEnabled    bool
		Project           projectBinding
		ToolDefinitions   []provider.ToolDefinition
		MCPServers        []mcp.ServerConfig
		PinnedReleases    []string
	}{
		Version: 1, BuildRevision: buildRevision, BuildModified: buildModified,
		ProviderID: providerConfig.ID, ProviderKind: providerConfig.Kind, ProviderBaseURL: providerConfig.BaseURL,
		ProviderModel: providerConfig.Model, ContextWindow: providerConfig.ContextWindow,
		GenerationID: generation.ID, GenerationDigest: generation.DefinitionDigest,
		PermissionProfile: profile, ProjectEnabled: projectEnabled,
		Project:         projectBinding{ID: project.ID, Name: project.Name, Workdir: project.Workdir, Instructions: project.Instructions, InstructionsEnabled: project.InstructionsEnabled, RemoteRepoURL: project.RemoteRepoURL, RemoteBranch: project.RemoteBranch},
		ToolDefinitions: defs, MCPServers: mcpConfigs, PinnedReleases: pinnedReleases,
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func New(hostCtx context.Context, store *storage.Store, providers *provider.Service, forge *pluginforge.Service, evolutionService *evolution.Service, workspaceRoot, dataDir, agentTempDir string, configuredLimits ...RunLimits) (*Service, error) {
	limits := DefaultRunLimits()
	if len(configuredLimits) > 0 {
		limits = configuredLimits[0].normalized()
	}
	runfileManager, err := runfiles.NewManager(agentTempDir, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if err := sandbox.RecoverRunfiles(runfileManager.Root()); err != nil {
		return nil, fmt.Errorf("recover interrupted Windows sandbox permissions: %w", err)
	}
	scripts := scriptruntime.New()
	capsules, err := capsule.Open(workspaceRoot, scripts)
	if err != nil {
		return nil, err
	}
	promotions, err := promotion.Open(hostCtx, dataDir, capsules, forge)
	if err != nil {
		return nil, err
	}
	return &Service{hostCtx: hostCtx, store: store, providers: providers, forge: forge, evolution: evolutionService, fragments: capability.NewRegistry(scripts), capsules: capsules, promotions: promotions, running: map[string]context.CancelCauseFunc{}, events: newEventBroker(), approvals: map[string]chan bool{}, workspaceRoot: workspaceRoot, runfiles: runfileManager, runLimits: limits, runSlots: make(chan struct{}, limits.MaxConcurrentRuns), plugins: plugins.NewManager(workspaceRoot)}, nil
}

func (s *Service) Plugins() *plugins.Manager { return s.plugins }

func (s *Service) WorkspaceRoot() string { return s.workspaceRoot }

func (s *Service) SetPlugins(pm *plugins.Manager) { s.plugins = pm }

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

func (s *Service) PendingApprovals(ctx context.Context, userID, conversationID string) ([]domain.ApprovalRequest, error) {
	return s.store.PendingApprovals(ctx, userID, conversationID)
}

func (s *Service) ToolUsageMetrics(ctx context.Context, userID string, days int) ([]domain.ToolUsageMetric, error) {
	if days < 1 || days > 365 {
		return nil, domain.ErrInvalid
	}
	return s.store.ToolUsageMetrics(ctx, userID, time.Now().UTC().Add(-time.Duration(days)*24*time.Hour))
}

func (s *Service) ResolveApproval(ctx context.Context, userID, approvalID, choice string) (domain.ApprovalRequest, error) {
	s.approvalMu.Lock()
	waiter := s.approvals[approvalID]
	s.approvalMu.Unlock()
	if waiter == nil {
		return domain.ApprovalRequest{}, fmt.Errorf("%w: approval is not attached to a running turn", domain.ErrConflict)
	}
	request, err := s.store.ResolveApproval(ctx, userID, approvalID, choice)
	if err != nil {
		return domain.ApprovalRequest{}, err
	}
	waiter <- (request.Status == "approved")
	s.events.notify(request.TurnID)
	return request, nil
}

func (s *Service) requestToolApproval(ctx context.Context, userID, conversationID, turnID, callID string, policyRequest permissions.Request, decision permissions.Decision, arguments json.RawMessage) (bool, error) {
	approvalID := id("approval")
	argsHash := sha256.Sum256(arguments)
	argsHashHex := hex.EncodeToString(argsHash[:])
	priorStatus, priorDecision, found, err := s.store.ResolvedToolApproval(ctx, userID, turnID, callID, argsHashHex)
	if err != nil {
		return false, fmt.Errorf("read prior tool approval decision: %w", err)
	}
	if found {
		switch priorStatus {
		case "approved":
			if priorDecision == "approve" {
				return true, nil
			}
			return false, fmt.Errorf("persisted approval has inconsistent decision %q", priorDecision)
		case "denied", "expired":
			return false, nil
		case "pending":
			return false, fmt.Errorf("matching tool approval is already pending: %w", domain.ErrConflict)
		// A pending request is cancelled during host recovery and must be asked
		// again. A cancelled turn never reaches this path automatically.
		case "cancelled":
		default:
			return false, fmt.Errorf("unknown persisted approval state %q", priorStatus)
		}
	}
	previewValue := any("[参数无法解析；为保护隐私已隐藏]")
	var validArguments any
	if json.Unmarshal(arguments, &validArguments) == nil {
		previewValue = traceJSONPreview(arguments, 8*1024)
	}
	preview, marshalErr := json.Marshal(previewValue)
	if marshalErr != nil {
		return false, fmt.Errorf("serialize approval preview: %w", marshalErr)
	}
	impact, err := s.approvalImpact(ctx, userID, policyRequest.ToolName, arguments)
	if err != nil {
		return false, err
	}
	var approvedRelease struct {
		PluginID  string `json:"pluginId"`
		ReleaseID string `json:"releaseId"`
	}
	if json.Unmarshal([]byte(impact), &approvedRelease) == nil {
		if policyRequest.PluginID == "" {
			policyRequest.PluginID = approvedRelease.PluginID
		}
		if policyRequest.ReleaseID == "" {
			policyRequest.ReleaseID = approvedRelease.ReleaseID
		}
	}
	now := time.Now().UTC()
	request := domain.ApprovalRequest{ID: approvalID, ConversationID: conversationID, TurnID: turnID, ToolCallID: callID, ToolName: policyRequest.ToolName, Source: policyRequest.Source, PluginID: policyRequest.PluginID, ReleaseID: policyRequest.ReleaseID, Effect: string(policyRequest.Effect), PermissionProfile: policyRequest.Profile, Resource: policyRequest.Resource, Impact: impact, Reason: decision.Reason, Arguments: string(preview), Status: "pending", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
	waiter := make(chan bool, 1)
	s.approvalMu.Lock()
	s.approvals[approvalID] = waiter
	s.approvalMu.Unlock()
	defer func() { s.approvalMu.Lock(); delete(s.approvals, approvalID); s.approvalMu.Unlock() }()
	if err := s.store.CreateApprovalAndPause(ctx, userID, request, argsHashHex); err != nil {
		return false, err
	}
	s.events.notify(turnID)
	timer := time.NewTimer(time.Until(request.ExpiresAt))
	defer timer.Stop()
	select {
	case approved := <-waiter:
		return approved, nil
	case <-ctx.Done():
		return false, context.Cause(ctx)
	case <-timer.C:
		request, err := s.store.ResolveApproval(context.WithoutCancel(ctx), userID, approvalID, "deny")
		if err != nil {
			return false, fmt.Errorf("expire tool approval: %w", err)
		}
		s.events.notify(request.TurnID)
		return false, nil
	}
}

func (s *Service) approvalImpact(ctx context.Context, userID, toolName string, arguments json.RawMessage) (string, error) {
	if toolName == "axiom_plugin_build" {
		var input struct {
			ProjectID string `json:"projectId"`
		}
		if err := json.Unmarshal(arguments, &input); err != nil || strings.TrimSpace(input.ProjectID) == "" {
			return "", fmt.Errorf("plugin build approval requires a project ID: %w", domain.ErrInvalid)
		}
		projects, err := s.forge.ListProjects(ctx, userID)
		if err != nil {
			return "", fmt.Errorf("load plugin build approval details: %w", err)
		}
		for _, project := range projects {
			if project.ID == input.ProjectID {
				details := map[string]any{"action": toolName, "project": project.Name, "projectId": project.ID, "currentState": project.State, "executesAgentAuthoredCode": true, "commands": []string{"go test ./...", "go build ."}, "networkMayBeUsedForDependencyResolution": true}
				raw, err := json.Marshal(details)
				if err != nil {
					return "", fmt.Errorf("encode plugin build approval details: %w", err)
				}
				return string(raw), nil
			}
		}
		return "", domain.ErrNotFound
	}
	if toolName == "axiom_compact_context" {
		return "此操作会压缩当前 turn 的较早工具输出；被省略细节可能无法恢复，仅影响本轮后续模型上下文，不删除持久化 trace。", nil
	}
	if toolName != "axiom_plugin_install" && toolName != "axiom_plugin_rollback" && toolName != "axiom_plugin_mark_unusable" {
		return "此操作仅授权当前工具调用。", nil
	}
	var input struct {
		ProjectID string `json:"projectId"`
		ReleaseID string `json:"releaseId"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "插件操作参数无效。", nil
	}
	if strings.TrimSpace(input.ProjectID) == "" || strings.TrimSpace(input.ReleaseID) == "" {
		return "", fmt.Errorf("plugin approval requires an exact project and release ID: %w", domain.ErrInvalid)
	}
	projects, err := s.forge.ListProjects(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("load plugin release approval details: %w", err)
	}
	for _, project := range projects {
		if project.ID != input.ProjectID {
			continue
		}
		var release *pluginforge.Release
		for i := range project.Releases {
			if project.Releases[i].ID == input.ReleaseID {
				release = &project.Releases[i]
				break
			}
		}
		if release == nil {
			return "", fmt.Errorf("the requested release is not available for approval: %w", domain.ErrNotFound)
		}
		if release.Availability != pluginforge.ReleaseAvailabilityAvailable {
			return "", fmt.Errorf("the requested release is marked %s and cannot be approved", release.Availability)
		}
		details := map[string]any{"action": toolName, "plugin": project.Name, "pluginId": release.PluginID, "releaseId": release.ID, "version": release.Version, "digest": release.Digest, "permissions": release.Manifest.Permissions, "capabilities": release.Manifest.Exports}
		if toolName == "axiom_plugin_mark_unusable" {
			details["reason"] = input.Reason
			details["effect"] = "下一次启动时移除此 release 的 bundle；版本元数据和审计保留，之后不能回退到该版本。"
		}
		raw, err := json.Marshal(details)
		if err != nil {
			return "", fmt.Errorf("encode plugin approval details: %w", err)
		}
		return string(raw), nil
	}
	return "目标插件不存在；批准后执行器仍会校验插件归属和状态。", nil
}

func (s *Service) Cancel(ctx context.Context, userID, turnID, reason string) (domain.AgentTurn, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "user_requested"
	}
	if err := s.store.RequestAgentTurnCancel(ctx, userID, turnID, reason); err != nil {
		return domain.AgentTurn{}, err
	}
	s.events.notify(turnID)
	s.runningMu.Lock()
	cancel := s.running[turnID]
	s.runningMu.Unlock()
	if cancel != nil {
		cancel(errors.New(reason))
	}
	return s.store.AgentTurn(ctx, userID, turnID)
}

func (s *Service) CancelConversation(ctx context.Context, userID, conversationID, reason string) (domain.ConversationCancelReceipt, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "user_requested"
	}
	receipt, err := s.store.CancelConversationAgentWork(ctx, userID, conversationID, reason)
	if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	if receipt.CancelledTurnID != "" {
		s.runningMu.Lock()
		cancel := s.running[receipt.CancelledTurnID]
		s.runningMu.Unlock()
		if cancel != nil {
			cancel(errors.New(reason))
		}
		s.events.notify(receipt.CancelledTurnID)
		turn, err := s.store.AgentTurn(ctx, userID, receipt.CancelledTurnID)
		if err != nil {
			return domain.ConversationCancelReceipt{}, err
		}
		receipt.CancelledTurnStatus = turn.Status
	}
	queued, err := s.store.QueuedAgentInputs(ctx, userID, conversationID)
	if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	if len(queued) != 0 {
		return domain.ConversationCancelReceipt{}, fmt.Errorf("%w: queued inputs remain after cancellation", domain.ErrConflict)
	}
	paused, err := s.store.ConversationExecutionPaused(ctx, userID, conversationID)
	if err != nil {
		return domain.ConversationCancelReceipt{}, err
	}
	if !paused {
		return domain.ConversationCancelReceipt{}, fmt.Errorf("%w: conversation did not enter paused state", domain.ErrConflict)
	}
	receipt.ExecutionPaused = paused
	receipt.QueuedInboxRemaining = len(queued)
	return receipt, nil
}

func (s *Service) Retry(ctx context.Context, userID, turnID string, content *string) (domain.TurnReceipt, error) {
	previousTurn, input, err := s.store.RetryAgentTurnSeed(ctx, userID, turnID)
	if err != nil {
		return domain.TurnReceipt{}, err
	}
	messageContent := input.Content
	if content != nil {
		messageContent = strings.TrimSpace(*content)
		if messageContent == "" {
			return domain.TurnReceipt{}, domain.ErrInvalid
		}
	}
	return s.submitRun(userID, previousTurn.ConversationID, messageContent, input.ID, turnID, "", content, "")
}

func (s *Service) Reconcile(ctx context.Context, userID, turnID, note string) (domain.AgentTurnReconciliation, error) {
	reconciliation, err := s.store.RecordAgentTurnReconciliation(ctx, userID, turnID, note, time.Now().UTC())
	if err != nil {
		return domain.AgentTurnReconciliation{}, err
	}
	s.events.notify(turnID)
	s.startNextInbox(userID, reconciliation.ConversationID)
	return reconciliation, nil
}

func (s *Service) BranchRetry(ctx context.Context, userID, turnID string, content *string) (domain.TurnReceipt, error) {
	previousTurn, input, err := s.store.AgentTurnSeedForBranch(ctx, userID, turnID)
	if err != nil {
		return domain.TurnReceipt{}, err
	}
	messageContent := input.Content
	if content != nil {
		messageContent = strings.TrimSpace(*content)
		if messageContent == "" {
			return domain.TurnReceipt{}, domain.ErrInvalid
		}
	}
	return s.submitRun(userID, previousTurn.ConversationID, messageContent, input.ID, turnID, "", nil, id("run"))
}

func (s *Service) QueueInput(ctx context.Context, userID, conversationID, content string) (domain.InboxInput, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return domain.InboxInput{}, domain.ErrInvalid
	}
	item := domain.InboxInput{ID: id("inbox"), ConversationID: conversationID, Content: content, Status: "queued", CreatedAt: time.Now().UTC()}
	if err := s.store.QueueAgentInput(ctx, userID, item); err != nil {
		return domain.InboxInput{}, err
	}
	s.ResumeQueuedInputs(userID)
	return s.store.AgentInboxItem(ctx, userID, item.ID)
}

func (s *Service) Inbox(ctx context.Context, userID, conversationID string) ([]domain.InboxInput, error) {
	return s.store.QueuedAgentInputs(ctx, userID, conversationID)
}

func (s *Service) ResumeQueuedInputs(userID string) {
	s.queueResumeMu.Lock()
	defer s.queueResumeMu.Unlock()
	conversationIDs, err := s.store.QueuedAgentConversationIDs(s.hostCtx, userID)
	if err != nil {
		slog.Error("failed to restore queued conversation inputs", "error", err)
		return
	}
	for _, conversationID := range conversationIDs {
		s.startNextInbox(userID, conversationID)
	}
}

// ResumeInterruptedTurns starts only turns that startup recovery has validated
// as having a durable checkpoint and no incomplete tool operation.
func (s *Service) ResumeInterruptedTurns(userID string) {
	turnIDs, err := s.store.ResumableAgentTurnIDs(s.hostCtx, userID)
	if err != nil {
		slog.Error("failed to read checkpoint-resumable agent turns", "error", err)
		return
	}
	for _, turnID := range turnIDs {
		if err := s.store.ResumeAgentTurn(s.hostCtx, userID, turnID, time.Now().UTC()); err != nil {
			slog.Error("failed to claim checkpoint-resumable agent turn", "turn_id", turnID, "error", err)
			continue
		}
		s.events.notify(turnID)
		go s.resumeActiveTurn(userID, turnID)
	}
}

func (s *Service) resumeActiveTurn(userID, turnID string) {
	select {
	case s.runSlots <- struct{}{}:
	case <-s.hostCtx.Done():
		return
	}
	defer func() {
		<-s.runSlots
		go s.ResumeQueuedInputs(userID)
	}()

	turn, err := s.store.AgentTurn(s.hostCtx, userID, turnID)
	if err != nil {
		slog.Error("failed to load resumed agent turn", "turn_id", turnID, "error", err)
		return
	}
	checkpointCipher, checkpointNonce, version, resumeAllowed, err := s.store.AgentTurnCheckpoint(s.hostCtx, userID, turnID)
	if err != nil {
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "checkpoint_unavailable", err, domain.RunMetrics{})
		return
	}
	checkpointPlain, err := s.providers.OpenRunCheckpoint(checkpointCipher, checkpointNonce)
	if err != nil {
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "checkpoint_decryption_failed", err, domain.RunMetrics{})
		return
	}
	checkpointRaw := json.RawMessage(checkpointPlain)
	var checkpoint loopCheckpoint
	if version != 1 || !resumeAllowed || json.Unmarshal(checkpointRaw, &checkpoint) != nil || !checkpoint.ResumeAllowed {
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "checkpoint_incompatible", fmt.Errorf("checkpoint cannot be resumed by this runtime"), domain.RunMetrics{})
		return
	}
	detail, err := s.store.Conversation(s.hostCtx, userID, turn.ConversationID)
	if err != nil {
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "conversation_unavailable", err, checkpoint.Metrics)
		return
	}
	if detail.ProviderID != turn.ProviderID || detail.PermissionProfile != turn.PermissionProfile {
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "turn_binding_changed", domain.ErrConflict, checkpoint.Metrics)
		return
	}
	generation, err := s.evolution.ConversationGeneration(s.hostCtx, userID, turn.ConversationID)
	if err != nil || generation.ID != turn.AgentGenerationID || generation.DefinitionDigest != turn.AgentDefinitionDigest {
		if err == nil {
			err = fmt.Errorf("conversation generation binding changed: %w", domain.ErrConflict)
		}
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "generation_binding_changed", err, checkpoint.Metrics)
		return
	}
	if !turn.PermissionProfile.Valid() {
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "permission_profile_invalid", domain.ErrInvalid, checkpoint.Metrics)
		return
	}
	scope, err := newTurnScope(s, userID, turn.ConversationID, turn.ID)
	if err != nil {
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "scope_unavailable", err, checkpoint.Metrics)
		return
	}
	scope.permissionProfile = turn.PermissionProfile
	projectBinding := domain.Project{}
	projectWorkspaceEnabled := s.plugins == nil || s.plugins.IsProjectWorkspaceEnabled()
	if projectWorkspaceEnabled && detail.ProjectID != "" {
		projectBinding, err = s.store.Project(s.hostCtx, userID, detail.ProjectID)
		if err != nil {
			cleanupErr := scope.Close()
			_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "project_context_unavailable", errors.Join(err, cleanupErr), checkpoint.Metrics)
			return
		}
		if projectBinding.Workdir != "" {
			scope.setWorkspaceRoot(projectBinding.Workdir)
		}
	}
	providerConfig, _, _, err := s.store.ProviderSecret(s.hostCtx, userID, turn.ProviderID)
	if err != nil {
		cleanupErr := scope.Close()
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "provider_configuration_unavailable", errors.Join(err, cleanupErr), checkpoint.Metrics)
		return
	}
	contextTokens := providerConfig.ContextWindow
	if contextTokens <= 0 {
		contextTokens = EstimateModelContextTokens(providerConfig.Model)
	}
	scope.setContextWindow(contextTokens)
	if err := scope.restoreCheckpointState(checkpoint.Messages); err != nil {
		cleanupErr := scope.Close()
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "turn_local_state_changed", errors.Join(err, cleanupErr), checkpoint.Metrics)
		return
	}
	runtimeFingerprint, err := turnRuntimeFingerprint(providerConfig, generation, turn.PermissionProfile, projectBinding, projectWorkspaceEnabled, scope)
	if err != nil || runtimeFingerprint != checkpoint.RuntimeFingerprint {
		if err == nil {
			err = fmt.Errorf("execution bindings changed since the checkpoint: %w", domain.ErrConflict)
		}
		cleanupErr := scope.Close()
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "runtime_binding_changed", errors.Join(err, cleanupErr), checkpoint.Metrics)
		return
	}
	remaining := s.runLimits.MaxRunDuration - time.Since(turn.StartedAt)
	budgetCtx, budgetCancel := context.WithTimeout(s.hostCtx, remaining)
	runCtx, runCancel := context.WithCancelCause(budgetCtx)
	s.runningMu.Lock()
	s.running[turnID] = runCancel
	s.runningMu.Unlock()
	defer func() {
		s.runningMu.Lock()
		delete(s.running, turnID)
		s.runningMu.Unlock()
		runCancel(nil)
		budgetCancel()
	}()
	trace := newTraceRecorder(runCtx, s.store, userID, turnID, s.events.notify, s.providers.SealRunCheckpoint)
	checkpointWriter := func(kind string, details any, state loopCheckpoint) error {
		state.RuntimeFingerprint, err = turnRuntimeFingerprint(providerConfig, generation, turn.PermissionProfile, projectBinding, projectWorkspaceEnabled, scope)
		if err != nil {
			return err
		}
		return trace.checkpoint(kind, details, state)
	}
	detailedTrace := s.plugins == nil || s.plugins.IsRunInspectorEnabled()
	result, runErr := executeLoop(runCtx, s.providers, loopRequest{UserID: userID, ProviderID: turn.ProviderID, Generation: generation, Checkpoint: checkpointRaw, RuntimeFingerprint: runtimeFingerprint, Scope: scope, Emit: trace.emit, PersistCheckpoint: checkpointWriter, DetailedTrace: detailedTrace, TokenBudget: s.runLimits.MaxTokensPerRun, ModelCallBudget: s.runLimits.MaxModelCalls})
	result.Metrics.DurationMillis = time.Since(turn.StartedAt).Milliseconds()
	if runErr != nil {
		status, reason := "failed", "runtime_error"
		if errors.Is(runCtx.Err(), context.Canceled) {
			status, reason = "cancelled", "cancelled"
		} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			status, reason = "incomplete", "time_limit"
		}
		if cleanupErr := scope.Close(); cleanupErr != nil {
			status, reason = "failed", "artifact_cleanup_failed"
			runErr = errors.Join(runErr, cleanupErr)
		}
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, status, reason, runErr, result.Metrics)
		return
	}
	assistant := domain.Message{ID: id("msg"), ConversationID: turn.ConversationID, Role: "assistant", Content: result.Reply, CreatedAt: time.Now().UTC()}
	detailsJSON, err := json.Marshal(map[string]any{"replyBytes": len(result.Reply), "metrics": result.Metrics, "generationId": generation.ID, "resumedFromCheckpoint": true})
	if err != nil {
		cleanupErr := scope.Close()
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "finish_details_failed", errors.Join(err, cleanupErr), result.Metrics)
		return
	}
	status, stopReason := "completed", "assistant_response"
	if result.Metrics.ReachedModelCallLimit {
		status, stopReason = "incomplete", "model_call_limit"
	} else if result.Metrics.ReachedTokenLimit {
		status, stopReason = "incomplete", "token_limit"
	} else if result.Metrics.ReachedStepLimit {
		status, stopReason = "incomplete", "step_limit"
	}
	if cleanupErr := scope.Close(); cleanupErr != nil {
		_ = s.finishFailedTurn(s.hostCtx, userID, turnID, turn.ConversationID, "failed", "artifact_cleanup_failed", cleanupErr, result.Metrics)
		return
	}
	if err := s.store.FinishAgentTurn(s.hostCtx, userID, turnID, status, stopReason, &assistant, detailsJSON); err != nil {
		slog.Error("failed to persist resumed agent turn", "conversation_id", turn.ConversationID, "turn_id", turnID, "stop_reason", stopReason, "error", err)
		return
	}
	s.events.notify(turnID)
}

func (s *Service) RunLimits() RunLimits { return s.runLimits }

func (s *Service) RuntimeHealth(ctx context.Context, userID string) (RuntimeHealth, error) {
	queued, err := s.store.CountQueuedAgentInputs(ctx, userID)
	if err != nil {
		return RuntimeHealth{}, err
	}
	outstandingEvaluations, err := s.store.CountOutstandingEvalExperiments(ctx, userID)
	if err != nil {
		return RuntimeHealth{}, err
	}
	return RuntimeHealth{
		ActiveRuns:             len(s.runSlots),
		QueuedInputs:           queued,
		OutstandingEvaluations: outstandingEvaluations,
		MaxConcurrent:          s.runLimits.MaxConcurrentRuns,
		MaxTokensPerRun:        s.runLimits.MaxTokensPerRun,
		MaxModelCalls:          s.runLimits.MaxModelCalls,
		MaxRunDuration:         s.runLimits.MaxRunDuration,
	}, nil
}

func (s *Service) startNextInbox(userID, conversationID string) {
	paused, err := s.store.ConversationExecutionPaused(s.hostCtx, userID, conversationID)
	if err != nil {
		slog.Error("failed to load conversation execution state", "conversation_id", conversationID, "error", err)
		return
	}
	if paused {
		return
	}
	active, err := s.store.HasActiveAgentTurn(s.hostCtx, userID, conversationID)
	if err != nil {
		slog.Error("failed to check active conversation turn", "conversation_id", conversationID, "error", err)
		return
	}
	if active {
		return
	}
	items, err := s.store.QueuedAgentInputs(s.hostCtx, userID, conversationID)
	if err != nil {
		slog.Error("failed to load queued conversation inputs", "conversation_id", conversationID, "error", err)
		return
	}
	if len(items) == 0 {
		return
	}
	item := items[0]
	started := make(chan turnStart, 1)
	finished := make(chan error, 1)
	go func() {
		_, runErr := s.runTurnWithOptions(s.hostCtx, userID, conversationID, item.Content, "", "", item.ID, nil, "", started)
		finished <- runErr
	}()
	result := <-started
	if result.err != nil {
		if !errors.Is(result.err, domain.ErrConflict) && !errors.Is(result.err, domain.ErrBusy) {
			slog.Error("queued conversation input was not started", "conversation_id", conversationID, "inbox_id", item.ID, "error", result.err)
		}
		return
	}
	go func() {
		if runErr := <-finished; runErr != nil {
			slog.Error("queued conversation turn terminated unsuccessfully", "conversation_id", conversationID, "inbox_id", item.ID, "turn_id", result.receipt.TurnID)
		}
	}()
}
func (s *Service) Create(ctx context.Context, userID, title, providerID string) (domain.Conversation, error) {
	return s.CreateWithProject(ctx, userID, title, providerID, "")
}

func (s *Service) CreateWithProject(ctx context.Context, userID, title, providerID, projectID string) (domain.Conversation, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "新对话"
	}
	if providerID == "" {
		return domain.Conversation{}, domain.ErrInvalid
	}
	if projectID != "" {
		if _, err := s.store.Project(ctx, userID, projectID); err != nil {
			return domain.Conversation{}, err
		}
	}
	now := time.Now().UTC()
	c := domain.Conversation{ID: id("run"), UserID: userID, Title: title, ProviderID: providerID, ProjectID: projectID, PermissionProfile: domain.DefaultPermissionProfile(), CreatedAt: now, UpdatedAt: now}
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

func (s *Service) UpdateProject(ctx context.Context, userID, id, projectID string) (domain.ConversationDetail, error) {
	if err := s.store.UpdateConversationProject(ctx, userID, id, projectID); err != nil {
		return domain.ConversationDetail{}, err
	}
	return s.store.Conversation(ctx, userID, id)
}

func (s *Service) UpdatePermissionProfile(ctx context.Context, userID, id string, profile domain.PermissionProfile) (domain.ConversationDetail, error) {
	if !profile.Valid() {
		return domain.ConversationDetail{}, domain.ErrInvalid
	}
	prior, err := s.store.Conversation(ctx, userID, id)
	if err != nil {
		return domain.ConversationDetail{}, err
	}
	if profileTightened(prior.PermissionProfile, profile) {
		turns, err := s.store.AgentTurns(ctx, userID, id)
		if err != nil {
			return domain.ConversationDetail{}, err
		}
		for _, turn := range turns {
			if turn.Status != "running" && turn.Status != "cancelling" && turn.Status != "awaiting_approval" {
				continue
			}
			if _, err := s.Cancel(ctx, userID, turn.ID, "permission_profile_reduced"); err != nil {
				if errors.Is(err, domain.ErrConflict) {
					latest, readErr := s.store.AgentTurns(ctx, userID, id)
					if readErr == nil {
						stillActive := false
						for _, current := range latest {
							if current.ID == turn.ID && (current.Status == "running" || current.Status == "cancelling" || current.Status == "awaiting_approval") {
								stillActive = true
							}
						}
						if !stillActive {
							continue
						}
					}
					if readErr != nil {
						return domain.ConversationDetail{}, readErr
					}
				}
				return domain.ConversationDetail{}, err
			}
		}
	}
	if err := s.store.UpdateConversationPermissionProfile(ctx, userID, id, profile); err != nil {
		return domain.ConversationDetail{}, err
	}
	return s.store.Conversation(ctx, userID, id)
}

func profileTightened(prior, next domain.PermissionProfile) bool {
	if !prior.Valid() {
		return true
	}
	if next == domain.PermissionProfileReadOnly {
		return prior != domain.PermissionProfileReadOnly
	}
	return prior == domain.PermissionProfileWorkspaceAutonomy && next == domain.PermissionProfileAskOnSensitive
}

func (s *Service) UpdateTitle(ctx context.Context, userID, id, title string) (domain.ConversationDetail, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return domain.ConversationDetail{}, domain.ErrInvalid
	}
	if err := s.store.UpdateConversationTitle(ctx, userID, id, title); err != nil {
		return domain.ConversationDetail{}, err
	}
	return s.store.Conversation(ctx, userID, id)
}

func (s *Service) GenerateTitle(ctx context.Context, userID, id, providerID string) (domain.ConversationDetail, error) {
	detail, err := s.store.Conversation(ctx, userID, id)
	if err != nil {
		return domain.ConversationDetail{}, err
	}

	targetProviderID := strings.TrimSpace(providerID)
	if targetProviderID == "" {
		targetProviderID = detail.ProviderID
	}
	if targetProviderID == "" && s.providers != nil {
		if list, err := s.providers.List(ctx, userID); err == nil && len(list) > 0 {
			targetProviderID = list[0].ID
		}
	}

	var userFirstMsg string
	var conversationText strings.Builder
	for i, m := range detail.Messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		if userFirstMsg == "" && m.Role == "user" {
			userFirstMsg = content
		}
		if i < 6 {
			role := "用户"
			if m.Role == "assistant" {
				role = "助手"
			}
			runes := []rune(content)
			if len(runes) > 150 {
				content = string(runes[:150]) + "..."
			}
			conversationText.WriteString(fmt.Sprintf("%s: %s\n", role, content))
		}
	}

	if userFirstMsg == "" {
		newTitle := "新对话"
		_ = s.store.UpdateConversationTitle(ctx, userID, id, newTitle)
		detail.Title = newTitle
		return detail, nil
	}

	generatedTitle := ""
	if targetProviderID != "" && s.providers != nil {
		prompt := `你是一个会话标题提炼专家。请仔细阅读以下对话记录片段，为该会话提炼一个简短、规范、一目了然的会话标题。
严格遵循以下要求：
1. 语义明确：直接概括该对话要做什么或解决什么问题（例如“开发智能会话标题插件”、“排查本地连接超时”、“探讨数据模型设计”等）。
2. 长度控制：严格控制在 6 到 18 个汉字（若为英文单词控制在 3 到 6 个词）以内。
3. 严禁分类标签：绝对不要输出任何【】分类标签、[]中括号或分类前缀（严禁输出任何形如【插件开发】、【日常问答】、【功能开发】等前缀）。
4. 纯净输出：严禁包含任何标点符号、引号、书名号、“标题：”前缀、换行或解释说明，仅直接输出提炼后的标题文字。`

		chatMsgs := []provider.ChatMessage{
			{
				Role:    "user",
				Content: prompt + "\n\n对话记录片段：\n" + conversationText.String() + "\n请直接输出标题：",
			},
		}

		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		if resp, err := s.providers.Complete(callCtx, userID, targetProviderID, chatMsgs); err == nil {
			resp = strings.TrimSpace(resp)
			if resp != "" {
				generatedTitle = cleanTitle(resp)
			}
		}
	}

	if generatedTitle == "" {
		generatedTitle = fallbackTitle(userFirstMsg)
	}

	if err := s.store.UpdateConversationTitle(ctx, userID, id, generatedTitle); err != nil {
		return domain.ConversationDetail{}, err
	}
	detail.Title = generatedTitle
	return detail, nil
}

func cleanTitle(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "`\"'“”‘’*# \t\r\n")
	if idx := strings.Index(raw, "\n"); idx != -1 {
		raw = strings.TrimSpace(raw[:idx])
	}
	raw = strings.Trim(raw, "`\"'“”‘’*# \t\r\n")

	// Strip common title prefixes
	for _, p := range []string{
		"会话标题：", "会话标题:", "标题：", "标题:", "Title:", "title:", "主题：", "主题:",
	} {
		if strings.HasPrefix(raw, p) {
			raw = strings.TrimSpace(strings.TrimPrefix(raw, p))
		}
	}

	// Strip bracket tags if present (legacy or stray)
	if strings.HasPrefix(raw, "【") && strings.Contains(raw, "】") {
		end := strings.Index(raw, "】")
		after := strings.TrimSpace(raw[end+len("】"):])
		if after != "" {
			raw = after
		} else {
			raw = strings.TrimSpace(raw[len("【"):end])
		}
	} else if strings.HasPrefix(raw, "[") && strings.Contains(raw, "]") {
		end := strings.Index(raw, "]")
		after := strings.TrimSpace(raw[end+1:])
		if after != "" {
			raw = after
		} else {
			raw = strings.TrimSpace(raw[1:end])
		}
	}

	raw = strings.Trim(raw, "【】[]`\"'“”‘’*# \t\r\n")
	raw = strings.TrimRight(raw, "。，、！？.!? \t")

	if raw == "" {
		return "新对话"
	}
	return truncateRuneString(raw, 22)
}

func fallbackTitle(firstMsg string) string {
	clean := strings.TrimSpace(firstMsg)
	clean = strings.ReplaceAll(clean, "\r\n", " ")
	clean = strings.ReplaceAll(clean, "\n", " ")
	clean = strings.Trim(clean, "`\"'“”‘’*# \t\r\n")

	fillers := []string{
		"帮我尝试做一个", "帮我尝试做个", "帮我做一个", "帮我做个", "帮我写一个", "帮我写个", "帮我实现一个", "帮我实现个",
		"请帮我", "帮我", "我想让你", "我想做一个", "我想实现", "请教一下", "请问一下", "请问",
	}
	for _, f := range fillers {
		if strings.HasPrefix(clean, f) {
			clean = strings.TrimSpace(strings.TrimPrefix(clean, f))
			break
		}
	}

	clean = cleanTitle(clean)
	runes := []rune(clean)
	if len(runes) > 18 {
		clean = string(runes[:18]) + "…"
	}
	if clean == "" {
		clean = "新对话"
	}
	return clean
}

func truncateRuneString(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "…"
}
func (s *Service) Turn(ctx context.Context, userID, conversationID, content string) (domain.Message, error) {
	return s.runTurn(ctx, userID, conversationID, content, nil)
}

type turnStart struct {
	receipt domain.TurnReceipt
	err     error
}

func (s *Service) Submit(_ context.Context, userID, conversationID, content string) (domain.TurnReceipt, error) {
	receipt, err := s.submitRun(userID, conversationID, content, "", "", "", nil, "")
	if !errors.Is(err, domain.ErrBusy) {
		return receipt, err
	}
	item, queueErr := s.QueueInput(s.hostCtx, userID, conversationID, content)
	if queueErr != nil {
		return domain.TurnReceipt{}, errors.Join(err, queueErr)
	}
	status := "queued"
	if item.Status == "claimed" && item.TurnID != "" {
		status = "running"
	}
	return domain.TurnReceipt{TurnID: item.TurnID, ConversationID: conversationID, Status: status}, nil
}

func (s *Service) submitRun(userID, conversationID, content, inputMessageID, retryOf, inboxID string, revisedContent *string, branchConversationID string) (domain.TurnReceipt, error) {
	started := make(chan turnStart, 1)
	finished := make(chan error, 1)
	go func() {
		_, runErr := s.runTurnWithOptions(s.hostCtx, userID, conversationID, content, inputMessageID, retryOf, inboxID, revisedContent, branchConversationID, started)
		finished <- runErr
	}()
	result := <-started
	if result.err == nil {
		go func() {
			if runErr := <-finished; runErr != nil {
				slog.Error("agent turn terminated unsuccessfully", "conversation_id", result.receipt.ConversationID, "turn_id", result.receipt.TurnID, "error_class", agentErrorClass(runErr))
			}
		}()
	}
	return result.receipt, result.err
}

func agentErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, domain.ErrConflict):
		return "conflict"
	case errors.Is(err, domain.ErrNotFound):
		return "not_found"
	case errors.Is(err, domain.ErrInvalid):
		return "invalid"
	default:
		return "runtime_error"
	}
}

func (s *Service) runTurn(ctx context.Context, userID, conversationID, content string, started chan<- turnStart) (domain.Message, error) {
	return s.runTurnWithOptions(ctx, userID, conversationID, content, "", "", "", nil, "", started)
}

func (s *Service) runTurnWithOptions(ctx context.Context, userID, conversationID, content, inputMessageID, retryOf, inboxID string, revisedContent *string, branchConversationID string, started chan<- turnStart) (domain.Message, error) {
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
	select {
	case s.runSlots <- struct{}{}:
	default:
		err := fmt.Errorf("%w: all %d Agent execution slots are in use; queued conversation inputs will start when a slot frees", domain.ErrBusy, s.runLimits.MaxConcurrentRuns)
		signalStart(domain.TurnReceipt{}, err)
		return domain.Message{}, err
	}
	defer func() {
		<-s.runSlots
		go s.ResumeQueuedInputs(userID)
	}()
	budgetCtx, budgetCancel := context.WithTimeout(ctx, s.runLimits.MaxRunDuration)
	defer budgetCancel()
	ctx = budgetCtx
	detail, err := s.store.Conversation(ctx, userID, conversationID)
	if err != nil {
		signalStart(domain.TurnReceipt{}, err)
		return domain.Message{}, err
	}
	sourceConversationID := conversationID
	generation, err := s.evolution.ConversationGeneration(ctx, userID, conversationID)
	if err != nil {
		signalStart(domain.TurnReceipt{}, err)
		return domain.Message{}, err
	}
	now := time.Now().UTC()
	branchConversation := domain.Conversation{}
	if branchConversationID != "" {
		branchConversation = domain.Conversation{
			ID:                    branchConversationID,
			UserID:                userID,
			Title:                 detail.Title + "（分支）",
			ProviderID:            detail.ProviderID,
			AgentGenerationID:     generation.ID,
			AgentDefinitionDigest: generation.DefinitionDigest,
			ProjectID:             detail.ProjectID,
			PermissionProfile:     detail.PermissionProfile,
			ParentConversationID:  detail.ID,
			BranchFromMessageID:   inputMessageID,
			CreatedAt:             now,
			UpdatedAt:             now,
		}
		conversationID = branchConversationID
	}
	userMessage := domain.Message{ID: id("msg"), ConversationID: conversationID, Role: "user", Content: content, CreatedAt: now}
	if inputMessageID != "" {
		found := false
		for _, message := range detail.Messages {
			if message.ID == inputMessageID && message.Role == "user" {
				if branchConversationID == "" {
					userMessage = message
					userMessage.Content = content
					userMessage.CreatedAt = now
				}
				found = true
				break
			}
		}
		if !found {
			signalStart(domain.TurnReceipt{}, domain.ErrNotFound)
			return domain.Message{}, domain.ErrNotFound
		}
	}
	profile := detail.PermissionProfile
	if !profile.Valid() {
		signalStart(domain.TurnReceipt{}, domain.ErrInvalid)
		return domain.Message{}, fmt.Errorf("conversation has invalid permission profile: %w", domain.ErrInvalid)
	}
	turn := domain.AgentTurn{ID: id("turn"), ConversationID: conversationID, UserID: userID, InputMessageID: userMessage.ID, RetryOfTurnID: retryOf, ProviderID: detail.ProviderID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, PermissionProfile: profile, Status: "running", StartedAt: now, UpdatedAt: now}
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
	scope, err := newTurnScope(s, userID, conversationID, turn.ID)
	if err != nil {
		signalStart(domain.TurnReceipt{}, err)
		return domain.Message{}, err
	}
	scope.permissionProfile = profile
	messageCount := len(detail.Messages)
	if branchConversationID != "" {
		messageCount = 1
		for _, message := range detail.Messages {
			if message.ID == inputMessageID {
				break
			}
			messageCount++
		}
	} else if inputMessageID == "" {
		messageCount++
	}
	startedDetails, _ := json.Marshal(map[string]any{"messageCount": messageCount, "pinnedTools": len(scope.tools), "pinnedSkills": len(scope.skills), "generationId": generation.ID, "definitionDigest": generation.DefinitionDigest, "strategy": generation.Definition.Spec.Strategy, "retryOfTurnId": retryOf, "inboxId": inboxID})
	var startErr error
	switch {
	case branchConversationID != "":
		startErr = s.store.StartBranchAgentTurn(ctx, userID, sourceConversationID, retryOf, inputMessageID, branchConversation, turn, userMessage, startedDetails)
	case retryOf != "":
		startErr = s.store.StartRetryAgentTurn(ctx, userID, turn, userMessage, retryOf, revisedContent, startedDetails)
	case inboxID != "":
		startErr = s.store.StartQueuedAgentTurn(ctx, userID, turn, userMessage, inboxID, startedDetails)
	default:
		startErr = s.store.StartAgentTurn(ctx, userID, turn, userMessage, startedDetails)
	}
	if startErr != nil {
		cleanupErr := scope.Close()
		signalStart(domain.TurnReceipt{}, startErr)
		return domain.Message{}, errors.Join(startErr, cleanupErr)
	}
	if branchConversationID != "" {
		branchDetail, readErr := s.store.Conversation(ctx, userID, branchConversationID)
		if readErr != nil {
			cleanupErr := scope.Close()
			_ = s.finishFailedTurn(ctx, userID, turn.ID, branchConversationID, "failed", "branch_readback_failed", errors.Join(readErr, cleanupErr), domain.RunMetrics{})
			s.events.notify(turn.ID)
			failedTurn, turnReadErr := s.store.AgentTurn(ctx, userID, turn.ID)
			if turnReadErr != nil {
				signalStart(domain.TurnReceipt{}, errors.Join(readErr, cleanupErr, turnReadErr))
				return domain.Message{}, errors.Join(readErr, cleanupErr, turnReadErr)
			}
			if failedTurn.Status != "failed" || failedTurn.StopReason != "branch_readback_failed" {
				signalStart(domain.TurnReceipt{}, errors.Join(readErr, cleanupErr, domain.ErrConflict))
				return domain.Message{}, errors.Join(readErr, cleanupErr, domain.ErrConflict)
			}
			receipt := domain.TurnReceipt{TurnID: turn.ID, ConversationID: branchConversationID, InputMessageID: userMessage.ID, Status: failedTurn.Status}
			signalStart(receipt, nil)
			return domain.Message{}, errors.Join(readErr, cleanupErr)
		}
		detail = branchDetail
	}
	s.events.notify(turn.ID)
	signalStart(domain.TurnReceipt{TurnID: turn.ID, ConversationID: conversationID, InputMessageID: userMessage.ID, Status: "running"}, nil)
	if inputMessageID == "" {
		detail.Messages = append(detail.Messages, userMessage)
	} else {
		for index := range detail.Messages {
			if detail.Messages[index].ID == userMessage.ID {
				detail.Messages[index] = userMessage
				break
			}
		}
	}
	extraPrompt := ""
	if s.plugins != nil {
		extraPrompt = s.plugins.SkillsRegistry().GeneratePrompt()
	}
	projectWorkspaceEnabled := s.plugins == nil || s.plugins.IsProjectWorkspaceEnabled()
	projectBinding := domain.Project{}
	if projectWorkspaceEnabled && detail.ProjectID != "" {
		projectBinding, err = s.store.Project(ctx, userID, detail.ProjectID)
		if err != nil {
			cleanupErr := scope.Close()
			return domain.Message{}, s.failTurn(ctx, userID, turn.ID, conversationID, errors.Join(err, cleanupErr), "project_context_unavailable")
		}
		if projectBinding.Workdir != "" {
			scope.setWorkspaceRoot(projectBinding.Workdir)
		}
		var projPrompt strings.Builder
		projPrompt.WriteString("\n\n---\n")
		projPrompt.WriteString("## 当前所属项目: " + projectBinding.Name + "\n")
		if projectBinding.Workdir != "" {
			projPrompt.WriteString("- 项目工作目录: `" + projectBinding.Workdir + "`\n")
		}
		if projectBinding.RemoteRepoURL != "" {
			branchInfo := ""
			if projectBinding.RemoteBranch != "" {
				branchInfo = " (分支: `" + projectBinding.RemoteBranch + "`)"
			}
			projPrompt.WriteString("- 关联远程仓库: `" + projectBinding.RemoteRepoURL + "`" + branchInfo + "\n")
		}
		if projectBinding.InstructionsEnabled && strings.TrimSpace(projectBinding.Instructions) != "" {
			projPrompt.WriteString("### 项目全局设定与约定 (Project Instructions):\n")
			projPrompt.WriteString(strings.TrimSpace(projectBinding.Instructions) + "\n")
			projPrompt.WriteString("（当前对话属于此项目，请在交互与代码实现中严格遵守上述项目约定）\n")
		}
		extraPrompt += projPrompt.String()
	}
	contextTokens := 131072
	providerConfig, _, _, pErr := s.store.ProviderSecret(ctx, userID, detail.ProviderID)
	if pErr != nil {
		cleanupErr := scope.Close()
		return domain.Message{}, s.failTurn(ctx, userID, turn.ID, conversationID, errors.Join(pErr, cleanupErr), "provider_configuration_unavailable")
	}
	if providerConfig.ContextWindow > 0 {
		contextTokens = providerConfig.ContextWindow
	} else {
		contextTokens = EstimateModelContextTokens(providerConfig.Model)
	}
	scope.setContextWindow(contextTokens)
	runtimeFingerprint, fingerprintErr := turnRuntimeFingerprint(providerConfig, generation, profile, projectBinding, projectWorkspaceEnabled, scope)
	if fingerprintErr != nil {
		cleanupErr := scope.Close()
		return domain.Message{}, s.failTurn(ctx, userID, turn.ID, conversationID, errors.Join(fingerprintErr, cleanupErr), "checkpoint_fingerprint_failed")
	}
	messages, omitted := buildContext(detail, generation, extraPrompt, contextTokens)
	trace := newTraceRecorder(runCtx, s.store, userID, turn.ID, s.events.notify, s.providers.SealRunCheckpoint)
	originalContextChars := 0
	for _, message := range detail.Messages {
		originalContextChars += len(message.Content)
	}
	compactedContextChars := 0
	for _, message := range messages {
		if message.Role != "system" {
			compactedContextChars += len(message.Content)
		}
	}
	if omitted > 0 || compactedContextChars < originalContextChars {
		if err := trace.emit("context.compacted", map[string]any{"scope": "conversation", "omittedMessages": omitted, "compactedMessages": omitted, "retainedMessages": len(detail.Messages) - omitted, "originalChars": originalContextChars, "compactedChars": compactedContextChars}); err != nil {
			cleanupErr := scope.Close()
			reason := "journal_error"
			if cleanupErr != nil {
				reason = "artifact_cleanup_failed"
			}
			return domain.Message{}, s.failTurn(ctx, userID, turn.ID, conversationID, errors.Join(err, cleanupErr), reason)
		}
	}
	detailedTrace := s.plugins == nil || s.plugins.IsRunInspectorEnabled()
	checkpointWriter := func(kind string, details any, state loopCheckpoint) error {
		state.RuntimeFingerprint, err = turnRuntimeFingerprint(providerConfig, generation, profile, projectBinding, projectWorkspaceEnabled, scope)
		if err != nil {
			return err
		}
		return trace.checkpoint(kind, details, state)
	}
	result, err := executeLoop(runCtx, s.providers, loopRequest{UserID: userID, ProviderID: detail.ProviderID, Generation: generation, Messages: messages, RuntimeFingerprint: runtimeFingerprint, Scope: scope, Emit: trace.emit, PersistCheckpoint: checkpointWriter, DetailedTrace: detailedTrace, TokenBudget: s.runLimits.MaxTokensPerRun, ModelCallBudget: s.runLimits.MaxModelCalls})
	if err != nil {
		status, reason := "failed", "runtime_error"
		if errors.Is(runCtx.Err(), context.Canceled) {
			status, reason = "cancelled", "cancelled"
		} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			status, reason = "incomplete", "time_limit"
		}
		if cleanupErr := scope.Close(); cleanupErr != nil {
			status, reason = "failed", "artifact_cleanup_failed"
			err = errors.Join(err, cleanupErr)
		}
		return domain.Message{}, s.finishFailedTurn(ctx, userID, turn.ID, conversationID, status, reason, err, result.Metrics)
	}
	assistant := domain.Message{ID: id("msg"), ConversationID: conversationID, Role: "assistant", Content: result.Reply, CreatedAt: time.Now().UTC()}
	details, _ := json.Marshal(map[string]any{"replyBytes": len(result.Reply), "metrics": result.Metrics, "generationId": generation.ID})
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	status, stopReason := "completed", "assistant_response"
	if result.Metrics.ReachedModelCallLimit {
		status, stopReason = "incomplete", "model_call_limit"
	} else if result.Metrics.ReachedTokenLimit {
		status, stopReason = "incomplete", "token_limit"
	} else if result.Metrics.ReachedStepLimit {
		status, stopReason = "incomplete", "step_limit"
	}
	if cleanupErr := scope.Close(); cleanupErr != nil {
		return domain.Message{}, s.finishFailedTurn(ctx, userID, turn.ID, conversationID, "failed", "artifact_cleanup_failed", cleanupErr, result.Metrics)
	}
	if err := s.store.FinishAgentTurn(finishCtx, userID, turn.ID, status, stopReason, &assistant, details); err != nil {
		slog.Error("failed to persist terminal agent turn", "conversation_id", conversationID, "turn_id", turn.ID, "stop_reason", stopReason, "error", err)
		return domain.Message{}, err
	}
	s.events.notify(turn.ID)

	return assistant, nil
}

func (s *Service) failTurn(ctx context.Context, userID, turnID, conversationID string, cause error, reason string) error {
	return s.finishFailedTurn(ctx, userID, turnID, conversationID, "failed", reason, cause, domain.RunMetrics{})
}

func (s *Service) finishFailedTurn(ctx context.Context, userID, turnID, conversationID, status, reason string, cause error, metrics domain.RunMetrics) error {
	details, _ := json.Marshal(map[string]any{"error": cause.Error(), "metrics": metrics})
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if journalErr := s.store.FinishAgentTurn(finishCtx, userID, turnID, status, reason, nil, details); journalErr != nil {
		slog.Error("failed to persist terminal agent turn", "conversation_id", conversationID, "turn_id", turnID, "stop_reason", reason, "error", journalErr)
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
	select {
	case s.runSlots <- struct{}{}:
	case <-ctx.Done():
		return "", domain.RunMetrics{}, ctx.Err()
	}
	defer func() {
		<-s.runSlots
		go s.ResumeQueuedInputs(userID)
	}()
	scope, err := newEvaluationScope(s, userID)
	if err != nil {
		return "", domain.RunMetrics{}, err
	}
	messages := []provider.ChatMessage{
		{Role: "system", Content: generation.Definition.Spec.SystemPrompt + "\n\nEvaluation mode: work only through the exposed read-only capabilities. Return the actual task result, not a description of this evaluation."},
		{Role: "user", Content: prompt},
	}
	evaluationCtx, cancel := context.WithTimeout(ctx, s.runLimits.MaxRunDuration)
	defer cancel()
	result, err := executeLoop(evaluationCtx, s.providers, loopRequest{UserID: userID, ProviderID: providerID, Generation: generation, Messages: messages, Scope: scope, TokenBudget: s.runLimits.MaxTokensPerRun, ModelCallBudget: s.runLimits.MaxModelCalls})
	cleanupErr := scope.Close()
	return result.Reply, result.Metrics, errors.Join(err, cleanupErr)
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
		Reason           string          `json:"reason"`
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
	case "axiom_plugin_install":
		var project pluginforge.Project
		var installation pluginforge.Installation
		if strings.TrimSpace(input.ReleaseID) == "" {
			err = domain.ErrInvalid
			break
		}
		project, err = s.forge.ApproveRelease(ctx, userID, input.ProjectID, input.ReleaseID)
		if err == nil {
			project, installation, err = s.forge.InstallRelease(ctx, userID, input.ProjectID, input.ReleaseID)
		}
		value = map[string]any{"project": project, "installation": installation}
	case "axiom_plugin_rollback":
		var project pluginforge.Project
		var installation pluginforge.Installation
		project, installation, err = s.forge.Rollback(ctx, userID, input.ProjectID, input.ReleaseID)
		value = map[string]any{"project": project, "installation": installation}
	case "axiom_plugin_mark_unusable":
		value, err = s.forge.MarkReleaseUnusable(ctx, userID, input.ProjectID, input.ReleaseID, input.Reason)
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
