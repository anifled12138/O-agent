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
	"axiom.local/agent/internal/plugins"
	"axiom.local/agent/internal/promotion"
	"axiom.local/agent/internal/provider"
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
	plugins       *plugins.Manager
	runningMu     sync.Mutex
	running       map[string]context.CancelCauseFunc
	events        *eventBroker
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
	return &Service{hostCtx: hostCtx, store: store, providers: providers, forge: forge, evolution: evolutionService, fragments: capability.NewRegistry(scripts), capsules: capsules, promotions: promotions, running: map[string]context.CancelCauseFunc{}, events: newEventBroker(), workspaceRoot: workspaceRoot, plugins: plugins.NewManager(workspaceRoot)}, nil
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
	c := domain.Conversation{ID: id("run"), UserID: userID, Title: title, ProviderID: providerID, ProjectID: projectID, CreatedAt: now, UpdatedAt: now}
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
	extraPrompt := ""
	if s.plugins != nil {
		extraPrompt = s.plugins.SkillsRegistry().GeneratePrompt()
	}
	if (s.plugins == nil || s.plugins.IsProjectWorkspaceEnabled()) && detail.ProjectID != "" {
		if proj, err := s.store.Project(ctx, userID, detail.ProjectID); err == nil {
			if proj.Workdir != "" {
				scope.setWorkspaceRoot(proj.Workdir)
			}
			var projPrompt strings.Builder
			projPrompt.WriteString("\n\n---\n")
			projPrompt.WriteString("## 当前所属项目: " + proj.Name + "\n")
			if proj.Workdir != "" {
				projPrompt.WriteString("- 项目工作目录: `" + proj.Workdir + "`\n")
			}
			if proj.RemoteRepoURL != "" {
				branchInfo := ""
				if proj.RemoteBranch != "" {
					branchInfo = " (分支: `" + proj.RemoteBranch + "`)"
				}
				projPrompt.WriteString("- 关联远程仓库: `" + proj.RemoteRepoURL + "`" + branchInfo + "\n")
			}
			if proj.InstructionsEnabled && strings.TrimSpace(proj.Instructions) != "" {
				projPrompt.WriteString("### 项目全局设定与约定 (Project Instructions):\n")
				projPrompt.WriteString(strings.TrimSpace(proj.Instructions) + "\n")
				projPrompt.WriteString("（当前对话属于此项目，请在交互与代码实现中严格遵守上述项目约定）\n")
			}
			extraPrompt += projPrompt.String()
		}
	}
	contextTokens := 131072
	if prov, _, _, pErr := s.store.ProviderSecret(ctx, userID, detail.ProviderID); pErr == nil {
		if prov.ContextWindow > 0 {
			contextTokens = prov.ContextWindow
		} else {
			contextTokens = EstimateModelContextTokens(prov.Model)
		}
	}
	scope.setContextWindow(contextTokens)
	messages, omitted := buildContext(detail, generation, extraPrompt, contextTokens)
	trace := newTraceRecorder(runCtx, s.store, userID, turn.ID, s.events.notify)
	if omitted > 0 {
		if err := trace.emit("context.compacted", map[string]any{"omittedMessages": omitted, "compactedMessages": omitted, "retainedMessages": len(detail.Messages) - omitted}); err != nil {
			return domain.Message{}, s.failTurn(ctx, userID, turn.ID, err, "journal_error")
		}
	}
	detailedTrace := s.plugins == nil || s.plugins.IsRunInspectorEnabled()
	result, err := executeLoop(runCtx, s.providers, loopRequest{UserID: userID, ProviderID: detail.ProviderID, Generation: generation, Messages: messages, Scope: scope, Emit: trace.emit, DetailedTrace: detailedTrace})
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
	status, stopReason := "completed", "assistant_response"
	if result.Metrics.ReachedStepLimit {
		status, stopReason = "incomplete", "step_limit"
	}
	if err := s.store.FinishAgentTurn(finishCtx, userID, turn.ID, status, stopReason, &assistant, details); err != nil {
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
	case "axiom_plugin_install":
		var project pluginforge.Project
		var installation pluginforge.Installation
		// Installation is the explicit authorization boundary in the simplified
		// local workflow. Keep the digest-bound grant, but expose no separate
		// approve tool or misleading user-only approval claim.
		if p, pErr := s.forge.Approve(ctx, userID, input.ProjectID); pErr == nil {
			project = p
		} else if _, _, requestErr := s.forge.RequestApproval(ctx, userID, input.ProjectID); requestErr == nil {
			if p, grantErr := s.forge.Approve(ctx, userID, input.ProjectID); grantErr == nil {
				project = p
			}
		}
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
