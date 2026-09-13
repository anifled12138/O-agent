package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"axiom.local/agent/internal/capability"
	"axiom.local/agent/internal/capsule"
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
	owner          *Service
	userID         string
	conversationID string
	turnID         string
	evaluation     bool
	lease          pluginforge.TurnLease
	tools          map[string]pluginforge.CapabilityBinding
	skills         map[string]pluginforge.SkillBinding
	creator        map[string]provider.ToolDefinition
	loaded         map[string]loadedTool
	loadedByID     map[string]string
	loadedCreate   map[string]provider.ToolDefinition
	loadedCapsules map[string]capsule.Manifest
}

func newTurnScope(owner *Service, userID, conversationID, turnID string) *turnScope {
	return newScopedTurn(owner, userID, conversationID, turnID, false)
}

func newEvaluationScope(owner *Service, userID string) *turnScope {
	return newScopedTurn(owner, userID, "", "", true)
}

func newScopedTurn(owner *Service, userID, conversationID, turnID string, evaluation bool) *turnScope {
	lease := owner.forge.BeginTurn(userID)
	creator := creatorTools()
	if evaluation {
		creator = map[string]provider.ToolDefinition{}
	}
	scope := &turnScope{owner: owner, userID: userID, conversationID: conversationID, turnID: turnID, evaluation: evaluation, lease: lease, tools: map[string]pluginforge.CapabilityBinding{}, skills: map[string]pluginforge.SkillBinding{}, creator: creator, loaded: map[string]loadedTool{}, loadedByID: map[string]string{}, loadedCreate: map[string]provider.ToolDefinition{}, loadedCapsules: map[string]capsule.Manifest{}}
	for _, binding := range lease.Capabilities() {
		if evaluation && binding.Risk != "workspace-readonly" {
			continue
		}
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

func (s *turnScope) Close() {
	if s.turnID != "" && s.owner != nil && s.owner.fragments != nil {
		s.owner.fragments.DropTurn(s.userID, s.turnID)
	}
	s.lease.Close()
}

func (s *turnScope) definitions() []provider.ToolDefinition {
	result := []provider.ToolDefinition{
		tool("axiom_capability_search", "Search the compact capability index for relevant installed Agent tools, lazy skills, or creator actions. Results contain summaries only; call axiom_capability_load before use.", `{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"additionalProperties":false}`),
		tool("axiom_capability_load", "Load one exact capability from search results. Agent tools become callable with their exact schema; skills return their instructions lazily.", `{"type":"object","required":["capabilityId"],"properties":{"capabilityId":{"type":"string"}},"additionalProperties":false}`),
	}
	if !s.evaluation {
		result = append(result, fragmentTools()...)
	}
	for _, item := range s.loaded {
		result = append(result, item.Definition)
	}
	for _, item := range s.loadedCreate {
		result = append(result, item)
	}
	for name, manifest := range s.loadedCapsules {
		definition := provider.ToolDefinition{Type: "function"}
		definition.Function.Name = name
		definition.Function.Description = manifest.Contract.Summary + " [workspace capsule]"
		definition.Function.Parameters = append(json.RawMessage(nil), manifest.Contract.InputSchema...)
		result = append(result, definition)
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
	case "axiom_fragment_create":
		return s.createFragment(arguments)
	case "axiom_fragment_invoke":
		return s.invokeFragment(ctx, arguments)
	case "axiom_fragment_drop":
		return s.dropFragment(arguments)
	case "axiom_capsule_save":
		return s.saveCapsule(arguments)
	case "axiom_capsule_promote":
		return s.promoteCapsule(arguments)
	case "axiom_promotion_status":
		return s.promotionStatus(arguments)
	}
	if manifest, ok := s.loadedCapsules[name]; ok {
		output, _, err := s.owner.capsules.Invoke(ctx, manifest.ID, arguments)
		if err != nil {
			return toolError(err.Error())
		}
		var value any
		if json.Unmarshal(output, &value) != nil {
			value = json.RawMessage(output)
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
	if s.owner != nil && s.owner.capsules != nil {
		for _, item := range s.owner.capsules.List() {
			candidate := capabilityCandidate{ID: item.ID, Kind: "capsule", Summary: item.Summary, Tags: item.Tags, Visibility: "workspace"}
			candidate.Score = relevance(terms, item.ID+" "+item.Name+" "+item.Summary+" "+item.Intent+" "+strings.Join(item.Tags, " "))
			if candidate.Score > 0 || len(terms) == 0 {
				result = append(result, candidate)
			}
		}
	}
	for id, definition := range s.creator {
		candidate := capabilityCandidate{ID: id, Kind: "creator-action", Summary: definition.Function.Description, Visibility: "creator-only"}
		candidate.Score = relevance(terms, id+" "+definition.Function.Description+" plugin create build generate install source revision inspect read tree diff patch repair rollback 插件 创建 生成 源码 查看 修改 补丁 构建 安装")
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
	if s.owner != nil && s.owner.capsules != nil {
		manifest, err := s.owner.capsules.Get(id)
		if err != nil {
			return nil, fmt.Errorf("capability %q is not present in this turn snapshot", id)
		}
		digest := sha256.Sum256([]byte(manifest.Digest + "\x00" + manifest.ID))
		name := "axiom_capsule_" + hex.EncodeToString(digest[:8])
		definition := provider.ToolDefinition{Type: "function"}
		definition.Function.Name = name
		definition.Function.Description = manifest.Contract.Summary + " [workspace capsule; fallback: " + manifest.Contract.Fallback + "]"
		definition.Function.Parameters = append(json.RawMessage(nil), manifest.Contract.InputSchema...)
		s.loadedCapsules[name] = manifest
		return map[string]any{"id": id, "kind": "capsule", "functionName": name, "digest": manifest.Digest, "inputSchema": manifest.Contract.InputSchema, "outputSchema": manifest.Contract.OutputSchema, "fallback": manifest.Contract.Fallback}, nil
	}
	return nil, fmt.Errorf("capability %q is not present in this turn snapshot", id)
}

func (s *turnScope) createFragment(arguments json.RawMessage) json.RawMessage {
	if s.evaluation || s.conversationID == "" || s.turnID == "" {
		return toolError("fragments cannot be created in this run mode")
	}
	var input struct {
		Name          string           `json:"name"`
		Summary       string           `json:"summary"`
		Intent        string           `json:"intent"`
		Tags          []string         `json:"tags"`
		Scope         capability.Scope `json:"scope"`
		InputSchema   json.RawMessage  `json:"inputSchema"`
		OutputSchema  json.RawMessage  `json:"outputSchema"`
		Program       string           `json:"program"`
		Fallback      string           `json:"fallback"`
		TimeoutMillis int              `json:"timeoutMillis"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return toolError("invalid fragment definition")
	}
	if input.Scope == "" {
		input.Scope = capability.ScopeTurn
	}
	contract := capability.Contract{
		APIVersion: capability.APIVersion, ID: fragmentContractID(input.Name), Name: input.Name, Summary: input.Summary, Intent: input.Intent,
		Tags: input.Tags, Tier: capability.TierFragment, Scope: input.Scope, Runtime: capability.RuntimeJavaScript,
		InputSchema: input.InputSchema, OutputSchema: input.OutputSchema,
		Limits: capability.Limits{TimeoutMillis: input.TimeoutMillis, MemoryMiB: 64, MaxOutputKiB: 256}, Fallback: input.Fallback,
	}
	fragment, err := s.owner.fragments.Register(s.userID, s.conversationID, s.turnID, contract, input.Program)
	if err != nil {
		return toolError(err.Error())
	}
	return toolOK(fragment)
}

func (s *turnScope) invokeFragment(ctx context.Context, arguments json.RawMessage) json.RawMessage {
	if s.evaluation {
		return toolError("fragments cannot run in evaluation mode")
	}
	var input struct {
		FragmentID string          `json:"fragmentId"`
		Input      json.RawMessage `json:"input"`
	}
	if json.Unmarshal(arguments, &input) != nil || input.FragmentID == "" || len(input.Input) == 0 {
		return toolError("fragmentId and input are required")
	}
	output, err := s.owner.fragments.Invoke(ctx, s.userID, s.conversationID, input.FragmentID, input.Input)
	if err != nil {
		return toolError(err.Error())
	}
	var value any
	if json.Unmarshal(output, &value) != nil {
		return toolError("fragment returned invalid JSON")
	}
	return toolOK(value)
}

func (s *turnScope) dropFragment(arguments json.RawMessage) json.RawMessage {
	if s.evaluation {
		return toolError("fragments cannot be changed in evaluation mode")
	}
	var input struct {
		FragmentID string `json:"fragmentId"`
	}
	if json.Unmarshal(arguments, &input) != nil || input.FragmentID == "" {
		return toolError("fragmentId is required")
	}
	if err := s.owner.fragments.Drop(s.userID, s.conversationID, input.FragmentID); err != nil {
		return toolError(err.Error())
	}
	return toolOK(map[string]any{"dropped": true, "fragmentId": input.FragmentID})
}

func (s *turnScope) saveCapsule(arguments json.RawMessage) json.RawMessage {
	if s.evaluation {
		return toolError("capsules cannot be saved in evaluation mode")
	}
	var input struct {
		FragmentID string `json:"fragmentId"`
		Intent     string `json:"intent"`
		Fallback   string `json:"fallback"`
	}
	if json.Unmarshal(arguments, &input) != nil || input.FragmentID == "" {
		return toolError("fragmentId is required")
	}
	fragment, err := s.owner.fragments.Get(s.userID, s.conversationID, input.FragmentID)
	if err != nil {
		return toolError(err.Error())
	}
	manifest, err := capsule.FromFragment(fragment, input.Intent, input.Fallback)
	if err != nil {
		return toolError(err.Error())
	}
	manifest, err = s.owner.capsules.Save(manifest)
	if err != nil {
		return toolError(err.Error())
	}
	return toolOK(manifest.Summary())
}

func (s *turnScope) promoteCapsule(arguments json.RawMessage) json.RawMessage {
	if s.evaluation {
		return toolError("capsules cannot be promoted in evaluation mode")
	}
	var input struct {
		CapsuleID string `json:"capsuleId"`
	}
	if json.Unmarshal(arguments, &input) != nil || strings.TrimSpace(input.CapsuleID) == "" {
		return toolError("capsuleId is required")
	}
	job, err := s.owner.PromoteCapsule(s.userID, input.CapsuleID)
	if err != nil {
		return toolError(err.Error())
	}
	return toolOK(map[string]any{"job": job, "approvalBoundary": "The pipeline may verify and build a release, but only the user can approve and install it."})
}

func (s *turnScope) promotionStatus(arguments json.RawMessage) json.RawMessage {
	var input struct {
		JobID string `json:"jobId"`
	}
	if len(arguments) > 0 && json.Unmarshal(arguments, &input) != nil {
		return toolError("invalid promotion status arguments")
	}
	jobs := s.owner.Promotions(s.userID)
	if input.JobID == "" {
		return toolOK(jobs)
	}
	for _, job := range jobs {
		if job.ID == input.JobID {
			return toolOK(job)
		}
	}
	return toolError("promotion job not found")
}

func fragmentContractID(name string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(name)))
	return "fragment." + hex.EncodeToString(digest[:8])
}

func fragmentTools() []provider.ToolDefinition {
	return []provider.ToolDefinition{
		tool("axiom_fragment_create", "Create an isolated zero-compile JavaScript capability for repeated deterministic work in this turn or conversation. The program is a function body with one input object and must return JSON. It has no file, network, process, environment, or secret access.", `{"type":"object","required":["name","summary","intent","inputSchema","outputSchema","program","fallback"],"properties":{"name":{"type":"string"},"summary":{"type":"string"},"intent":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}},"scope":{"type":"string","enum":["turn","conversation"]},"inputSchema":{"type":"object"},"outputSchema":{"type":"object"},"program":{"type":"string"},"fallback":{"type":"string"},"timeoutMillis":{"type":"integer"}},"additionalProperties":false}`),
		tool("axiom_fragment_invoke", "Invoke one previously created fragment by opaque handle. Do not recreate a fragment when the same handle still applies.", `{"type":"object","required":["fragmentId","input"],"properties":{"fragmentId":{"type":"string"},"input":{"type":"object"}},"additionalProperties":false}`),
		tool("axiom_fragment_drop", "Drop an ephemeral capability that is no longer useful.", `{"type":"object","required":["fragmentId"],"properties":{"fragmentId":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_capsule_save", "Promote a successfully executed conversation fragment into an immutable workspace capsule with its real execution evidence. This does not install a global plugin.", `{"type":"object","required":["fragmentId"],"properties":{"fragmentId":{"type":"string"},"intent":{"type":"string"},"fallback":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_capsule_promote", "Queue a verified workspace Capsule for background promotion into a compiled reference plugin. The pipeline stops at the user approval boundary and never installs automatically.", `{"type":"object","required":["capsuleId"],"properties":{"capsuleId":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_promotion_status", "Inspect background Capsule promotion jobs without waiting for them. Use the returned Forge project only after the job reaches user approval.", `{"type":"object","properties":{"jobId":{"type":"string"}},"additionalProperties":false}`),
	}
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
		tool("axiom_plugin_source_tree", "Inspect a compact file tree and content hashes for a generated plugin without loading file contents.", `{"type":"object","required":["projectId"],"properties":{"projectId":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_read_source", "Read one allowed text source file from a generated plugin Git workspace.", `{"type":"object","required":["projectId","path"],"properties":{"projectId":{"type":"string"},"path":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_source_diff", "Read the latest committed plugin source change as a bounded unified diff.", `{"type":"object","required":["projectId"],"properties":{"projectId":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_write_source", "Create or replace one text source file against an inspected plugin revision. Prefer a patch for ordinary edits.", `{"type":"object","required":["projectId","path","content","expectedRevision"],"properties":{"projectId":{"type":"string"},"path":{"type":"string"},"content":{"type":"string"},"expectedRevision":{"type":"string"}},"additionalProperties":false}`),
		tool("axiom_plugin_apply_patch", "Apply a revision-checked Git patch to allowed plugin text sources. Deletion, rename, binary, mode, and out-of-contract changes are rejected.", `{"type":"object","required":["projectId","expectedRevision","patch"],"properties":{"projectId":{"type":"string"},"expectedRevision":{"type":"string"},"patch":{"type":"string"}},"additionalProperties":false}`),
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
