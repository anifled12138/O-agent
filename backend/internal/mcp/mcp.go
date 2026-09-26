package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/provider"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// ServerConfig defines the launch configuration for an external MCP server
type ServerConfig struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Enabled     bool              `json:"enabled"`
}

// Client wraps the official/community mark3labs MCPClient
type Client struct {
	config    ServerConfig
	mcpClient client.MCPClient
	tools     []provider.ToolDefinition
	mu        sync.Mutex
	closed    bool
}

// StartClient initializes and connects the MCP client (stdio or SSE)
func StartClient(cfg ServerConfig) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var mcpCli client.MCPClient

	if cfg.URL != "" {
		sseCli, err := client.NewSSEMCPClient(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("failed to create SSE MCP client: %w", err)
		}
		if err := sseCli.Start(ctx); err != nil {
			return nil, fmt.Errorf("failed to start SSE MCP client: %w", err)
		}
		mcpCli = sseCli
	} else {
		if cfg.Command == "" {
			return nil, fmt.Errorf("command cannot be empty")
		}

		var envList []string
		if len(cfg.Env) > 0 {
			envList = os.Environ()
			for k, v := range cfg.Env {
				envList = append(envList, fmt.Sprintf("%s=%s", k, v))
			}
		}

		stdioCli, err := client.NewStdioMCPClient(cfg.Command, envList, cfg.Args...)
		if err != nil {
			return nil, fmt.Errorf("failed to launch stdio MCP process: %w", err)
		}
		mcpCli = stdioCli
	}

	c := &Client{
		config:    cfg,
		mcpClient: mcpCli,
	}

	// Initialize handshake
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "axiom-agent",
		Version: "1.0.0",
	}

	_, err := mcpCli.Initialize(ctx, initReq)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("MCP handshake failed: %w", err)
	}

	// Discover tools
	if err := c.refreshTools(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("failed to list MCP tools: %w", err)
	}

	return c, nil
}

func (c *Client) refreshTools(ctx context.Context) error {
	res, err := c.mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var toolDefs []provider.ToolDefinition
	for _, t := range res.Tools {
		namespacedName := fmt.Sprintf("%s__%s", c.config.ID, t.Name)

		rawSchema, err := json.Marshal(t.InputSchema)
		if err != nil {
			rawSchema = []byte(`{"type":"object"}`)
		}

		td := provider.ToolDefinition{Type: "function"}
		td.Function.Name = namespacedName
		td.Function.Description = fmt.Sprintf("[%s] %s", c.config.Name, t.Description)
		td.Function.Parameters = json.RawMessage(rawSchema)
		toolDefs = append(toolDefs, td)
	}
	c.tools = toolDefs
	return nil
}

func (c *Client) CallTool(ctx context.Context, toolName string, args map[string]any) (string, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", fmt.Errorf("client is closed")
	}
	c.mu.Unlock()

	prefix := c.config.ID + "__"
	serverToolName := strings.TrimPrefix(toolName, prefix)

	callReq := mcp.CallToolRequest{}
	callReq.Params.Name = serverToolName
	callReq.Params.Arguments = args

	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := c.mcpClient.CallTool(callCtx, callReq)
	if err != nil {
		return "", fmt.Errorf("MCP call failed: %w", err)
	}

	if result.IsError {
		var errMsgs []string
		for _, content := range result.Content {
			if txt, ok := mcp.AsTextContent(content); ok {
				errMsgs = append(errMsgs, txt.Text)
			}
		}
		if len(errMsgs) > 0 {
			return "", fmt.Errorf("MCP tool error: %s", strings.Join(errMsgs, "\n"))
		}
		return "", fmt.Errorf("MCP tool reported execution failure")
	}

	var texts []string
	for _, content := range result.Content {
		if txt, ok := mcp.AsTextContent(content); ok {
			texts = append(texts, txt.Text)
		} else {
			raw, _ := json.Marshal(content)
			texts = append(texts, string(raw))
		}
	}

	if len(texts) == 0 {
		return "Success (no output)", nil
	}
	return strings.Join(texts, "\n"), nil
}

func (c *Client) Tools() []provider.ToolDefinition {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]provider.ToolDefinition(nil), c.tools...)
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.mcpClient.Close()
}

// Manager orchestrates multiple external MCP servers with persistent config
type Manager struct {
	workspaceRoot string
	configFile    string
	clients       map[string]*Client
	configs       map[string]ServerConfig
	runtimeErrors map[string]string
	mu            sync.RWMutex
}

