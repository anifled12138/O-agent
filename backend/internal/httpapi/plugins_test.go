package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/mcp"
	"axiom.local/agent/internal/plugins"
)

func setupTestServer(t *testing.T) (*Server, *plugins.Manager, string) {
	t.Helper()
	tempDir := t.TempDir()
	skillDir := filepath.Join(tempDir, ".skills", "test-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create test skill dir: %v", err)
	}

	skillContent := `---
name: HTTP Test Skill
description: Skill for testing HTTP API
tags: ["http", "test"]
---
Instructions for HTTP Test Skill
`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	pm := plugins.NewManager(tempDir)
	ag := &agent.Service{}
	ag.SetPlugins(pm)

	srv := &Server{
		agent: ag,
	}
	return srv, pm, tempDir
}

func TestUnifiedPluginList(t *testing.T) {
	srv, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/plugins", nil)
	w := httptest.NewRecorder()
	srv.unifiedPluginList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var list []plugins.UnifiedPlugin
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("failed to unmarshal plugins: %v", err)
	}

	if len(list) == 0 {
		t.Fatalf("expected plugins list to have items, got 0")
	}

	// Verify core tools are present
	foundCore := false
	foundSkill := false
	for _, p := range list {
		if p.Type == plugins.TypeCore && p.ID == "core:grep_search" {
			foundCore = true
		}
		if p.Type == plugins.TypeSkill && p.Name == "HTTP Test Skill" {
			foundSkill = true
		}
	}

	if !foundCore {
		t.Errorf("expected to find core:grep_search in list")
	}
	if !foundSkill {
		t.Errorf("expected to find HTTP Test Skill in list")
	}

	foundConvTitle := false
	foundRunInspector := false
	for _, p := range list {
		if p.ID == "core:conversation_title" {
			foundConvTitle = true
			if p.Type != plugins.TypeCore {
				t.Errorf("expected core:conversation_title to be TypeCore, got %s", p.Type)
			}
			break
		}
		if p.ID == "core:run_inspector" {
			foundRunInspector = true
			if p.Type != plugins.TypeCore || p.Status != plugins.StatusEnabled {
				t.Errorf("expected enabled core run inspector, got type=%s status=%s", p.Type, p.Status)
			}
		}
	}
	if !foundConvTitle {
		t.Errorf("expected to find core:conversation_title in list")
	}
	if !foundRunInspector {
		t.Errorf("expected to find core:run_inspector in list")
	}
}

func TestUnifiedPluginToggle(t *testing.T) {
	srv, pm, _ := setupTestServer(t)

	// Toggle core:grep_search off
	body, _ := json.Marshal(map[string]bool{"enabled": false})
	req := httptest.NewRequest("POST", "/api/v1/plugins/core:grep_search/toggle", bytes.NewReader(body))
	req.SetPathValue("id", "core:grep_search")
	w := httptest.NewRecorder()
	srv.unifiedPluginToggle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Check catalog
	for _, p := range pm.Catalog() {
		if p.ID == "core:grep_search" {
			if p.Status != plugins.StatusDisabled {
				t.Fatalf("expected core:grep_search to be disabled, got %s", p.Status)
			}
		}
	}

	// Toggle back on
	body, _ = json.Marshal(map[string]bool{"enabled": true})
	req = httptest.NewRequest("POST", "/api/v1/plugins/core:grep_search/toggle", bytes.NewReader(body))
	req.SetPathValue("id", "core:grep_search")
	w = httptest.NewRecorder()
	srv.unifiedPluginToggle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Toggle conversation_title off and on
	body, _ = json.Marshal(map[string]bool{"enabled": false})
	req = httptest.NewRequest("POST", "/api/v1/plugins/core:conversation_title/toggle", bytes.NewReader(body))
	req.SetPathValue("id", "core:conversation_title")
	w = httptest.NewRecorder()
	srv.unifiedPluginToggle(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if pm.IsConversationTitleEnabled() {
		t.Fatalf("expected conversation_title to be disabled")
	}

	// The run inspector is a real execution-path plugin, not a presentation-only flag.
	body, _ = json.Marshal(map[string]bool{"enabled": false})
	req = httptest.NewRequest("POST", "/api/v1/plugins/core:run_inspector/toggle", bytes.NewReader(body))
	req.SetPathValue("id", "core:run_inspector")
	w = httptest.NewRecorder()
	srv.unifiedPluginToggle(w, req)
	if w.Code != http.StatusOK || pm.IsRunInspectorEnabled() {
		t.Fatalf("expected run inspector to be durably disabled, status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestUnifiedPluginReload(t *testing.T) {
	srv, _, tempDir := setupTestServer(t)

	// Add a new skill on disk
	newSkillDir := filepath.Join(tempDir, ".skills", "skill-2")
	_ = os.MkdirAll(newSkillDir, 0755)
	skillContent := `---
name: Skill Two
description: Desc
---
Prompt`
	_ = os.WriteFile(filepath.Join(newSkillDir, "SKILL.md"), []byte(skillContent), 0644)

	req := httptest.NewRequest("POST", "/api/v1/plugins/reload", nil)
	w := httptest.NewRecorder()
	srv.unifiedPluginReload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var list []plugins.UnifiedPlugin
	_ = json.Unmarshal(w.Body.Bytes(), &list)

	foundNew := false
	for _, p := range list {
		if p.Name == "Skill Two" {
			foundNew = true
			break
		}
	}
	if !foundNew {
		t.Errorf("expected newly reloaded skill to appear in catalog")
	}
}

func TestUnifiedPluginMCPManagement(t *testing.T) {
	srv, pm, _ := setupTestServer(t)

	cfg := mcp.ServerConfig{
		ID:          "test-mcp-server",
		Name:        "Test MCP Server",
		Description: "MCP for testing HTTP endpoints",
		Command:     "node",
		Args:        []string{"server.js"},
		Enabled:     false,
	}

	body, _ := json.Marshal(cfg)
	req := httptest.NewRequest("POST", "/api/v1/plugins/mcp", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.unifiedPluginAddMCP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from add MCP, got %d: %s", w.Code, w.Body.String())
	}

	// Verify it is registered
	foundMCP := false
	for _, p := range pm.Catalog() {
		if p.ID == "mcp:test-mcp-server" {
			foundMCP = true
			if p.Name != "Test MCP Server" {
				t.Errorf("expected name 'Test MCP Server', got %q", p.Name)
			}
		}
	}
	if !foundMCP {
		t.Fatalf("expected mcp:test-mcp-server in catalog")
	}

	// Remove MCP
	delReq := httptest.NewRequest("DELETE", "/api/v1/plugins/mcp/test-mcp-server", nil)
	delReq.SetPathValue("id", "test-mcp-server")
	delW := httptest.NewRecorder()
	srv.unifiedPluginRemoveMCP(delW, delReq)

	if delW.Code != http.StatusOK {
		t.Fatalf("expected 200 from remove MCP, got %d: %s", delW.Code, delW.Body.String())
	}

	// Verify removal
	for _, p := range pm.Catalog() {
		if p.ID == "mcp:test-mcp-server" {
			t.Fatalf("expected mcp:test-mcp-server to be removed")
		}
	}
}
