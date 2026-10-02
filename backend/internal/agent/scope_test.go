package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/permissions"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginmanifest"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/skills"
	"axiom.local/agent/internal/storage"
)

type scopedGitCredentialTestBroker struct{}

func (scopedGitCredentialTestBroker) Lookup(string) (string, string, bool) {
	return "user", "secret", true
}

func TestTaskWorkspaceQuotaFlowsIntoLinuxToolContract(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux command contract is platform-specific")
	}
	makeScope := func() *turnScope {
		return &turnScope{owner: &Service{}, coreTools: map[string]coretools.Tool{}}
	}
	cloudScope := makeScope()
	cloudScope.setWorkspaceRootWithDiskQuota("/workspace/cloud-task", 8<<30)
	cloudTool, ok := cloudScope.coreTools["exec_command"]
	if !ok || !strings.Contains(cloudTool.Definition.Function.Description, "hard limit of 8589934592 bytes") {
		t.Fatalf("cloud task workspace quota did not reach the Agent tool contract: %+v", cloudTool.Definition.Function)
	}
	localScope := makeScope()
	localScope.setWorkspaceRoot("/workspace/local-node")
	localTool, ok := localScope.coreTools["exec_command"]
	if !ok || !strings.Contains(localTool.Definition.Function.Description, "no O-managed per-task disk quota") {
		t.Fatalf("local workspace was incorrectly described as quota-managed: %+v", localTool.Definition.Function)
	}
}

func TestExecutionConfigScopesGitCredentialsToConversationRepository(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := &Service{store: store}
	service.SetGitCredentialBroker(scopedGitCredentialTestBroker{})
	config := service.executionConfigSnapshot("https://github.com/acme/o-agent")
	if config.GitCredentials == nil || len(config.GitCredentialURLs) != 1 || config.GitCredentialURLs[0] != "https://github.com/acme/o-agent.git" {
		t.Fatalf("project repository scope was not attached to the execution config: %+v", config)
	}
	if invalid := service.executionConfigSnapshot("https://attacker.example/acme/o-agent.git"); len(invalid.GitCredentialURLs) != 0 {
		t.Fatalf("unsupported host received a Git credential scope: %+v", invalid.GitCredentialURLs)
	}
	if noProject := service.executionConfigSnapshot(); len(noProject.GitCredentialURLs) != 0 {
		t.Fatalf("credential scope leaked into a turn without a selected project: %+v", noProject.GitCredentialURLs)
	}
}

func TestSearchUsesCompactTerms(t *testing.T) {
	terms := searchTerms("Find workspace TODO inspection")
	if got := relevance(terms, "workspace inspector scans todo markers"); got < 20 {
		t.Fatalf("expected relevant capability to score, got %d", got)
	}
	if got := relevance(terms, "calendar weather"); got != 0 {
		t.Fatalf("irrelevant capability scored %d", got)
	}
}

func TestExecCommandExpandedAccessIsClassifiedForApproval(t *testing.T) {
	scope := &turnScope{permissionProfile: domain.PermissionProfileWorkspaceAutonomy}
	for _, test := range []struct {
		name      string
		arguments string
		resource  string
	}{
		{name: "workspace write", arguments: `{"cmd":"gofmt -w main.go","writeAccess":true}`, resource: "工作区写入"},
		{name: "network", arguments: `{"cmd":"go mod download","networkAccess":true}`, resource: "外网访问"},
		{name: "network hosts", arguments: `{"cmd":"go mod download","networkAccess":true,"networkHosts":["api.example.com","github.com"]}`, resource: "外网访问: api.example.com, github.com"},
		{name: "both", arguments: `{"cmd":"go mod download","writeAccess":true,"networkAccess":true}`, resource: "工作区写入 + 外网访问"},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision, request := scope.authorizeTool("exec_command", json.RawMessage(test.arguments))
			if request.Effect != permissions.EffectShell || request.Resource != test.resource {
				t.Fatalf("expanded command access was not classified: request=%#v", request)
			}
			if decision.Outcome != permissions.OutcomeAllow {
				t.Fatalf("workspace-auto should allow expanded command access: %#v", decision)
			}
			requestApprovalScope := &turnScope{permissionProfile: domain.PermissionProfileRequestApproval}
			requestDecision, _ := requestApprovalScope.authorizeTool("exec_command", json.RawMessage(test.arguments))
			if requestDecision.Outcome != permissions.OutcomeAsk {
				t.Fatalf("request-approval mode should ask for expanded command access: %#v", requestDecision)
			}
		})
	}

	decision, request := scope.authorizeTool("exec_command", json.RawMessage(`{"cmd":"git status"}`))
	if request.Effect != permissions.EffectRead || decision.Outcome != permissions.OutcomeAllow {
		t.Fatalf("default command should remain read-only and require no pre-approval: request=%#v decision=%#v", request, decision)
	}
}