type mcpStoreFile struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

func NewManager() *Manager {
	return NewManagerWithWorkspace("")
}

func NewManagerWithWorkspace(workspaceRoot string) *Manager {
	var cfgFile string
	if workspaceRoot != "" {
		cfgFile = filepath.Join(workspaceRoot, ".axiom", "mcp.json")
	}
	m := &Manager{
		workspaceRoot: workspaceRoot,
		configFile:    cfgFile,
		clients:       make(map[string]*Client),
		configs:       make(map[string]ServerConfig),
		runtimeErrors: make(map[string]string),
	}
	m.loadConfig()
	return m
}

func (m *Manager) loadConfig() {
	if m.configFile == "" {
		return
	}
	data, err := os.ReadFile(m.configFile)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("read MCP configuration", "path", m.configFile, "error", err)
		}
		return
	}
	var sf mcpStoreFile
	if err := json.Unmarshal(data, &sf); err != nil {
		slog.Error("parse MCP configuration", "path", m.configFile, "error", err)
		return
	}

	for id, cfg := range sf.MCPServers {
		if cfg.ID == "" {
			cfg.ID = id
		}
		m.configs[id] = cfg
		if cfg.Enabled {
			cli, err := StartClient(cfg)
			if err == nil {
				m.clients[id] = cli
			} else {
				m.runtimeErrors[id] = err.Error()
				slog.Error("restore MCP server", "server_id", id, "error", err)
			}
		}
	}
}

func (m *Manager) saveConfigLocked() error {
	if m.configFile == "" {
		return nil
	}
	dir := filepath.Dir(m.configFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	previous, readErr := os.ReadFile(m.configFile)
	previousExists := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) { return readErr }

	sf := mcpStoreFile{
		MCPServers: m.configs,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".mcp-config-*.tmp")
	if err != nil { return err }
	tmpName := tmp.Name()
	writeErr := tmp.Chmod(0600)
	if writeErr == nil { _, writeErr = tmp.Write(data) }
	if writeErr == nil { writeErr = tmp.Sync() }
	closeErr := tmp.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err = os.Rename(tmpName, m.configFile); err != nil {
		return errors.Join(err, os.Remove(tmpName))
	}
	readBack, readErr := os.ReadFile(m.configFile)
	if readErr == nil && bytes.Equal(readBack, data) { return nil }
	if readErr == nil { readErr = fmt.Errorf("MCP configuration read-back mismatch") }
	var restoreErr error
	if previousExists {
		restoreErr = writeMCPConfigAtomically(dir, m.configFile, previous)
	} else if removeErr := os.Remove(m.configFile); removeErr != nil && !os.IsNotExist(removeErr) {
		restoreErr = removeErr
	}
	return errors.Join(readErr, restoreErr)
}

func writeMCPConfigAtomically(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".mcp-restore-*.tmp")
	if err != nil { return err }
	tmpName := tmp.Name()
	writeErr := tmp.Chmod(0600)
	if writeErr == nil { _, writeErr = tmp.Write(data) }
	if writeErr == nil { writeErr = tmp.Sync() }
	closeErr := tmp.Close()
	if err = errors.Join(writeErr, closeErr); err != nil { _ = os.Remove(tmpName); return err }
	if err = os.Rename(tmpName, path); err != nil { return errors.Join(err, os.Remove(tmpName)) }
	readBack, err := os.ReadFile(path)
	if err != nil { return err }
	if !bytes.Equal(readBack, data) { return fmt.Errorf("restored MCP configuration read-back mismatch") }
	return nil
}

func (m *Manager) AddOrUpdateConfig(cfg ServerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cfg.ID == "" {
		return fmt.Errorf("server ID cannot be empty")
	}

	var nextClient *Client
	if cfg.Enabled {
		var err error
		nextClient, err = StartClient(cfg)
		if err != nil {
			return fmt.Errorf("failed to start MCP server '%s': %w", cfg.ID, err)
		}
	}

	previous, existed := m.configs[cfg.ID]
	m.configs[cfg.ID] = cfg
	if err := m.saveConfigLocked(); err != nil {
		if existed {
			m.configs[cfg.ID] = previous
		} else {
			delete(m.configs, cfg.ID)
		}
		cleanupErr := error(nil)
		if nextClient != nil {
			cleanupErr = nextClient.Close()
		}
		return errors.Join(fmt.Errorf("persist MCP server '%s' configuration: %w", cfg.ID, err), cleanupErr)
	}

	old := m.clients[cfg.ID]
	if cfg.Enabled {
		m.clients[cfg.ID] = nextClient
	} else {
		delete(m.clients, cfg.ID)
	}
	delete(m.runtimeErrors, cfg.ID)
	if old != nil {
		if err := old.Close(); err != nil {
			m.runtimeErrors[cfg.ID] = fmt.Sprintf("previous MCP client did not close cleanly: %v", err)
			return fmt.Errorf("close previous MCP server '%s': %w", cfg.ID, err)
		}
	}

	return nil
}

