package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/mcp"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/skills"
)

type PluginType string

const (
	TypeCore    PluginType = "core"
	TypeSkill   PluginType = "skill"
	TypeMCP     PluginType = "mcp"
	TypeRelease PluginType = "release"
	TypeContext PluginType = "context"
	TypeModel   PluginType = "model"
)

type PluginStatus string

const (
	StatusEnabled  PluginStatus = "enabled"
	StatusDisabled PluginStatus = "disabled"
	StatusError    PluginStatus = "error"
)

type UnifiedPlugin struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         PluginType        `json:"type"`
	Description  string            `json:"description"`
	Status       PluginStatus      `json:"status"`
	Error        string            `json:"error,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type Manager struct {
	mu                sync.RWMutex
	workspaceRoot     string
	skillsRegistry    *skills.Registry
	mcpManager        *mcp.Manager
	coreTools         map[string]coretools.Tool
	coreEnabled       map[string]bool
	corePluginName    map[string]string
	corePluginInfo    map[string]func() map[string]string
	contextCompactor  *ContextCompactorPlugin
	providerLister    func(ctx context.Context) ([]domain.Provider, error)
	disabledModels    map[string]bool
	contextCompaction CompactionSettings
	statePath         string
}

// CompactionSettings is persisted with the core plugin settings. The host
// coordinator enforces hard limits and passes the selected policy to the plugin.
type CompactionSettings struct {
	Mode                               string `json:"mode"`
	TriggerPercent                     int    `json:"triggerPercent"`
	MinimumGrowthBeforeRecompactTokens int    `json:"minimumGrowthBeforeRecompactTokens"`
	RecentContextTokens                int    `json:"recentContextTokens"`
	CompactionCallBudget               int    `json:"compactionCallBudget"`
	Version                            int    `json:"version"`
}

type persistedState struct {
	Version           int                `json:"version"`
	CoreEnabled       map[string]bool    `json:"coreEnabled"`
	DisabledModels    map[string]bool    `json:"disabledModels,omitempty"`
	ContextCompaction CompactionSettings `json:"contextCompaction"`
}

const persistedStateVersion = 3

const hardCompactionTriggerPercent = 75

func defaultCompactionSettings() CompactionSettings {
	return CompactionSettings{Mode: "auto", TriggerPercent: hardCompactionTriggerPercent, MinimumGrowthBeforeRecompactTokens: 8000, RecentContextTokens: 20000, CompactionCallBudget: 12, Version: 2}
}

func validateCompactionSettings(settings CompactionSettings) error {
	switch settings.Mode {
	case "auto", "semantic", "provider_native", "extractive":
	default:
		return fmt.Errorf("unsupported compaction mode %q", settings.Mode)
	}
	if settings.TriggerPercent < 10 || settings.TriggerPercent > hardCompactionTriggerPercent {
		return fmt.Errorf("compaction trigger must be between 10 and %d percent", hardCompactionTriggerPercent)
	}
	if settings.MinimumGrowthBeforeRecompactTokens < 1000 || settings.MinimumGrowthBeforeRecompactTokens > 200000 {
		return fmt.Errorf("minimum growth must be between 1000 and 200000 tokens")
	}
	if settings.RecentContextTokens < 1000 || settings.RecentContextTokens > 100000 {
		return fmt.Errorf("recent context retention must be between 1000 and 100000 tokens")
	}
	if settings.CompactionCallBudget < 1 || settings.CompactionCallBudget > 32 {
		return fmt.Errorf("compaction model-call budget must be between 1 and 32 calls")
	}
	return nil
}

type CorePluginRegistration struct {
	Name        string
	DisplayName string
	Tool        coretools.Tool
	Enabled     bool
	Metadata    func() map[string]string
}

func NewManager(workspaceRoot string) (*Manager, error) {
	manager, err := newManager(workspaceRoot)
	if err != nil {
		return nil, err
	}
	if err := manager.loadState(); err != nil {
		return nil, err
	}
	return manager, nil
}

// NewManagerWithCorePlugins builds the complete core registry before loading
// persisted settings, so settings are applied to the registry as one unit.
func NewManagerWithCorePlugins(workspaceRoot string, registrations ...CorePluginRegistration) (*Manager, error) {
	manager, err := newManager(workspaceRoot)
	if err != nil {
		return nil, err
	}
	for _, registration := range registrations {
		if err := manager.registerCorePlugin(registration); err != nil {
			return nil, err
		}
	}
	if err := manager.loadState(); err != nil {
		return nil, err
	}
	return manager, nil
}

func newManager(workspaceRoot string) (*Manager, error) {
	skillsReg, err := skills.NewRegistry(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("load workspace skills: %w", err)
	}
	mcpMgr := mcp.NewManagerWithWorkspace(workspaceRoot)
	contextPlugin := NewDefaultContextManagerPlugin()
	contextCompactor := NewContextCompactorPluginWithPolicy(contextPlugin)

	tools := coretools.GetCoreTools(workspaceRoot)
	coreMap := make(map[string]coretools.Tool, len(tools))
	coreEnabled := make(map[string]bool, len(tools))
	for _, t := range tools {
		name := t.Definition.Function.Name
		coreMap[name] = t
		coreEnabled[name] = true
	}
	coreEnabled["model_selector"] = true
	coreEnabled["context_compactor"] = true
	coreEnabled["conversation_title"] = true
	coreEnabled["project_workspace"] = true
	coreEnabled["run_inspector"] = true
	coreEnabled["conversation_fork"] = true

	manager := &Manager{
		workspaceRoot:     workspaceRoot,
		skillsRegistry:    skillsReg,
		mcpManager:        mcpMgr,
		coreTools:         coreMap,
		coreEnabled:       coreEnabled,
		corePluginName:    make(map[string]string),
		corePluginInfo:    make(map[string]func() map[string]string),
		contextCompactor:  contextCompactor,
		disabledModels:    make(map[string]bool),
		contextCompaction: defaultCompactionSettings(),
		statePath:         filepath.Join(workspaceRoot, ".axiom", "plugin-state.json"),
	}
	return manager, nil
}

func (m *Manager) registerCorePlugin(registration CorePluginRegistration) error {
	if registration.Name == "" || registration.DisplayName == "" || registration.Tool.Definition.Function.Name != registration.Name || registration.Tool.Handler == nil {
		return fmt.Errorf("invalid core plugin registration")
	}
	if _, exists := m.coreTools[registration.Name]; exists {
		return fmt.Errorf("core plugin %q is already registered", registration.Name)
	}
	m.coreTools[registration.Name] = registration.Tool
	m.corePluginName[registration.Name] = registration.DisplayName
	m.coreEnabled[registration.Name] = registration.Enabled
	if registration.Metadata != nil {
		m.corePluginInfo[registration.Name] = registration.Metadata
	}
	return nil
}

func (m *Manager) SkillsRegistry() *skills.Registry {
	return m.skillsRegistry
}

func (m *Manager) MCPManager() *mcp.Manager {
	return m.mcpManager
}

func (m *Manager) ContextCompactor() *ContextCompactorPlugin {
	return m.contextCompactor
}

func (m *Manager) SetProviderLister(lister func(ctx context.Context) ([]domain.Provider, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providerLister = lister
}

func (m *Manager) IsContextCompactorEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.coreEnabled["context_compactor"]
}

func (m *Manager) ContextCompactionSettings() CompactionSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.contextCompaction
}

func (m *Manager) SetContextCompactionSettings(settings CompactionSettings) (CompactionSettings, error) {
	settings.Version = 2
	if err := validateCompactionSettings(settings); err != nil {
		return CompactionSettings{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.contextCompaction
	m.contextCompaction = settings
	if err := m.persistStateLocked(); err != nil {
		m.contextCompaction = previous
		return CompactionSettings{}, fmt.Errorf("persist context compaction settings: %w", err)
	}
	return m.contextCompaction, nil
}

func (m *Manager) IsProjectWorkspaceEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.coreEnabled["project_workspace"]
}

func (m *Manager) IsRunInspectorEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.coreEnabled["run_inspector"]
}

func (m *Manager) IsConversationForkEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.coreEnabled["conversation_fork"]
}

func (m *Manager) IsModelEnabled(providerID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.disabledModels[providerID]
}

func (m *Manager) IsCoreToolEnabled(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.coreEnabled[name]
}

func (m *Manager) List() []UnifiedPlugin                      { return m.Catalog() }
func (m *Manager) Toggle(pluginID string, enabled bool) error { return m.SetEnabled(pluginID, enabled) }

func (m *Manager) Catalog() []UnifiedPlugin {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var list []UnifiedPlugin

	for name, tool := range m.coreTools {
		status := StatusEnabled
		if !m.coreEnabled[name] {
			status = StatusDisabled
		}
		displayName := m.corePluginName[name]
		if displayName == "" {
			displayName = tool.Definition.Function.Name
		}
		var metadata map[string]string
		if getMetadata := m.corePluginInfo[name]; getMetadata != nil {
			metadata = getMetadata()
		}
		list = append(list, UnifiedPlugin{
			ID:           "core:" + name,
			Name:         displayName,
			Type:         TypeCore,
			Description:  tool.Definition.Function.Description,
			Status:       status,
			Capabilities: []string{"tool:" + tool.Definition.Function.Name},
			Metadata:     metadata,
		})
	}

	for _, s := range m.skillsRegistry.List() {
		status := StatusEnabled
		if !s.Enabled {
			status = StatusDisabled
		}
		list = append(list, UnifiedPlugin{
			ID:           "skill:" + s.ID,
			Name:         s.Name,
			Type:         TypeSkill,
			Description:  s.Description,
			Status:       status,
			Capabilities: append([]string{"prompt_injection"}, s.Tags...),
			Metadata:     map[string]string{"path": s.FilePath},
		})
	}

	for _, cfg := range m.mcpManager.ListConfigs() {
		status := StatusEnabled
		if !cfg.Enabled {
			status = StatusDisabled
		}
		runtimeError := m.mcpManager.RuntimeError(cfg.ID)
		if runtimeError != "" {
			status = StatusError
		}
		list = append(list, UnifiedPlugin{
			ID:          "mcp:" + cfg.ID,
			Name:        cfg.Name,
			Type:        TypeMCP,
			Description: cfg.Description,
			Status:      status,
			Error:       runtimeError,
			Metadata:    map[string]string{"command": cfg.Command},
		})
	}

	compactorStatus := StatusEnabled
	if !m.coreEnabled["context_compactor"] {
		compactorStatus = StatusDisabled
	}
	list = append(list, UnifiedPlugin{
		ID:           "core:context_compactor",
		Name:         "智能上下文压缩提炼器 (Context Compactor)",
		Type:         TypeCore,
		Description:  "在模型请求前统一管理跨轮历史与轮内工具上下文；auto 模式在 75% 触发上限内优先使用 OpenAI Responses 原生 compact，并完整往返 opaque item；语义模式用持久来源和原文摘录校验摘要，extractive 模式不调用摘要模型。近期完整工具交互会跨轮恢复。摘要作为低信任历史资料注入；原生状态不兼容时从原始来源重建，来源不可读则明确终止。关闭策略后仍保留 host 硬窗口保护。",
		Status:       compactorStatus,
		Capabilities: []string{"context_compaction", "adaptive_budget", "memory_distillation", "provider_native_compaction"},
		Metadata: map[string]string{
			"mode":                               m.contextCompaction.Mode,
			"triggerPercent":                     fmt.Sprintf("%d", m.contextCompaction.TriggerPercent),
			"minimumGrowthBeforeRecompactTokens": fmt.Sprintf("%d", m.contextCompaction.MinimumGrowthBeforeRecompactTokens),
			"recentContextTokens":                fmt.Sprintf("%d", m.contextCompaction.RecentContextTokens),
			"compactionCallBudget":               fmt.Sprintf("%d", m.contextCompaction.CompactionCallBudget),
		},
	})

	runInspectorStatus := StatusEnabled
	if !m.coreEnabled["run_inspector"] {
		runInspectorStatus = StatusDisabled
	}
	list = append(list, UnifiedPlugin{
		ID:          "core:run_inspector",
		Name:        "运行详情 (Run Inspector)",
		Type:        TypeCore,
		Description: "持久化可折叠的命令、工作目录、标准输出、错误输出、退出码及工具反馈；用量统计由回答本身展示。",
		Status:      runInspectorStatus,
		Capabilities: []string{
			"tool_command_trace", "tool_output_trace", "run_details_disclosure",
		},
	})

	modelSelectorStatus := StatusEnabled
	if !m.coreEnabled["model_selector"] {
		modelSelectorStatus = StatusDisabled
	}
	list = append(list, UnifiedPlugin{
		ID:           "core:model_selector",
		Name:         "模型切换器 (Model Switcher)",
		Type:         TypeCore,
		Description:  "在输入框底栏提供模型选择与动态切换能力。停用后底栏隐藏模型选择浮层，使用系统默认模型。",
		Status:       modelSelectorStatus,
		Capabilities: []string{"model_selection", "composer_action"},
	})

	convTitleStatus := StatusEnabled
	if !m.coreEnabled["conversation_title"] {
		convTitleStatus = StatusDisabled
	}
	list = append(list, UnifiedPlugin{
		ID:           "core:conversation_title",
		Name:         "智能会话标题 (Smart Conversation Title)",
		Type:         TypeCore,
		Description:  "智能分析对话上下文，自动调用模型提炼规范清晰的会话标题，支持在对话框左上角点击即时编辑与保存。",
		Status:       convTitleStatus,
		Capabilities: []string{"title_generation", "title_editing"},
	})

	projStatus := StatusEnabled
	if !m.coreEnabled["project_workspace"] {
		projStatus = StatusDisabled
	}
	list = append(list, UnifiedPlugin{
		ID:           "core:project_workspace",
		Name:         "项目文件夹与工作区 (Project Folders & Workspace)",
		Type:         TypeCore,
		Description:  "支持在左侧侧边栏以项目文件夹组织多个对话，支持对话在文件夹间移入与移出，并在项目内对话间共享全局设定指令（Project Instructions）与工作区上下文。",
		Status:       projStatus,
		Capabilities: []string{"project_folders", "shared_context", "project_workspace"},
	})

	forkStatus := StatusEnabled
	if !m.coreEnabled["conversation_fork"] {
		forkStatus = StatusDisabled
	}
	list = append(list, UnifiedPlugin{
		ID:           "core:conversation_fork",
		Name:         "对话分支 (Conversation Fork)",
		Type:         TypeCore,
		Description:  "在已完成的回答下创建一条包含该回答上下文的独立会话；创建分支不会自动提交新消息或启动模型。",
		Status:       forkStatus,
		Capabilities: []string{"assistant_message_action", "conversation_fork"},
	})

	return list
}

func (m *Manager) SetEnabled(pluginID string, enabled bool) error {
	parts := strings.SplitN(pluginID, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid plugin ID format, expected prefix:name")
	}
	prefix := parts[0]
	name := parts[1]

	switch prefix {
	case "core":
		m.mu.Lock()
		defer m.mu.Unlock()
		if name == "model_selector" || name == "conversation_title" || name == "context_compactor" || name == "project_workspace" || name == "run_inspector" || name == "conversation_fork" {
			previous := m.coreEnabled[name]
			m.coreEnabled[name] = enabled
			if err := m.persistStateLocked(); err != nil {
				m.coreEnabled[name] = previous
				return fmt.Errorf("persist core plugin %q: %w", name, err)
			}
			return nil
		}
		if _, ok := m.coreTools[name]; !ok {
			return fmt.Errorf("core tool %q not found", name)
		}
		previous := m.coreEnabled[name]
		m.coreEnabled[name] = enabled
		if err := m.persistStateLocked(); err != nil {
			m.coreEnabled[name] = previous
			return fmt.Errorf("persist core tool %q: %w", name, err)
		}
		return nil
	case "skill":
		return m.skillsRegistry.SetEnabled(name, enabled)
	case "mcp":
		return m.mcpManager.SetEnabled(name, enabled)
	case "model":
		m.mu.Lock()
		defer m.mu.Unlock()
		previous := m.disabledModels[name]
		if enabled {
			delete(m.disabledModels, name)
		} else {
			m.disabledModels[name] = true
		}
		if err := m.persistStateLocked(); err != nil {
			if previous {
				m.disabledModels[name] = true
			} else {
				delete(m.disabledModels, name)
			}
			return fmt.Errorf("persist model plugin %q: %w", name, err)
		}
		return nil
	case "builtin":
		return fmt.Errorf("builtin plugin %q is internal and cannot be toggled directly", name)
	default:
		return fmt.Errorf("unsupported plugin type %q", prefix)
	}
}

func (m *Manager) loadState() error {
	raw, err := os.ReadFile(m.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read plugin state: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode plugin state: %w", err)
	}
	needsShellToolMigration := state.Version < 1
	needsCompactionSettingsMigration := state.Version < 3
	for name, enabled := range state.CoreEnabled {
		if _, known := m.coreEnabled[name]; known {
			if needsShellToolMigration && (name == "exec_command" || name == "exec_script") {
				continue
			}
			m.coreEnabled[name] = enabled
		}
	}
	if needsShellToolMigration {
		for _, name := range []string{"exec_command", "exec_script"} {
			if _, known := m.coreEnabled[name]; known {
				m.coreEnabled[name] = true
			}
		}
	}
	for providerID, disabled := range state.DisabledModels {
		if disabled {
			m.disabledModels[providerID] = true
		}
	}
	if !needsCompactionSettingsMigration {
		settings := state.ContextCompaction
		settings.Version = 2
		if err := validateCompactionSettings(settings); err != nil {
			return fmt.Errorf("decode persisted context compaction settings: %w", err)
		}
		m.contextCompaction = settings
	} else if state.Version >= 2 {
		settings := state.ContextCompaction
		defaults := defaultCompactionSettings()
		if settings.Mode == "" {
			settings.Mode = defaults.Mode
		}
		if settings.TriggerPercent == 0 {
			settings.TriggerPercent = defaults.TriggerPercent
		}
		if settings.MinimumGrowthBeforeRecompactTokens == 0 {
			settings.MinimumGrowthBeforeRecompactTokens = defaults.MinimumGrowthBeforeRecompactTokens
		}
		if settings.RecentContextTokens == 0 {
			settings.RecentContextTokens = defaults.RecentContextTokens
		}
		if settings.CompactionCallBudget == 0 {
			settings.CompactionCallBudget = defaults.CompactionCallBudget
		}
		settings.Version = defaults.Version
		if err := validateCompactionSettings(settings); err != nil {
			return fmt.Errorf("migrate persisted context compaction settings: %w", err)
		}
		m.contextCompaction = settings
	}
	if needsShellToolMigration || needsCompactionSettingsMigration {
		if err := m.persistStateLocked(); err != nil {
			return fmt.Errorf("persist plugin state migration: %w", err)
		}
	}
	return nil
}

func (m *Manager) persistStateLocked() error {
	state := persistedState{
		Version:           persistedStateVersion,
		CoreEnabled:       make(map[string]bool, len(m.coreEnabled)),
		DisabledModels:    make(map[string]bool, len(m.disabledModels)),
		ContextCompaction: m.contextCompaction,
	}
	for name, enabled := range m.coreEnabled {
		state.CoreEnabled[name] = enabled
	}
	for providerID, disabled := range m.disabledModels {
		state.DisabledModels[providerID] = disabled
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.statePath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(m.statePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	readBack, err := os.ReadFile(m.statePath)
	if err != nil {
		return err
	}
	if string(readBack) != string(raw) {
		return fmt.Errorf("state read-back mismatch")
	}
	return nil
}

func (m *Manager) Reload() error {
	return m.skillsRegistry.Reload()
}

func (m *Manager) ActiveTools() []provider.ToolDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var tools []provider.ToolDefinition
	for name, tool := range m.coreTools {
		if m.coreEnabled[name] {
			tools = append(tools, tool.Definition)
		}
	}

	mcpTools := m.mcpManager.AllActiveTools()
	tools = append(tools, mcpTools...)

	return tools
}

func (m *Manager) ExecuteTool(ctx context.Context, toolName string, args json.RawMessage) (any, bool, error) {
	m.mu.RLock()
	tool, ok := m.coreTools[toolName]
	enabled := m.coreEnabled[toolName]
	m.mu.RUnlock()

	if ok {
		if !enabled {
			return nil, true, fmt.Errorf("core tool %q is currently disabled", toolName)
		}
		res, err := tool.Handler(ctx, args)
		return res, true, err
	}

	return m.mcpManager.ExecuteTool(ctx, toolName, args)
}

func (m *Manager) IsConversationTitleEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.coreEnabled["conversation_title"]
}
