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
	Capabilities []string          `json:"capabilities,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type Manager struct {
	mu             sync.RWMutex
	workspaceRoot  string
	skillsRegistry *skills.Registry
	mcpManager     *mcp.Manager
	coreTools      map[string]coretools.Tool
	coreEnabled    map[string]bool
	contextPlugin  *ContextManagerPlugin
	providerLister func(ctx context.Context) ([]domain.Provider, error)
	disabledModels map[string]bool
	statePath      string
}

type persistedState struct {
	CoreEnabled    map[string]bool `json:"coreEnabled"`
	DisabledModels map[string]bool `json:"disabledModels,omitempty"`
}

func NewManager(workspaceRoot string) *Manager {
	skillsReg := skills.NewRegistry(workspaceRoot)
	mcpMgr := mcp.NewManagerWithWorkspace(workspaceRoot)
	contextPlugin := NewDefaultContextManagerPlugin()

	tools := coretools.GetCoreTools(workspaceRoot)
	coreMap := make(map[string]coretools.Tool, len(tools))
	coreEnabled := make(map[string]bool, len(tools))
	for _, t := range tools {
		name := t.Definition.Function.Name
		coreMap[name] = t
		coreEnabled[name] = name != "exec_command" || os.Getenv("O_ENABLE_SHELL_TOOL") == "1"
	}
	coreEnabled["model_selector"] = true
	coreEnabled["context_compactor"] = true
	coreEnabled["conversation_title"] = true
	coreEnabled["project_workspace"] = true
	coreEnabled["run_inspector"] = true

	manager := &Manager{
		workspaceRoot:  workspaceRoot,
		skillsRegistry: skillsReg,
		mcpManager:     mcpMgr,
		coreTools:      coreMap,
		coreEnabled:    coreEnabled,
		contextPlugin:  contextPlugin,
		disabledModels: make(map[string]bool),
		statePath:      filepath.Join(workspaceRoot, ".axiom", "plugin-state.json"),
	}
	manager.loadState()
	return manager
}

func (m *Manager) SkillsRegistry() *skills.Registry {
	return m.skillsRegistry
}

func (m *Manager) MCPManager() *mcp.Manager {
	return m.mcpManager
}

func (m *Manager) ContextPlugin() *ContextManagerPlugin {
	return m.contextPlugin
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
		list = append(list, UnifiedPlugin{
			ID:           "core:" + name,
			Name:         tool.Definition.Function.Name,
			Type:         TypeCore,
			Description:  tool.Definition.Function.Description,
			Status:       status,
			Capabilities: []string{"tool:" + tool.Definition.Function.Name},
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
		list = append(list, UnifiedPlugin{
			ID:          "mcp:" + cfg.ID,
			Name:        cfg.Name,
			Type:        TypeMCP,
			Description: cfg.Description,
			Status:      status,
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
		Description:  "按模型或用户配置的上下文窗口管理预算；默认将约 55% 留给持久化对话，并为系统指令、工具、推理和输出预留余量。压缩是有损的，会优先保留当前目标和近期完整步骤。",
		Status:       compactorStatus,
		Capabilities: []string{"context_compaction", "adaptive_budget", "memory_distillation"},
	})

	runInspectorStatus := StatusEnabled
	if !m.coreEnabled["run_inspector"] {
		runInspectorStatus = StatusDisabled
	}
	list = append(list, UnifiedPlugin{
		ID:          "core:run_inspector",
		Name:        "运行详情与统计 (Run Inspector)",
		Type:        TypeCore,
		Description: "在每条回答顶部显示用时、Token 与运行步数，并持久化可折叠的命令、工作目录、标准输出、错误输出、退出码及工具反馈。",
		Status:      runInspectorStatus,
		Capabilities: []string{
			"run_metrics", "tool_command_trace", "tool_output_trace", "answer_header_surface",
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
		if name == "model_selector" || name == "conversation_title" || name == "context_compactor" || name == "project_workspace" || name == "run_inspector" {
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

func (m *Manager) loadState() {
	raw, err := os.ReadFile(m.statePath)
	if err != nil {
		return
	}
	var state persistedState
	if json.Unmarshal(raw, &state) != nil {
		return
	}
	for name, enabled := range state.CoreEnabled {
		if _, known := m.coreEnabled[name]; known {
			m.coreEnabled[name] = enabled
		}
	}
	for providerID, disabled := range state.DisabledModels {
		if disabled {
			m.disabledModels[providerID] = true
		}
	}
}

func (m *Manager) persistStateLocked() error {
	state := persistedState{
		CoreEnabled:    make(map[string]bool, len(m.coreEnabled)),
		DisabledModels: make(map[string]bool, len(m.disabledModels)),
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
