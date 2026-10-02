package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginruntime"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/runfiles"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func TestExportContinuationCheckpointReadsVerifiedDurableBoundary(t *testing.T) {
	ctx := context.Background()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	workspace := filepath.Dir(sourceFile)
	repoRoot := filepath.Clean(filepath.Join(workspace, "..", "..", ".."))
	root, err := os.MkdirTemp(repoRoot, ".handoff-checkpoint-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove handoff test directory: %v", err)
		}
	})
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	userID, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secure.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	providers := provider.New(store, vault)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"test-model","choices":[{"message":{"role":"assistant","content":"Cloud continuation succeeded."}}],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`))
	}))
	defer providerServer.Close()
	providerConfig, err := providers.Create(ctx, userID, provider.Input{Name: "handoff-test", Kind: provider.KindOpenAICompatible, BaseURL: providerServer.URL + "/v1", Model: "test-model", APIKey: "handoff-test-key", ContextWindow: 8192})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := evolution.New(store).EnsureSeed(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runGit := func(workdir string, args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = workdir
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v: %s", args, workdir, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	project := domain.Project{ID: "project_handoff_source", UserID: userID, Name: "handoff project", Workdir: filepath.Join(root, "source-project"), RemoteRepoURL: "https://github.com/example/handoff.git", RemoteBranch: "main", RepositoryProvider: "github", CreatedAt: now, UpdatedAt: now}
	if err := os.MkdirAll(project.Workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(project.Workdir, "init")
	runGit(project.Workdir, "config", "user.name", "O Agent Test")
	runGit(project.Workdir, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(project.Workdir, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(project.Workdir, "add", "README.md")
	runGit(project.Workdir, "commit", "-m", "base")
	baseCommit := runGit(project.Workdir, "rev-parse", "HEAD")
	project.ResolvedCommit = baseCommit
	runGit(project.Workdir, "remote", "add", "origin", project.RemoteRepoURL)
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "convo_handoff_export", UserID: userID, Title: "handoff export", ProviderID: providerConfig.ID, ProjectID: project.ID, PermissionProfile: domain.DefaultPermissionProfile(), CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversationWithGeneration(ctx, conversation, generation); err != nil {
		t.Fatal(err)
	}
	repository, err := pluginforge.OpenRepository(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	runtimeSupervisor, err := pluginruntime.NewWithData(workspace, filepath.Join(dataDir, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeSupervisor.Close()
	forge := pluginforge.NewService(repository, runtimeSupervisor, dataDir, workspace)
	runfileManager, err := runfiles.NewManager(filepath.Join(root, "agent-temp"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{hostCtx: ctx, store: store, providers: providers, forge: forge, evolution: evolution.New(store), workspaceRoot: workspace, runfiles: runfileManager, running: make(map[string]context.CancelCauseFunc), events: newEventBroker(), approvals: make(map[string]chan bool)}
	service.sandboxCommand = func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, workdir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		process := exec.CommandContext(ctx, command, args...)
		process.Dir = workdir
		output, err := process.Output()
		if err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				return string(output), string(exit.Stderr), err, nil
			}
			return string(output), "", err, nil
		}
		return string(output), "", nil, nil
	}

	turnID := "turn_handoff_export"
	input := domain.Message{ID: "msg_handoff_export_input", ConversationID: conversation.ID, Role: "user", Content: "finish the pending implementation", CreatedAt: now}
	turn := domain.AgentTurn{ID: turnID, ConversationID: conversation.ID, ProviderID: providerConfig.ID, AgentGenerationID: generation.ID, AgentDefinitionDigest: generation.DefinitionDigest, PermissionProfile: domain.DefaultPermissionProfile(), StartedAt: now, UpdatedAt: now}
	if err := store.StartAgentTurn(ctx, userID, turn, input, json.RawMessage(`{"source":"test"}`)); err != nil {
		t.Fatal(err)
	}
	scope, err := newTurnScope(service, userID, conversation.ID, "handoff_preflight")
	if err != nil {
		t.Fatal(err)
	}
	scope.permissionProfile = turn.PermissionProfile
	scope.setContextWindow(providerConfig.ContextWindow)
	scope.setWorkspaceRoot(project.Workdir, project.RemoteRepoURL)
	fingerprint, err := turnRuntimeFingerprint(providerConfig, generation, turn.PermissionProfile, project, true, scope)
	closeErr := scope.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	checkpoint := loopCheckpoint{
		Version: 1, ProviderID: providerConfig.ID, GenerationID: generation.DefinitionDigest,
		RuntimeFingerprint: fingerprint, ResumeAllowed: true, Stage: "loop",
		Messages: []provider.ChatMessage{{Role: "system", Content: "system prompt"}, {Role: "user", Content: input.Content}},
	}
	plain, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := providers.SealRunCheckpoint(plain)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(plain)
	assistant := domain.Message{ID: "msg_handoff_export_output", ConversationID: conversation.ID, Role: "assistant", Content: "Stopped at a safe budget boundary.", CreatedAt: now.Add(time.Second)}
	continuation := &storage.ContinuationState{Version: 1, Ciphertext: ciphertext, Nonce: nonce, ContentHash: hex.EncodeToString(digest[:])}
	if err := store.FinishAgentTurnWithContinuation(ctx, userID, turnID, "incomplete", "step_limit", &assistant, json.RawMessage(`{"reason":"step_limit"}`), continuation); err != nil {
		t.Fatal(err)
	}

	exported, err := service.ExportContinuationCheckpoint(ctx, userID, turnID)
	if err != nil {
		t.Fatal(err)
	}
	if exported.SourceTurnID != turnID || exported.SourceConversationID != conversation.ID || exported.SourceInputMessageID != input.ID || exported.ProviderID != providerConfig.ID || exported.GenerationID != generation.ID || exported.DefinitionDigest != generation.DefinitionDigest || exported.PermissionProfile != turn.PermissionProfile || exported.ContentSHA256 != hex.EncodeToString(digest[:]) || string(exported.Checkpoint) != string(plain) {
		t.Fatalf("exported checkpoint envelope did not match validated durable source: %+v", exported)
	}
	if err := service.ValidateHandoffCheckpointBindings(ctx, userID, conversation.ID, exported); err != nil {
		t.Fatalf("cloud runtime rejected a matching checkpoint binding: %v", err)
	}
	branchNow := time.Now().UTC()
	branch := domain.Conversation{ID: "conv_handoff_cloud_branch", UserID: userID, Title: "cloud branch", ProviderID: providerConfig.ID, ProjectID: project.ID, PermissionProfile: turn.PermissionProfile, ParentConversationID: conversation.ID, CreatedAt: branchNow, UpdatedAt: branchNow}
	branchInput, branchAssistant := input, assistant
	branchInput.CreatedAt, branchAssistant.CreatedAt = branchNow, branchNow
	branchMessages := []domain.Message{branchInput, branchAssistant}
	continued, err := store.CreateTaskContinuationConversation(ctx, branch, assistant.ID, branchMessages)
	if err != nil {
		t.Fatal(err)
	}
	imported, err := service.ImportHandoffContinuation(ctx, userID, continued.ID, exported, domain.AgentTurn{
		ID: TaskContinuationTurnID(conversation.ID, "task_handoff_export"), ConversationID: continued.ID, UserID: userID,
		InputMessageID: continued.Messages[0].ID, ProviderID: continued.ProviderID, AgentGenerationID: continued.AgentGenerationID,
		AgentDefinitionDigest: continued.AgentDefinitionDigest, PermissionProfile: continued.PermissionProfile, Status: "incomplete", StopReason: "step_limit",
	}, continued.Messages[1].ID, branchNow.Add(time.Second))
	if err != nil || imported.Status != "incomplete" || !imported.ContinuationAvailable || imported.ConversationID != continued.ID {
		t.Fatalf("verified node checkpoint was not imported as a cloud continuation: turn=%+v err=%v", imported, err)
	}
	importedSnapshot, err := store.AgentContinuationSnapshot(ctx, userID, imported.ID)
	if err != nil || importedSnapshot.Status != "available" || importedSnapshot.ContentHash != exported.ContentSHA256 {
		t.Fatalf("cloud continuation snapshot did not read back: %+v err=%v", importedSnapshot, err)
	}
	service.workspaceRoot = root
	const cloudExecutionTaskID = "task_handoff_cloud_resume"
	receipt, continuedTurn, _, err := service.ExecuteExecutionTaskContinuation(ctx, userID, cloudExecutionTaskID, imported.ID, "continue from the verified node checkpoint", "handoff-cloud-resume")
	if err != nil || receipt.TurnID == "" || receipt.ConversationID != continued.ID || receipt.ContinuedFromTurnID != imported.ID || continuedTurn.Status != "completed" {
		t.Fatalf("cloud Agent did not complete the imported continuation: receipt=%+v turn=%+v err=%v", receipt, continuedTurn, err)
	}
	if continuedTurn.Status != "completed" || continuedTurn.ConversationID != continued.ID {
		t.Fatalf("cloud continuation did not complete from the imported checkpoint: turn=%+v err=%v", continuedTurn, err)
	}
	assertExecutionTaskTurnBinding := func(turnID string) {
		t.Helper()
		events, eventsErr := store.TurnEvents(ctx, userID, turnID, 0)
		if eventsErr != nil {
			t.Fatalf("read back task-aware turn events: %v", eventsErr)
		}
		for _, event := range events {
			if event.Kind != "turn.started" {
				continue
			}
			var details map[string]any
			if err := json.Unmarshal(event.Details, &details); err != nil {
				t.Fatalf("decode durable turn.started details: %v", err)
			}
			if details["executionTaskId"] == cloudExecutionTaskID {
				return
			}
		}
		t.Fatalf("turn.started did not durably record execution task %q", cloudExecutionTaskID)
	}
	assertExecutionTaskTurnBinding(receipt.TurnID)
	ordinaryReceipt, ordinaryTurn, _, err := service.ExecuteExecutionTaskTurn(ctx, userID, cloudExecutionTaskID, continued.ID, "verify cloud task identity is durable")
	if err != nil || ordinaryReceipt.TurnID == "" || ordinaryTurn.Status != "completed" {
		t.Fatalf("task-aware cloud Agent turn did not complete: receipt=%+v turn=%+v err=%v", ordinaryReceipt, ordinaryTurn, err)
	}
	assertExecutionTaskTurnBinding(ordinaryReceipt.TurnID)
	projectlessConversation := domain.Conversation{ID: "conv_handoff_projectless", UserID: userID, Title: "projectless cloud task", ProviderID: providerConfig.ID, PermissionProfile: domain.DefaultPermissionProfile(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := store.CreateConversationWithGeneration(ctx, projectlessConversation, generation); err != nil {
		t.Fatal(err)
	}
	projectlessTaskID := "task_handoff_projectless"
	projectlessReceipt, projectlessTurn, _, err := service.ExecuteExecutionTaskTurn(ctx, userID, projectlessTaskID, projectlessConversation.ID, "complete in the isolated cloud task workspace")
	if err != nil || projectlessReceipt.TurnID == "" || projectlessTurn.Status != "completed" {
		t.Fatalf("projectless cloud Agent task did not complete: receipt=%+v turn=%+v err=%v", projectlessReceipt, projectlessTurn, err)
	}
	projectlessWorkspace, err := store.ExecutionTaskWorkspace(ctx, userID, projectlessTaskID)
	if err != nil || projectlessWorkspace.Status != "ready" || projectlessWorkspace.Workdir == "" || !pathWithin(service.workspaceRoot, projectlessWorkspace.Workdir) {
		t.Fatalf("projectless Agent turn did not use a durable isolated workspace: %+v err=%v", projectlessWorkspace, err)
	}
	retryReceipt, err := service.ContinueTurn(ctx, userID, imported.ID, "continue from the verified node checkpoint", "handoff-cloud-resume", nil, nil)
	if err != nil || retryReceipt.TurnID != receipt.TurnID || retryReceipt.ConversationID != receipt.ConversationID {
		t.Fatalf("cloud continuation retry was not idempotent: first=%+v retry=%+v err=%v", receipt, retryReceipt, err)
	}
	retryImport, err := service.ImportHandoffContinuation(ctx, userID, continued.ID, exported, domain.AgentTurn{
		ID: imported.ID, ConversationID: continued.ID, UserID: userID, InputMessageID: continued.Messages[0].ID,
		ProviderID: continued.ProviderID, AgentGenerationID: continued.AgentGenerationID, AgentDefinitionDigest: continued.AgentDefinitionDigest,
		PermissionProfile: continued.PermissionProfile, Status: "incomplete", StopReason: "step_limit",
	}, continued.Messages[1].ID, branchNow.Add(2*time.Second))
	if err != nil || retryImport.ID != imported.ID || retryImport.ContinuationAvailable {
		t.Fatalf("checkpoint re-import after consumption changed its durable state: turn=%+v err=%v", retryImport, err)
	}
	branchRetry, err := store.CreateTaskContinuationConversation(ctx, branch, assistant.ID, branchMessages)
	if err != nil || len(branchRetry.Messages) <= len(branchMessages) || branchRetry.Messages[0].Content != branchMessages[0].Content {
		persistedBranch, readErr := store.Conversation(ctx, userID, continued.ID)
		t.Fatalf("task conversation retry did not preserve the resumed cloud turn: conversation=%+v persisted=%+v readErr=%v err=%v", branchRetry, persistedBranch, readErr, err)
	}
	turnReadBack, err := store.AgentTurn(ctx, userID, turnID)
	if err != nil || turnReadBack.Status != "incomplete" || !turnReadBack.ContinuationAvailable {
		t.Fatalf("export consumed or changed the source checkpoint: turn=%+v err=%v", turnReadBack, err)
	}
	snapshot, err := store.AgentContinuationSnapshot(ctx, userID, turnID)
	if err != nil || snapshot.Status != "available" || snapshot.ContentHash != exported.ContentSHA256 {
		t.Fatalf("durable source checkpoint changed during export: snapshot=%+v err=%v", snapshot, err)
	}

	const deltaTaskID = "task_handoff_delta"
	deltaProject := domain.Project{ID: ProjectDeltaProjectID(project.ID, deltaTaskID), UserID: userID, Name: project.Name + " · task " + deltaTaskID, Workdir: filepath.Join(root, "delta-project"), RemoteRepoURL: project.RemoteRepoURL, RemoteBranch: "codex/task-" + deltaTaskID, RepositoryProvider: project.RepositoryProvider, CreatedAt: branchNow, UpdatedAt: branchNow}
	runGit(project.Workdir, "worktree", "add", "-b", deltaProject.RemoteBranch, deltaProject.Workdir, baseCommit)
	if err := os.WriteFile(filepath.Join(deltaProject.Workdir, "task.txt"), []byte("task delta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(deltaProject.Workdir, "add", "task.txt")
	runGit(deltaProject.Workdir, "commit", "-m", "task delta")
	deltaCommit := runGit(deltaProject.Workdir, "rev-parse", "HEAD")
	deltaProject.ResolvedCommit = deltaCommit
	if err := store.CreateProject(ctx, deltaProject); err != nil {
		t.Fatal(err)
	}
	deltaBranch := domain.Conversation{ID: "conv_handoff_delta_branch", UserID: userID, Title: "delta branch", ProviderID: providerConfig.ID, ProjectID: deltaProject.ID, PermissionProfile: turn.PermissionProfile, ParentConversationID: conversation.ID, CreatedAt: branchNow, UpdatedAt: branchNow}
	deltaMessages := []domain.Message{{ID: "msg_handoff_delta_input", ConversationID: conversation.ID, Role: "user", Content: input.Content, CreatedAt: branchNow}, {ID: "msg_handoff_delta_output", ConversationID: conversation.ID, Role: "assistant", Content: assistant.Content, CreatedAt: branchNow}}
	deltaConversation, err := store.CreateTaskContinuationConversation(ctx, deltaBranch, input.ID, deltaMessages)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RebindHandoffCheckpointForTaskDelta(ctx, userID, "another_task", conversation.ID, deltaConversation.ID, exported, baseCommit, deltaCommit); err == nil {
		t.Fatal("checkpoint rebinding accepted a task branch with the wrong task identity")
	}
	tampered := exported
	tampered.Checkpoint = append(json.RawMessage(nil), exported.Checkpoint...)
	tampered.Checkpoint[0] ^= 1
	if _, err := service.RebindHandoffCheckpointForTaskDelta(ctx, userID, deltaTaskID, conversation.ID, deltaConversation.ID, tampered, baseCommit, deltaCommit); err == nil {
		t.Fatal("checkpoint rebinding accepted content that no longer matches the source digest")
	}
	tamperedProject := deltaProject
	tamperedProject.Instructions = "unverified project instructions"
	if err := store.UpdateProject(ctx, userID, tamperedProject); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RebindHandoffCheckpointForTaskDelta(ctx, userID, deltaTaskID, conversation.ID, deltaConversation.ID, exported, baseCommit, deltaCommit); err == nil {
		t.Fatal("checkpoint rebinding accepted a task branch with altered project instructions")
	}
	if err := store.UpdateProject(ctx, userID, deltaProject); err != nil {
		t.Fatal(err)
	}
	rebound, err := service.RebindHandoffCheckpointForTaskDelta(ctx, userID, deltaTaskID, conversation.ID, deltaConversation.ID, exported, baseCommit, deltaCommit)
	if err != nil {
		t.Fatalf("rebind checkpoint to verified task delta branch: %v", err)
	}
	if rebound.ProjectID != deltaProject.ID || rebound.ProjectCommit != deltaCommit || rebound.ContentSHA256 == exported.ContentSHA256 || len(rebound.Checkpoint) == 0 {
		t.Fatalf("rebound checkpoint does not name the isolated task project: %+v", rebound)
	}
	if err := service.ValidateHandoffCheckpointBindings(ctx, userID, deltaConversation.ID, rebound); err != nil {
		t.Fatalf("rebound checkpoint did not match the isolated branch runtime: %v", err)
	}
	deltaInputMessage := deltaConversation.Messages[len(deltaConversation.Messages)-2]
	deltaResultMessage := deltaConversation.Messages[len(deltaConversation.Messages)-1]
	deltaTurn, err := service.ImportHandoffContinuation(ctx, userID, deltaConversation.ID, rebound, domain.AgentTurn{
		ID: TaskContinuationTurnID(conversation.ID, deltaTaskID), ConversationID: deltaConversation.ID, UserID: userID,
		InputMessageID: deltaInputMessage.ID, ProviderID: deltaConversation.ProviderID, AgentGenerationID: deltaConversation.AgentGenerationID,
		AgentDefinitionDigest: deltaConversation.AgentDefinitionDigest, PermissionProfile: deltaConversation.PermissionProfile, Status: "incomplete", StopReason: "step_limit",
	}, deltaResultMessage.ID, branchNow.Add(3*time.Second))
	if err != nil || !deltaTurn.ContinuationAvailable {
		t.Fatalf("rebound project-delta checkpoint was not imported: turn=%+v err=%v", deltaTurn, err)
	}
	deltaReceipt, err := service.ContinueTurn(ctx, userID, deltaTurn.ID, "continue after the verified project changes", "handoff-delta-resume", nil, nil)
	if err != nil || deltaReceipt.TurnID == "" {
		t.Fatalf("cloud Agent did not continue from the imported project delta: receipt=%+v err=%v", deltaReceipt, err)
	}
	deltaDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deltaDeadline) {
		continued, readErr := store.AgentTurn(ctx, userID, deltaReceipt.TurnID)
		if readErr == nil && continued.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	deltaContinued, err := store.AgentTurn(ctx, userID, deltaReceipt.TurnID)
	if err != nil || deltaContinued.Status != "completed" || deltaContinued.ConversationID != deltaConversation.ID {
		t.Fatalf("cloud project-delta continuation did not complete on its isolated branch: turn=%+v err=%v", deltaContinued, err)
	}
	deltaSnapshot, err := store.AgentContinuationSnapshot(ctx, userID, deltaTurn.ID)
	if err != nil || deltaSnapshot.Status != "consumed" || deltaSnapshot.ContentHash != rebound.ContentSHA256 {
		t.Fatalf("rebound checkpoint consumption did not read back durably: snapshot=%+v err=%v", deltaSnapshot, err)
	}
	reboundRetry, err := service.RebindHandoffCheckpointForTaskDelta(ctx, userID, deltaTaskID, conversation.ID, deltaConversation.ID, exported, baseCommit, deltaCommit)
	if err != nil || reboundRetry.ContentSHA256 != rebound.ContentSHA256 || string(reboundRetry.Checkpoint) != string(rebound.Checkpoint) {
		t.Fatalf("checkpoint rebind retry was not deterministic: first=%+v retry=%+v err=%v", rebound, reboundRetry, err)
	}
	retryDeltaImport, err := service.ImportHandoffContinuation(ctx, userID, deltaConversation.ID, reboundRetry, domain.AgentTurn{
		ID: deltaTurn.ID, ConversationID: deltaConversation.ID, UserID: userID, InputMessageID: deltaInputMessage.ID,
		ProviderID: deltaConversation.ProviderID, AgentGenerationID: deltaConversation.AgentGenerationID,
		AgentDefinitionDigest: deltaConversation.AgentDefinitionDigest, PermissionProfile: deltaConversation.PermissionProfile,
		Status: "incomplete", StopReason: "step_limit",
	}, deltaResultMessage.ID, branchNow.Add(4*time.Second))
	if err != nil || retryDeltaImport.ID != deltaTurn.ID || retryDeltaImport.ContinuationAvailable {
		t.Fatalf("re-import after project-delta continuation changed the consumed turn: turn=%+v err=%v", retryDeltaImport, err)
	}
}