func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cli := m.clients[id]
	if cli != nil {
		if err := cli.Close(); err != nil {
			m.runtimeErrors[id] = fmt.Sprintf("removed MCP client did not close cleanly: %v", err)
			return fmt.Errorf("close removed MCP server '%s': %w", id, err)
		}
		delete(m.clients, id)
	}
	previous, existed := m.configs[id]
	delete(m.configs, id)
	if err := m.saveConfigLocked(); err != nil {
		if existed {
			m.configs[id] = previous
		}
		if cli != nil && existed && previous.Enabled {
			restarted, restartErr := StartClient(previous)
			if restartErr != nil {
				m.runtimeErrors[id] = fmt.Sprintf("restore MCP client after persistence failure: %v", restartErr)
				err = errors.Join(err, restartErr)
			} else {
				m.clients[id] = restarted
				delete(m.runtimeErrors, id)
			}
		}
		return fmt.Errorf("persist removal of MCP server '%s': %w", id, err)
	}
	delete(m.runtimeErrors, id)
	return nil
}

func (m *Manager) SetEnabled(id string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, exists := m.configs[id]
	if !exists {
		return fmt.Errorf("mcp server '%s' not found", id)
	}

	if cfg.Enabled == enabled && (!enabled || m.clients[id] != nil) {
		return nil
	}

	var nextClient *Client
	if enabled {
		cfg.Enabled = true
		cli, err := StartClient(cfg)
		if err != nil {
			return fmt.Errorf("failed to start MCP client '%s': %w", id, err)
		}
		nextClient = cli
	}

	previous := m.configs[id]
	cfg.Enabled = enabled
	m.configs[id] = cfg
	if err := m.saveConfigLocked(); err != nil {
		m.configs[id] = previous
		cleanupErr := error(nil)
		if nextClient != nil {
			cleanupErr = nextClient.Close()
		}
		return errors.Join(fmt.Errorf("persist MCP server '%s' state: %w", id, err), cleanupErr)
	}

	old := m.clients[id]
	if enabled {
		m.clients[id] = nextClient
		delete(m.runtimeErrors, id)
	} else {
		delete(m.clients, id)
		delete(m.runtimeErrors, id)
	}
	if old != nil {
		if err := old.Close(); err != nil {
			m.runtimeErrors[id] = fmt.Sprintf("previous MCP client did not close cleanly: %v", err)
			return fmt.Errorf("close previous MCP server '%s': %w", id, err)
		}
	}

	return nil
}

// RuntimeError reports the latest observed startup or shutdown failure for a server.
func (m *Manager) RuntimeError(id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.runtimeErrors[id]
}

func (m *Manager) ListConfigs() []ServerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []ServerConfig
	for _, cfg := range m.configs {
		result = append(result, cfg)
	}
	return result
}

func (m *Manager) ActiveTools() []provider.ToolDefinition {
	return m.AllActiveTools()
}

func (m *Manager) AllActiveTools() []provider.ToolDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var allTools []provider.ToolDefinition
	for _, cli := range m.clients {
		allTools = append(allTools, cli.Tools()...)
	}
	return allTools
}

func (m *Manager) ExecuteTool(ctx context.Context, fullName string, args json.RawMessage) (any, bool, error) {
	parts := strings.SplitN(fullName, "__", 2)
	if len(parts) != 2 {
		return nil, false, nil
	}
	serverID := parts[0]

	m.mu.RLock()
	cli, exists := m.clients[serverID]
	m.mu.RUnlock()

	if !exists {
		return nil, false, nil
	}

	var parsedArgs map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsedArgs); err != nil {
			return nil, true, fmt.Errorf("failed to parse tool arguments: %w", err)
		}
	}

	res, err := cli.CallTool(ctx, fullName, parsedArgs)
	return res, true, err
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, cli := range m.clients {
		_ = cli.Close()
		delete(m.clients, id)
	}
}