func TestWebSearchUsesNetworkPermissionProfile(t *testing.T) {
	for _, test := range []struct {
		profile domain.PermissionProfile
		want    permissions.Outcome
	}{
		{profile: domain.PermissionProfileReadOnly, want: permissions.OutcomeDeny},
		{profile: domain.PermissionProfileRequestApproval, want: permissions.OutcomeAsk},
		{profile: domain.PermissionProfileWorkspaceAutonomy, want: permissions.OutcomeAllow},
		{profile: domain.PermissionProfileFullyAutonomous, want: permissions.OutcomeAllow},
	} {
		scope := &turnScope{permissionProfile: test.profile}
		decision, request := scope.authorizeTool("web_search", json.RawMessage(`{"query":"GitHub repo source"}`))
		if request.Effect != permissions.EffectExternalRead {
			t.Errorf("profile %s: web search effect=%q, want external_read", test.profile, request.Effect)
		}
		if decision.Outcome != test.want {
			t.Errorf("profile %s: web search outcome=%q, want %q", test.profile, decision.Outcome, test.want)
		}
	}
}

func TestBrowserSessionSeparatesReadAndRemoteWritePermissions(t *testing.T) {
	for _, test := range []struct {
		profile domain.PermissionProfile
		open    permissions.Outcome
		click   permissions.Outcome
	}{
		{profile: domain.PermissionProfileReadOnly, open: permissions.OutcomeDeny, click: permissions.OutcomeDeny},
		{profile: domain.PermissionProfileRequestApproval, open: permissions.OutcomeAsk, click: permissions.OutcomeAsk},
		{profile: domain.PermissionProfileWorkspaceAutonomy, open: permissions.OutcomeAllow, click: permissions.OutcomeAllow},
		{profile: domain.PermissionProfileFullyAutonomous, open: permissions.OutcomeAllow, click: permissions.OutcomeAllow},
	} {
		scope := &turnScope{permissionProfile: test.profile}
		openDecision, openRequest := scope.authorizeTool("browser_session", json.RawMessage(`{"action":"open","url":"https://example.com"}`))
		if openRequest.Effect != permissions.EffectExternalRead || openDecision.Outcome != test.open {
			t.Errorf("profile %s: browser open request=%#v decision=%#v", test.profile, openRequest, openDecision)
		}
		clickDecision, clickRequest := scope.authorizeTool("browser_session", json.RawMessage(`{"action":"click","sessionId":"browser_123","selector":"button.submit"}`))
		if clickRequest.Effect != permissions.EffectExternalWrite || clickDecision.Outcome != test.click {
			t.Errorf("profile %s: browser click request=%#v decision=%#v", test.profile, clickRequest, clickDecision)
		}
		closeDecision, closeRequest := scope.authorizeTool("browser_session", json.RawMessage(`{"action":"close","sessionId":"browser_123"}`))
		if closeRequest.Effect != permissions.EffectEphemeral || closeDecision.Outcome != permissions.OutcomeAllow {
			t.Errorf("profile %s: closing a browser session should always be allowed cleanup: request=%#v decision=%#v", test.profile, closeRequest, closeDecision)
		}
	}
}

