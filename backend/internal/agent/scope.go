package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/provider"
)

type capabilityCandidate struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Summary    string   `json:"summary"`
	Tags       []string `json:"tags,omitempty"`
	Visibility string   `json:"visibility"`
	ReleaseID  string   `json:"releaseId,omitempty"`
	Score      int      `json:"-"`
}

type loadedTool struct {
	Capability pluginforge.CapabilityBinding
	Definition provider.ToolDefinition
}

type turnScope struct {
	owner        *Service
	userID       string
	lease        pluginforge.TurnLease
	tools        map[string]pluginforge.CapabilityBinding
	skills       map[string]pluginforge.SkillBinding
	creator      map[string]provider.ToolDefinition
	loaded       map[string]loadedTool
	loadedByID   map[string]string
	loadedCreate map[string]provider.ToolDefinition
}

func newTurnScope(owner *Service, userID string) *turnScope {
	lease := owner.forge.BeginTurn(userID)
	scope := &turnScope{owner: owner, userID: userID, lease: lease, tools: map[string]pluginforge.CapabilityBinding{}, skills: map[string]pluginforge.SkillBinding{}, creator: creatorTools(), loaded: map[string]loadedTool{}, loadedByID: map[string]string{}, loadedCreate: map[string]provider.ToolDefinition{}}
	for _, binding := range lease.Capabilities() {
		scope.tools[binding.ID] = binding
		if binding.Visibility == "always" {
			scope.loadPluginTool(binding)
		}
	}
	for _, binding := range lease.Skills() {
		scope.skills[binding.Skill.ID] = binding
	}
	return scope
}

func (s *turnScope) Close() { s.lease.Close() }

func (s *turnScope) definitions() []provider.ToolDefinition {
	result := []provider.ToolDefinition{
		tool("axiom_capability_search", "Search the compact capability index for relevant installed Agent tools, lazy skills, or creator actions. Results contain summaries only; call axiom_capability_load before use.", `{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"additionalProperties":false}`),
		tool("axiom_capability_load", "Load one exact capability from search results. Agent tools become callable with their exact schema; skills return their instructions lazily.", `{"type":"object","required":["capabilityId"],"properties":{"capabilityId":{"type":"string"}},"additionalProperties":false}`),
	}
	for _, item := range s.loaded {
		result = append(result, item.Definition)
	}
	for _, item := range s.loadedCreate {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Function.Name < result[j].Function.Name })
	return result
}

func (s *turnScope) execute(ctx context.Context, name string, arguments json.RawMessage) json.RawMessage {
	switch name {
	case "axiom_capability_search":
		var input struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if json.Unmarshal(arguments, &input) != nil {
			return toolError("invalid capability search arguments")
		}
		return toolOK(s.search(input.Query, input.Limit))
	case "axiom_capability_load":
		var input struct {
			CapabilityID string `json:"capabilityId"`
		}
		if json.Unmarshal(arguments, &input) != nil {
			return toolError("invalid capability load arguments")
		}
		value, err := s.load(input.CapabilityID)
		if err != nil {
			return toolError(err.Error())
		}
		return toolOK(value)
	}
	if item, ok := s.loaded[name]; ok {
		result, err := s.lease.Invoke(ctx, item.Capability.ID, arguments)
		if err != nil {
			return toolError(err.Error())
		}
		var value any
		if json.Unmarshal(result, &value) != nil {
			value = json.RawMessage(result)
		}
		return toolOK(value)
	}
	if _, ok := s.loadedCreate[name]; ok {
		return s.owner.runCreatorTool(ctx, s.userID, name, arguments)
	}
	return toolError(fmt.Sprintf("tool %q is not loaded in this turn", name))
}

