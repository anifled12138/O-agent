package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"axiom.local/agent/internal/coretools"
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
		coreEnabled[name] = true
	}

	return &Manager{
		workspaceRoot:  workspaceRoot,
		skillsRegistry: skillsReg,
		mcpManager:     mcpMgr,
		coreTools:      coreMap,
		coreEnabled:    coreEnabled,
		contextPlugin:  contextPlugin,
	}
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

func (m *Manager) List() []UnifiedPlugin { return m.Catalog() }
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

	list = append(list, UnifiedPlugin{
		ID:           m.contextPlugin.ID,
		Name:         m.contextPlugin.Name,
		Type:         TypeContext,
		Description:  "Dynamic context window guardian, sliding budget allocator and tool output sanitizer.",
		Status:       StatusEnabled,
		Capabilities: []string{"context_pruning", "tool_sanitization", "token_budgeting"},
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
		if _, ok := m.coreTools[name]; !ok {
			return fmt.Errorf("core tool %q not found", name)
		}
		m.coreEnabled[name] = enabled
		return nil
	case "skill":
		return m.skillsRegistry.SetEnabled(name, enabled)
	case "mcp":
		return m.mcpManager.SetEnabled(name, enabled)
	case "builtin":
		return nil
	default:
		return fmt.Errorf("unsupported plugin type %q", prefix)
	}
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