func TestSearchCardsExcludeHiddenCapabilitiesAndFullSchemas(t *testing.T) {
	scope := &turnScope{tools: map[string]pluginforge.CapabilityBinding{
		"visible.tool": {ToolExport: pluginmanifest.ToolExport{ID: "visible.tool", Summary: "Inspect workspace", Visibility: "discoverable", InputSchema: json.RawMessage(`{"type":"object","properties":{"secret":{"type":"string"}}}`)}, ReleaseID: "rel_visible"},
		"hidden.tool":  {ToolExport: pluginmanifest.ToolExport{ID: "hidden.tool", Summary: "Inspect workspace secretly", Visibility: "none"}, ReleaseID: "rel_hidden"},
	}, skills: map[string]pluginforge.SkillBinding{}, creator: map[string]provider.ToolDefinition{}}
	results := scope.search("workspace inspect", 10)
	if len(results) != 1 || results[0].ID != "visible.tool" {
		t.Fatalf("unexpected search projection: %#v", results)
	}
	raw, _ := json.Marshal(results)
	if strings.Contains(string(raw), "inputSchema") || strings.Contains(string(raw), "secret") {
		t.Fatalf("search card leaked a full schema: %s", raw)
	}
}

func TestLocalSkillSelectionPinsBodyAndUsesUntrustedAssistantContext(t *testing.T) {
	goTestSource := "---\nname: Go Test Skill\ndescription: Run tests\ntriggers: go test\n---\nRun the relevant Go tests."
	otherSource := "---\nname: Release Skill\ndescription: Release guidance\ntriggers: release\n---\nPrepare a release."
	newSkill := func(id, source, body, trigger string) skills.Skill {
		digest := sha256.Sum256([]byte(source))
		return skills.Skill{ID: id, Name: id, Description: id + " description", Prompt: body, SourceContent: source, ContentHash: hex.EncodeToString(digest[:]), Triggers: []string{trigger}}
	}
	scope := &turnScope{
		evaluation: true,
		localSkills: map[string]skills.Skill{
			"local-skill:go-test": newSkill("go-test", goTestSource, "Run the relevant Go tests.", "go test"),
			"local-skill:release": newSkill("release", otherSource, "Prepare a release.", "release"),
		},
		ephemeralSources: map[string][]byte{},
	}
	messages, selected, enabledCount, err := scope.localSkillContext(context.Background(), "Please run go test ./internal/agent")
	if err != nil {
		t.Fatalf("build local Skill context: %v", err)
	}
	if enabledCount != 2 || len(selected) != 1 || selected[0].ID != "local-skill:go-test" || selected[0].ContentHash == "" || selected[0].SourceRef == "" {
		t.Fatalf("unexpected proactive selection: enabled=%d selected=%#v", enabledCount, selected)
	}
	if len(messages) != 2 || messages[0].Role != "assistant" || messages[1].Role != "assistant" || messages[1].SourceID != selected[0].SourceRef {
		t.Fatalf("catalog and selected body were not lower-trust, source-linked context: %#v", messages)
	}
	if strings.Contains(messages[0].Content, "Run the relevant Go tests") || !strings.Contains(messages[1].Content, "Run the relevant Go tests") {
		t.Fatalf("skill body was not loaded only for the matched trigger: %#v", messages)
	}
	archived, hash, err := scope.readContextSourceInConversation(context.Background(), "conversation", selected[0].SourceRef)
	if err != nil || archived != goTestSource || hash != selected[0].ContentHash {
		t.Fatalf("skill source did not read back at its pinned hash: content=%q hash=%q err=%v", archived, hash, err)
	}
	loaded, err := scope.load(context.Background(), selected[0].ID)
	if err != nil {
		t.Fatalf("load local Skill through capability path: %v", err)
	}
	loadedValue := loaded.(map[string]any)
	if loadedValue["contentHash"] != selected[0].ContentHash || loadedValue["sourceRef"] != selected[0].SourceRef || loadedValue["instructions"] != "Run the relevant Go tests." {
		t.Fatalf("capability load did not use the pinned Skill version: %#v", loadedValue)
	}
	search := scope.search("go test", 10)
	if len(search) != 1 || search[0].ID != selected[0].ID || search[0].ReleaseID != selected[0].ContentHash {
		t.Fatalf("local Skill was not discoverable with its pinned version: %#v", search)
	}

	generation := domain.AgentGeneration{ID: "gen_test", DefinitionDigest: "sha256:test", Definition: domain.AgentDefinition{Spec: domain.AgentSpec{SystemPrompt: "system"}}}
	contextMessages, _ := buildContextWithAdditions(domain.ConversationDetail{Messages: []domain.Message{{Role: "user", Content: "Please run go test"}}}, generation, "", 8192, []provider.ChatMessage{{Role: "system", Content: "skill content must not become system authority"}, messages[1]})
	if contextMessages[1].Role != "assistant" || contextMessages[2].Role != "assistant" || contextMessages[len(contextMessages)-1].Role != "user" {
		t.Fatalf("skill additions were not forcibly kept below system and current user context: %#v", contextMessages)
	}
}

