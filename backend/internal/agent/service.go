package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/storage"
)

const systemPrompt = `You are Axiom, a local-first engineering agent. Be direct, evidence-driven, and explicit about uncertainty. Use tools when they materially improve the result. Never claim a tool ran unless its result is present. You may propose, generate, and test plugins when a durable capability is missing. Permission approval is reserved for the user; installation only succeeds for an already-approved immutable release.`

type Service struct {
	store     *storage.Store
	providers *provider.Service
	forge     *pluginforge.Service
}

func New(store *storage.Store, providers *provider.Service, forge *pluginforge.Service) *Service {
	return &Service{store: store, providers: providers, forge: forge}
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
	return c, s.store.CreateConversation(ctx, c)
}
func (s *Service) Turn(ctx context.Context, userID, conversationID, content string) (domain.Message, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return domain.Message{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	userMessage := domain.Message{ID: id("msg"), ConversationID: conversationID, Role: "user", Content: content, CreatedAt: now}
	if err := s.store.AddMessage(ctx, userID, userMessage); err != nil {
		return domain.Message{}, err
	}
	detail, err := s.store.Conversation(ctx, userID, conversationID)
	if err != nil {
		return domain.Message{}, err
	}
	messages, omitted := buildContext(detail)
	scope := newTurnScope(s, userID)
	defer scope.Close()
	trace := newTraceRecorder(ctx, s.store, userID, conversationID)
	trace.emit("turn.started", map[string]any{"messageCount": len(detail.Messages), "omittedMessages": omitted, "pinnedTools": len(scope.tools), "pinnedSkills": len(scope.skills)})
	var reply string
	for step := 0; step < 12; step++ {
		definitions := scope.definitions()
		trace.emit("model.requested", map[string]any{"step": step + 1, "messageCount": len(messages), "toolDefinitionCount": len(definitions)})
		completion, completeErr := s.providers.CompleteWithTools(ctx, userID, detail.ProviderID, messages, definitions)
		if completeErr != nil {
			trace.emit("model.failed", map[string]any{"step": step + 1, "error": completeErr.Error()})
			return domain.Message{}, completeErr
		}
		trace.emit("model.completed", map[string]any{"step": step + 1, "toolCallCount": len(completion.ToolCalls), "contentBytes": len(completion.Content)})
		if len(completion.ToolCalls) == 0 {
			reply = strings.TrimSpace(completion.Content)
			break
		}
		messages = append(messages, provider.ChatMessage{Role: "assistant", Content: completion.Content, ToolCalls: completion.ToolCalls})
		for _, call := range completion.ToolCalls {
			started := time.Now()
			trace.emit("tool.started", map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "argumentBytes": len(call.Function.Arguments)})
			result := scope.execute(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments))
			trace.emit("tool.completed", map[string]any{"step": step + 1, "toolCallId": call.ID, "name": call.Function.Name, "resultBytes": len(result), "durationMillis": time.Since(started).Milliseconds(), "ok": toolResultOK(result)})
			messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: call.ID, Content: string(result)})
		}
	}
	if reply == "" {
		reply = "I reached the tool execution limit for this turn. The completed tool results are preserved in the current run; continue the mission to resume."
	}
	assistant := domain.Message{ID: id("msg"), ConversationID: conversationID, Role: "assistant", Content: reply, CreatedAt: time.Now().UTC()}
	err = s.store.AddMessage(ctx, userID, assistant)
	trace.emit("turn.completed", map[string]any{"replyBytes": len(reply), "persisted": err == nil})
	return assistant, err
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
		ProjectID    string          `json:"projectId"`
		CapabilityID string          `json:"capabilityId"`
		Input        json.RawMessage `json:"input"`
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		Path         string          `json:"path"`
		Content      string          `json:"content"`
		ReleaseID    string          `json:"releaseId"`
		Shape        string          `json:"shape"`
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
	case "axiom_plugin_write_source":
		value, err = s.forge.WriteSourceFile(ctx, userID, input.ProjectID, pluginforge.SourceFileInput{Path: input.Path, Content: input.Content})
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