func (s *turnScope) search(query string, limit int) []capabilityCandidate {
	if limit < 1 || limit > 20 {
		limit = 8
	}
	terms := searchTerms(query)
	result := make([]capabilityCandidate, 0)
	for _, item := range s.tools {
		if item.Visibility == "none" {
			continue
		}
		candidate := capabilityCandidate{ID: item.ID, Kind: "tool", Summary: item.Summary, Tags: item.Tags, Visibility: item.Visibility, ReleaseID: item.ReleaseID}
		candidate.Score = relevance(terms, item.ID+" "+item.Summary+" "+strings.Join(item.Tags, " "))
		if candidate.Score > 0 || len(terms) == 0 {
			result = append(result, candidate)
		}
	}
	for _, item := range s.skills {
		if item.Skill.Visibility == "none" {
			continue
		}
		candidate := capabilityCandidate{ID: item.Skill.ID, Kind: "skill", Summary: item.Skill.Summary, Visibility: item.Skill.Visibility, ReleaseID: item.ReleaseID}
		candidate.Score = relevance(terms, item.Skill.ID+" "+item.Skill.Summary)
		if candidate.Score > 0 || len(terms) == 0 {
			result = append(result, candidate)
		}
	}
	for id, definition := range s.creator {
		candidate := capabilityCandidate{ID: id, Kind: "creator-action", Summary: definition.Function.Description, Visibility: "creator-only"}
		candidate.Score = relevance(terms, id+" "+definition.Function.Description+" plugin create build generate install source revision rollback")
		if candidate.Score > 0 {
			result = append(result, candidate)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Score == result[j].Score {
			return result[i].ID < result[j].ID
		}
		return result[i].Score > result[j].Score
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func (s *turnScope) load(id string) (any, error) {
	if binding, ok := s.tools[id]; ok && binding.Visibility != "none" {
		name := s.loadPluginTool(binding)
		return map[string]any{"id": id, "kind": "tool", "functionName": name, "releaseId": binding.ReleaseID, "inputSchema": binding.InputSchema, "outputSchema": binding.OutputSchema, "risk": binding.Risk}, nil
	}
	if binding, ok := s.skills[id]; ok && binding.Skill.Visibility != "none" {
		content, err := s.owner.forge.LoadPinnedSkill(binding)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": id, "kind": "skill", "releaseId": binding.ReleaseID, "instructions": content}, nil
	}
	if definition, ok := s.creator[id]; ok {
		s.loadedCreate[id] = definition
		return map[string]any{"id": id, "kind": "creator-action", "functionName": id, "inputSchema": definition.Function.Parameters, "approvalBoundary": "The Agent may prepare releases; only the user can approve grants."}, nil
	}
	return nil, fmt.Errorf("capability %q is not present in this turn snapshot", id)
}

func (s *turnScope) loadPluginTool(binding pluginforge.CapabilityBinding) string {
	if existing := s.loadedByID[binding.ID]; existing != "" {
		return existing
	}
	digest := sha256.Sum256([]byte(binding.ReleaseID + "\x00" + binding.ID))
	name := "axiom_dynamic_" + hex.EncodeToString(digest[:8])
	definition := provider.ToolDefinition{Type: "function"}
	definition.Function.Name = name
	definition.Function.Description = binding.Summary + " [pinned " + binding.Version + "; risk: " + binding.Risk + "]"
	definition.Function.Parameters = append(json.RawMessage(nil), binding.InputSchema...)
	s.loaded[name] = loadedTool{Capability: binding, Definition: definition}
	s.loadedByID[binding.ID] = name
	return name
}

func searchTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= '\u4e00' && r <= '\u9fff')
	})
	seen := map[string]bool{}
	result := []string{}
	for _, field := range fields {
		if len(field) > 1 && !seen[field] {
			seen[field] = true
			result = append(result, field)
		}
	}
	return result
}

func relevance(terms []string, text string) int {
	text = strings.ToLower(text)
	score := 0
	for _, term := range terms {
		if strings.Contains(text, term) {
			score += 10
		}
		if strings.HasPrefix(text, term) {
			score += 4
		}
	}
	return score
}

func toolOK(value any) json.RawMessage {
	raw, err := json.Marshal(map[string]any{"ok": true, "result": value})
	if err != nil {
		return toolError(err.Error())
	}
	return raw
}

func creatorTools() map[string]provider.ToolDefinition {
	items := []provider.ToolDefinition{
		tool("axiom_plugin_projects", "List Plugin Forge projects and lifecycle state.", `{"type":"object","properties":{},"additionalProperties":false}`),
		tool("axiom_plugin_propose", "Create a plugin proposal for a missing durable capability. This does not generate or install anything.", `{"type":"object","required":["name","description"],"properties":{"name":{"type":"string"},"description":{"type":"string"},"shape":{"type":"string","enum":["hybrid","agent-tool","ui","service","skill"]}},"additionalProperties":false}`),
		tool("axiom_plugin_generate", "Generate source for a proposed plugin project in its isolated Git repository.", `{"type":"object","required":["projectId"],"properties":{"projectId":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_write_source", "Create or replace a text source file inside a generated plugin Git workspace.", `{"type":"object","required":["projectId","path","content"],"properties":{"projectId":{"type":"string"},"path":{"type":"string"},"content":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_begin_revision", "Begin an update while the active release keeps serving pinned turns.", `{"type":"object","required":["projectId"],"properties":{"projectId":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_build", "Build and verify declared plugin surfaces into an immutable release.", `{"type":"object","required":["projectId"],"properties":{"projectId":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_request_approval", "Move a tested release to user permission review; this never grants approval.", `{"type":"object","required":["projectId"],"properties":{"projectId":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_install", "Activate only an already user-approved immutable plugin release.", `{"type":"object","required":["projectId"],"properties":{"projectId":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_rollback", "Atomically roll an active plugin back to an approved release.", `{"type":"object","required":["projectId","releaseId"],"properties":{"projectId":{"type":"string"},"releaseId":{"type":"string"}},"additionalProperties":false}`),
	}
	result := map[string]provider.ToolDefinition{}
	for _, item := range items {
		result[item.Function.Name] = item
	}
	return result
}