func TestBuildContextKeepsFullTranscriptForUnifiedCoordinator(t *testing.T) {
	detail := domain.ConversationDetail{}
	for index := 0; index < 10; index++ {
		detail.Messages = append(detail.Messages, domain.Message{ID: "msg_" + string(rune('a'+index)), Role: "user", Content: strings.Repeat(string(rune('a'+index)), 7000)})
	}
	generation := domain.AgentGeneration{ID: "gen_test", DefinitionDigest: "sha256:test", Definition: domain.AgentDefinition{Spec: domain.AgentSpec{Strategy: "react.v1", SystemPrompt: "test system"}}}
	messages, omitted := buildContext(detail, generation, "", 16000)
	if omitted != 0 || len(messages) != 11 {
		t.Fatalf("context construction pre-trimmed history: omitted=%d messages=%d", omitted, len(messages))
	}
	if !strings.Contains(messages[len(messages)-1].Content, strings.Repeat("j", 100)) {
		t.Fatal("newest message was not retained")
	}
	if !strings.HasPrefix(messages[len(messages)-1].SourceID, "message:msg_j#") {
		t.Fatalf("message source does not pin a content version: %q", messages[len(messages)-1].SourceID)
	}
}

func TestBuildContextDoesNotPreSummarizeOldMessages(t *testing.T) {
	messages := make([]domain.Message, 0, 30)
	for i := 0; i < 30; i++ {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		messages = append(messages, domain.Message{Role: role, Content: strings.Repeat("中文目标与验证结果", 200)})
	}
	generation := domain.AgentGeneration{ID: "gen_test", DefinitionDigest: "sha256:test", Definition: domain.AgentDefinition{Spec: domain.AgentSpec{Strategy: "react.v1", SystemPrompt: "test system"}}}
	contextMessages, omitted := buildContext(domain.ConversationDetail{Messages: messages}, generation, "", 512)
	if omitted != 0 || len(contextMessages) != len(messages)+1 {
		t.Fatalf("history was summarized before the unified request planner: omitted=%d messages=%d", omitted, len(contextMessages))
	}
}

func TestCreatorActionsAreNotPartOfTheBootstrapTools(t *testing.T) {
	for id, definition := range creatorTools() {
		if id == "axiom_capability_search" || id == "axiom_capability_load" || definition.Function.Name != id {
			t.Fatalf("unexpected creator definition %q", id)
		}
	}
}

func TestCreatorActionsExposeLazySourceDevelopmentLoop(t *testing.T) {
	tools := creatorTools()
	for _, name := range []string{"axiom_plugin_source_tree", "axiom_plugin_read_source", "axiom_plugin_source_diff", "axiom_plugin_apply_patch", "axiom_plugin_build"} {
		if _, ok := tools[name]; !ok {
			t.Fatalf("missing creator action %s", name)
		}
	}
	if !strings.Contains(string(tools["axiom_plugin_apply_patch"].Function.Parameters), "expectedRevision") {
		t.Fatal("patch action must require optimistic revision control")
	}
}
