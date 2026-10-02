package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/storage"
)

type LocalAgentExecutor struct {
	agent        *agent.Service
	providers    *provider.Service
	workspace    string
	providerID   string
	projectLocks sync.Map
	pauseMu      sync.Mutex
	pauseActive  map[string]*atomic.Bool
	pausePending map[string]struct{}
}

type localAgentPrompt struct {
	Kind                    string                    `json:"kind"`
	SourceConversationID    string                    `json:"sourceConversationId"`
	Content                 string                    `json:"content"`
	Title                   string                    `json:"title,omitempty"`
	SourceContextArtifactID string                    `json:"sourceContextArtifactId,omitempty"`
	SourceContextSHA256     string                    `json:"sourceContextSHA256,omitempty"`
	SourceContextByteSize   int64                     `json:"sourceContextByteSize,omitempty"`
	SourceContextFilePath   string                    `json:"sourceContextFilePath,omitempty"`
	SourceProjectDelta      *localSourceProjectDelta  `json:"sourceProjectDelta,omitempty"`
	Attachments             []localAgentInputArtifact `json:"attachments,omitempty"`
}

type localAgentInputArtifact struct {
	ArtifactID string `json:"artifactId"`
	SHA256     string `json:"sha256"`
	ByteSize   int64  `json:"byteSize"`
	FileName   string `json:"fileName"`
	MediaType  string `json:"mediaType"`
	FilePath   string `json:"filePath,omitempty"`
}

type localSourceProjectDelta struct {
	ArtifactID              string                      `json:"artifactId"`
	SHA256                  string                      `json:"sha256"`
	ByteSize                int64                       `json:"byteSize"`
	BaseCommit              string                      `json:"baseCommit"`
	Commit                  string                      `json:"commit"`
	FilePath                string                      `json:"filePath,omitempty"`
	LFSObjects              *localSourceProjectArtifact `json:"lfsObjects,omitempty"`
	LFSFilePath             string                      `json:"lfsFilePath,omitempty"`
	SubmoduleDeltas         *localSourceProjectArtifact `json:"submoduleDeltas,omitempty"`
	SubmoduleDeltasFilePath string                      `json:"submoduleDeltasFilePath,omitempty"`
}

type localSourceProjectArtifact struct {
	ArtifactID string `json:"artifactId"`
	SHA256     string `json:"sha256"`
	ByteSize   int64  `json:"byteSize"`
}

type localAgentTaskResult struct {
	SourceConversationID       string          `json:"sourceConversationId"`
	ProjectID                  string          `json:"projectId,omitempty"`
	ProjectCommit              string          `json:"projectCommit,omitempty"`
	ProjectDeltaPath           string          `json:"projectDeltaPath,omitempty"`
	ProjectLFSPath             string          `json:"projectLfsPath,omitempty"`
	ProjectSubmoduleDeltasPath string          `json:"projectSubmoduleDeltasPath,omitempty"`
	ProjectDeltaBase           string          `json:"projectDeltaBaseCommit,omitempty"`
	ProjectDeltaCommit         string          `json:"projectDeltaCommit,omitempty"`
	ConversationID             string          `json:"conversationId"`
	AgentTurnID                string          `json:"agentTurnId"`
	ResultMessageID            string          `json:"resultMessageId"`
	AgentStatus                string          `json:"agentStatus"`
	AgentStopReason            string          `json:"agentStopReason,omitempty"`
	AssistantText              string          `json:"assistantText"`
	Transcript                 json.RawMessage `json:"transcript"`
	HandoffCheckpoint          json.RawMessage `json:"handoffCheckpoint,omitempty"`
}

type localAgentTranscript struct {
	Conversation domain.ConversationDetail `json:"conversation"`
	Project      *domain.Project           `json:"project,omitempty"`
	Turns        []domain.AgentTurn        `json:"turns"`
	Trace        []domain.TraceEvent       `json:"trace"`
}

func removeDownloadedTaskArtifacts(paths ...string) error {
	var cleanupErr error
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		removeErr := os.Remove(path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		if removeErr == nil {
			if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
				if statErr == nil {
					statErr = errors.New("downloaded task artifact remains after cleanup")
				}
				removeErr = statErr
			}
		}
		if removeErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove downloaded task artifact %q: %w", path, removeErr))
		}
	}
	return cleanupErr
}

func NewLocalAgentExecutor(runtime *agent.Service, providers *provider.Service, workspaceID, providerID string) (*LocalAgentExecutor, error) {
	if runtime == nil || providers == nil || strings.TrimSpace(workspaceID) == "" {
		return nil, domain.ErrInvalid
	}
	return &LocalAgentExecutor{agent: runtime, providers: providers, workspace: workspaceID, providerID: strings.TrimSpace(providerID), pauseActive: map[string]*atomic.Bool{}, pausePending: map[string]struct{}{}}, nil
}

func (e *LocalAgentExecutor) PrepareNodeTask(ctx context.Context, task storage.ExecutionTask, client *nodeAPIClient, lease string) (prepared storage.ExecutionTask, retErr error) {
	var input localAgentPrompt
	decoder := json.NewDecoder(strings.NewReader(string(task.Payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Kind != "agent_prompt" || input.SourceContextArtifactID == "" || len(input.SourceContextSHA256) != 64 || input.SourceContextByteSize < 0 {
		return storage.ExecutionTask{}, domain.ErrInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return storage.ExecutionTask{}, domain.ErrInvalid
	}
	path, err := client.downloadTaskArtifact(ctx, task.ID, lease, input.SourceContextArtifactID, input.SourceContextSHA256, input.SourceContextByteSize)
	if err != nil {
		return storage.ExecutionTask{}, fmt.Errorf("download authoritative source conversation context: %w", err)
	}
	input.SourceContextFilePath = path
	downloadedPaths := []string{path}
	if input.SourceContextByteSize == 0 {
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
			cleanupErr := removeDownloadedTaskArtifacts(path)
			return storage.ExecutionTask{}, errors.Join(fmt.Errorf("downloaded conversation context has no verified byte size: %w", domain.ErrConflict), statErr, cleanupErr)
		}
		input.SourceContextByteSize = info.Size()
	}
	if input.SourceProjectDelta != nil {
		delta := input.SourceProjectDelta
		if delta.ArtifactID == "" || len(delta.SHA256) != 64 || delta.ByteSize <= 0 || !validCommitSHA(delta.BaseCommit) || !validCommitSHA(delta.Commit) || strings.EqualFold(delta.BaseCommit, delta.Commit) {
			return storage.ExecutionTask{}, errors.Join(fmt.Errorf("source project workspace delta metadata is incomplete: %w", domain.ErrInvalid), removeDownloadedTaskArtifacts(path))
		}
		deltaPath, err := client.downloadTaskArtifact(ctx, task.ID, lease, delta.ArtifactID, delta.SHA256, delta.ByteSize)
		if err != nil {
			return storage.ExecutionTask{}, errors.Join(fmt.Errorf("download verified cloud project workspace delta: %w", err), removeDownloadedTaskArtifacts(path))
		}
		delta.FilePath = deltaPath
		downloadedPaths = append(downloadedPaths, deltaPath)
		if delta.LFSObjects != nil {
			if delta.LFSObjects.ArtifactID == "" || len(delta.LFSObjects.SHA256) != 64 || delta.LFSObjects.ByteSize <= 0 {
				return storage.ExecutionTask{}, errors.Join(fmt.Errorf("source project Git LFS archive metadata is incomplete: %w", domain.ErrInvalid), removeDownloadedTaskArtifacts(path, deltaPath))
			}
			lfsPath, err := client.downloadTaskArtifact(ctx, task.ID, lease, delta.LFSObjects.ArtifactID, delta.LFSObjects.SHA256, delta.LFSObjects.ByteSize)
			if err != nil {
				return storage.ExecutionTask{}, errors.Join(fmt.Errorf("download verified cloud Git LFS archive: %w", err), removeDownloadedTaskArtifacts(path, deltaPath))
			}
			delta.LFSFilePath = lfsPath
			downloadedPaths = append(downloadedPaths, lfsPath)
		}
		if delta.SubmoduleDeltas != nil {
			if delta.SubmoduleDeltas.ArtifactID == "" || len(delta.SubmoduleDeltas.SHA256) != 64 || delta.SubmoduleDeltas.ByteSize <= 0 {
				return storage.ExecutionTask{}, errors.Join(fmt.Errorf("source project submodule delta metadata is incomplete: %w", domain.ErrInvalid), removeDownloadedTaskArtifacts(path, deltaPath, delta.LFSFilePath))
			}
			submodulePath, err := client.downloadTaskArtifact(ctx, task.ID, lease, delta.SubmoduleDeltas.ArtifactID, delta.SubmoduleDeltas.SHA256, delta.SubmoduleDeltas.ByteSize)
			if err != nil {
				return storage.ExecutionTask{}, errors.Join(fmt.Errorf("download verified cloud submodule delta archive: %w", err), removeDownloadedTaskArtifacts(path, deltaPath, delta.LFSFilePath))
			}
			delta.SubmoduleDeltasFilePath = submodulePath
			downloadedPaths = append(downloadedPaths, submodulePath)
		}
	}
	for index := range input.Attachments {
		attachment := &input.Attachments[index]
		if strings.TrimSpace(attachment.ArtifactID) == "" || len(attachment.SHA256) != 64 || attachment.ByteSize < 0 || strings.TrimSpace(attachment.FileName) == "" {
			return storage.ExecutionTask{}, errors.Join(fmt.Errorf("user input attachment manifest is invalid: %w", domain.ErrInvalid), removeDownloadedTaskArtifacts(downloadedPaths...))
		}
		path, err := client.downloadTaskArtifact(ctx, task.ID, lease, attachment.ArtifactID, attachment.SHA256, attachment.ByteSize)
		if err != nil {
			return storage.ExecutionTask{}, errors.Join(fmt.Errorf("download verified user input attachment: %w", err), removeDownloadedTaskArtifacts(downloadedPaths...))
		}
		attachment.FilePath = path
		downloadedPaths = append(downloadedPaths, path)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return storage.ExecutionTask{}, errors.Join(err, removeDownloadedTaskArtifacts(downloadedPaths...))
	}
	task.Payload = payload
	return task, nil
}

func (e *LocalAgentExecutor) ExecuteNodeTask(ctx context.Context, task storage.ExecutionTask) (json.RawMessage, error) {
	return e.executeNodeTask(ctx, task, "", nil)
}

func (e *LocalAgentExecutor) ExecuteLeasedNodeTask(ctx context.Context, task storage.ExecutionTask, lease string) (json.RawMessage, error) {
	pause := &atomic.Bool{}
	e.pauseMu.Lock()
	if e.pauseActive == nil {
		e.pauseActive = map[string]*atomic.Bool{}
	}
	if e.pausePending == nil {
		e.pausePending = map[string]struct{}{}
	}
	if _, requested := e.pausePending[task.ID]; requested {
		pause.Store(true)
		delete(e.pausePending, task.ID)
	}
	e.pauseActive[task.ID] = pause
	e.pauseMu.Unlock()
	defer func() {
		e.pauseMu.Lock()
		delete(e.pauseActive, task.ID)
		delete(e.pausePending, task.ID)
		e.pauseMu.Unlock()
	}()
	return e.executeNodeTask(ctx, task, lease, pause.Load)
}

// RequestSafePause is idempotent. The worker can receive a handoff pulse
// before the task executor has registered its callback.
func (e *LocalAgentExecutor) RequestSafePause(taskID string) error {
	if e == nil || strings.TrimSpace(taskID) == "" {
		return domain.ErrInvalid
	}
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	if e.pauseActive == nil {
		e.pauseActive = map[string]*atomic.Bool{}
	}
	if e.pausePending == nil {
		e.pausePending = map[string]struct{}{}
	}
	if active := e.pauseActive[taskID]; active != nil {
		active.Store(true)
	} else {
		e.pausePending[taskID] = struct{}{}
	}
	return nil
}

func (e *LocalAgentExecutor) executeNodeTask(ctx context.Context, task storage.ExecutionTask, lease string, shouldPause func() bool) (result json.RawMessage, retErr error) {
	var input localAgentPrompt
	decoder := json.NewDecoder(strings.NewReader(string(task.Payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Kind != "agent_prompt" || strings.TrimSpace(input.SourceConversationID) == "" || strings.TrimSpace(input.Content) == "" || len(input.Content) > 100000 {
		return nil, domain.ErrInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, domain.ErrInvalid
	}
	if input.SourceContextFilePath == "" || input.SourceContextArtifactID == "" || len(input.SourceContextSHA256) != 64 || input.SourceContextByteSize < 0 {
		return nil, domain.ErrInvalid
	}
	cleanupPaths := []string{input.SourceContextFilePath}
	for _, attachment := range input.Attachments {
		if attachment.FilePath == "" {
			return nil, fmt.Errorf("task input attachment was not downloaded: %w", domain.ErrInvalid)
		}
		cleanupPaths = append(cleanupPaths, attachment.FilePath)
	}
	if input.SourceProjectDelta != nil {
		if input.SourceProjectDelta.FilePath == "" || input.SourceProjectDelta.ArtifactID == "" || len(input.SourceProjectDelta.SHA256) != 64 || input.SourceProjectDelta.ByteSize <= 0 || !validCommitSHA(input.SourceProjectDelta.BaseCommit) || !validCommitSHA(input.SourceProjectDelta.Commit) {
			return nil, fmt.Errorf("source project workspace delta metadata is invalid: %w", domain.ErrInvalid)
		}
		cleanupPaths = append(cleanupPaths, input.SourceProjectDelta.FilePath)
		if input.SourceProjectDelta.LFSObjects != nil {
			if input.SourceProjectDelta.LFSFilePath == "" {
				return nil, fmt.Errorf("source Git LFS artifact was not downloaded: %w", domain.ErrInvalid)
			}
			cleanupPaths = append(cleanupPaths, input.SourceProjectDelta.LFSFilePath)
		}
		if input.SourceProjectDelta.SubmoduleDeltas != nil {
			if input.SourceProjectDelta.SubmoduleDeltasFilePath == "" {
				return nil, fmt.Errorf("source submodule delta artifact was not downloaded: %w", domain.ErrInvalid)
			}
			cleanupPaths = append(cleanupPaths, input.SourceProjectDelta.SubmoduleDeltasFilePath)
		}
	}
	defer func() { retErr = errors.Join(retErr, removeDownloadedTaskArtifacts(cleanupPaths...)) }()
	contextFile, err := os.Open(input.SourceContextFilePath)
	if err != nil {
		return nil, fmt.Errorf("open verified source conversation context: %w", err)
	}
	var sourceContext localAgentTranscript
	contextDecoder := json.NewDecoder(contextFile)
	contextDecodeErr := contextDecoder.Decode(&sourceContext)
	if contextDecodeErr == nil {
		var trailing any
		if err := contextDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				contextDecodeErr = errors.New("source conversation context contains trailing JSON")
			} else {
				contextDecodeErr = err
			}
		}
	}
	closeErr := contextFile.Close()
	if contextDecodeErr != nil {
		return nil, fmt.Errorf("decode source conversation context: %w", contextDecodeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close source conversation context: %w", closeErr)
	}
	if sourceContext.Conversation.ID != input.SourceConversationID {
		return nil, domain.ErrConflict
	}
	if sourceContext.Conversation.ProjectID == "" && sourceContext.Project != nil {
		return nil, fmt.Errorf("source conversation context contains unrelated project metadata: %w", domain.ErrConflict)
	}
	if sourceContext.Conversation.ProjectID != "" && (sourceContext.Project == nil || sourceContext.Project.ID != sourceContext.Conversation.ProjectID) {
		return nil, fmt.Errorf("source project snapshot is missing or does not match the conversation: %w", domain.ErrConflict)
	}
	validTurns := make(map[string]bool, len(sourceContext.Turns))
	for _, sourceTurn := range sourceContext.Turns {
		if sourceTurn.ID == "" || sourceTurn.ConversationID != input.SourceConversationID {
			return nil, domain.ErrConflict
		}
		validTurns[sourceTurn.ID] = true
	}
	for _, event := range sourceContext.Trace {
		if event.ConversationID != input.SourceConversationID || (event.TurnID != "" && !validTurns[event.TurnID]) {
			return nil, domain.ErrConflict
		}
	}
	for _, message := range sourceContext.Conversation.Messages {
		if message.ConversationID != input.SourceConversationID || (message.Role != "user" && message.Role != "assistant") {
			return nil, domain.ErrConflict
		}
	}
	if strings.TrimSpace(input.Title) == "" {
		input.Title = "远程本地任务"
	}
	configured, err := e.providers.List(ctx, e.workspace)
	if err != nil {
		return nil, fmt.Errorf("list local model providers: %w", err)
	}
	providerID := e.providerID
	if providerID == "" && len(configured) == 1 {
		providerID = configured[0].ID
	}
	providerAvailable := false
	for _, candidate := range configured {
		if candidate.ID == providerID {
			providerAvailable = true
			break
		}
	}
	if !providerAvailable {
		return nil, errors.New("local provider selection is ambiguous or unavailable; configure O_NODE_PROVIDER_ID or leave exactly one local provider configured")
	}
	var localProject domain.Project
	projectID, projectCommit, sourceProjectID := "", "", ""
	sourceOverlay := input.SourceProjectDelta != nil
	if sourceContext.Project != nil {
		projectLockValue, _ := e.projectLocks.LoadOrStore(sourceContext.Project.ID, &sync.Mutex{})
		projectLock := projectLockValue.(*sync.Mutex)
		projectLock.Lock()
		defer projectLock.Unlock()
		localProject, err = e.agent.PrepareProjectSnapshot(ctx, e.workspace, *sourceContext.Project)
		if err != nil {
			return nil, fmt.Errorf("prepare exact cloud project commit on local node: %w", err)
		}
		projectID, projectCommit, sourceProjectID = localProject.ID, sourceContext.Project.ResolvedCommit, sourceContext.Project.ID
		if sourceOverlay {
			delta := input.SourceProjectDelta
			if delta.BaseCommit != projectCommit {
				return nil, fmt.Errorf("cloud workspace delta does not descend from the conversation's pinned project commit: %w", domain.ErrConflict)
			}
			file, err := os.Open(delta.FilePath)
			if err != nil {
				return nil, fmt.Errorf("open downloaded cloud project workspace delta: %w", err)
			}
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() || info.Size() != delta.ByteSize {
				closeErr := file.Close()
				return nil, errors.Join(fmt.Errorf("downloaded cloud project delta size is inconsistent: %w", domain.ErrConflict), statErr, closeErr)
			}
			hasher := sha256.New()
			_, hashErr := io.Copy(hasher, file)
			_, seekErr := file.Seek(0, io.SeekStart)
			if hashErr != nil || seekErr != nil {
				closeErr := file.Close()
				return nil, errors.Join(fmt.Errorf("verify downloaded cloud project delta: %w", errors.Join(hashErr, seekErr)), closeErr)
			}
			if hex.EncodeToString(hasher.Sum(nil)) != strings.ToLower(delta.SHA256) {
				closeErr := file.Close()
				return nil, errors.Join(domain.ErrConflict, closeErr)
			}
			var lfsReader io.Reader
			var lfsSize int64
			var lfsSHA string
			var lfsFile *os.File
			if delta.LFSObjects != nil {
				lfsFile, err = os.Open(delta.LFSFilePath)
				if err != nil {
					return nil, errors.Join(fmt.Errorf("open downloaded cloud Git LFS archive: %w", err), file.Close())
				}
				lfsInfo, statErr := lfsFile.Stat()
				if statErr != nil || !lfsInfo.Mode().IsRegular() || lfsInfo.Size() != delta.LFSObjects.ByteSize {
					return nil, errors.Join(fmt.Errorf("downloaded cloud Git LFS archive size is inconsistent: %w", domain.ErrConflict), statErr, lfsFile.Close(), file.Close())
				}
				lfsHash := sha256.New()
				_, hashErr := io.Copy(lfsHash, lfsFile)
				_, seekErr := lfsFile.Seek(0, io.SeekStart)
				if hashErr != nil || seekErr != nil || hex.EncodeToString(lfsHash.Sum(nil)) != strings.ToLower(delta.LFSObjects.SHA256) {
					return nil, errors.Join(fmt.Errorf("verify downloaded cloud Git LFS archive: %w", errors.Join(hashErr, seekErr, domain.ErrConflict)), lfsFile.Close(), file.Close())
				}
				lfsReader, lfsSize, lfsSHA = lfsFile, lfsInfo.Size(), delta.LFSObjects.SHA256
			}
			var submoduleReader io.Reader
			var submoduleSize int64
			var submoduleSHA string
			var submoduleFile *os.File
			if delta.SubmoduleDeltas != nil {
				submoduleFile, err = os.Open(delta.SubmoduleDeltasFilePath)
				if err != nil {
					if lfsFile != nil {
						_ = lfsFile.Close()
					}
					return nil, errors.Join(fmt.Errorf("open downloaded cloud submodule delta archive: %w", err), file.Close())
				}
				submoduleInfo, statErr := submoduleFile.Stat()
				if statErr != nil || !submoduleInfo.Mode().IsRegular() || submoduleInfo.Size() != delta.SubmoduleDeltas.ByteSize {
					return nil, errors.Join(fmt.Errorf("downloaded cloud submodule archive size is inconsistent: %w", domain.ErrConflict), statErr, submoduleFile.Close(), file.Close())
				}
				submoduleHash := sha256.New()
				_, hashErr := io.Copy(submoduleHash, submoduleFile)
				_, seekErr := submoduleFile.Seek(0, io.SeekStart)
				if hashErr != nil || seekErr != nil || hex.EncodeToString(submoduleHash.Sum(nil)) != strings.ToLower(delta.SubmoduleDeltas.SHA256) {
					return nil, errors.Join(fmt.Errorf("verify downloaded cloud submodule delta archive: %w", errors.Join(hashErr, seekErr, domain.ErrConflict)), submoduleFile.Close(), file.Close())
				}
				submoduleReader, submoduleSize, submoduleSHA = submoduleFile, submoduleInfo.Size(), delta.SubmoduleDeltas.SHA256
			}
			localProject, _, err = e.agent.ImportProjectDeltaBundleWithArtifacts(ctx, e.workspace, localProject, task.ID, file, delta.ByteSize, delta.SHA256, delta.BaseCommit, delta.Commit, lfsReader, lfsSize, lfsSHA, submoduleReader, submoduleSize, submoduleSHA)
			closeErr := file.Close()
			var lfsCloseErr error
			if lfsFile != nil {
				lfsCloseErr = lfsFile.Close()
			}
			var submoduleCloseErr error
			if submoduleFile != nil {
				submoduleCloseErr = submoduleFile.Close()
			}
			if err != nil || closeErr != nil || lfsCloseErr != nil || submoduleCloseErr != nil {
				return nil, errors.Join(fmt.Errorf("prepare task-local Git, LFS and submodule project snapshot: %w", err), closeErr, lfsCloseErr, submoduleCloseErr)
			}
			projectID = localProject.ID
		}
	} else if sourceOverlay {
		return nil, fmt.Errorf("task has a cloud project delta without a source project: %w", domain.ErrConflict)
	}
	inputWorkspace := e.agent.WorkspaceRoot()
	if localProject.Workdir != "" {
		inputWorkspace = localProject.Workdir
	}
	inputDir, inputPrompt, err := stageNodeTaskInputs(task.ID, inputWorkspace, input.Attachments)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("stage task input artifacts in the local workspace: %w", err), removeNodeTaskInputDirectory(inputDir, inputWorkspace))
	}
	input.Content += inputPrompt
	conversation, err := e.agent.CreateWithProject(ctx, e.workspace, input.Title, providerID, projectID)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create local task conversation: %w", err), removeNodeTaskInputDirectory(inputDir, inputWorkspace))
	}
	if err := e.agent.ImportConversationContext(ctx, e.workspace, conversation.ID, sourceContext.Conversation.Messages); err != nil {
		return nil, errors.Join(fmt.Errorf("import cloud conversation context into local task: %w", err), removeNodeTaskInputDirectory(inputDir, inputWorkspace))
	}
	var receipt domain.TurnReceipt
	var turn domain.AgentTurn
	var message domain.Message
	var runErr error
	if shouldPause != nil {
		receipt, turn, message, runErr = e.agent.ExecuteTaskTurnWithPause(ctx, e.workspace, conversation.ID, input.Content, shouldPause)
	} else {
		receipt, turn, message, runErr = e.agent.ExecuteTaskTurn(ctx, e.workspace, conversation.ID, input.Content)
	}
	if cleanupErr := removeNodeTaskInputDirectory(inputDir, inputWorkspace); cleanupErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("remove temporary local task inputs after execution: %w", cleanupErr))
	}
	continuable := false
	var encryptedHandoffCheckpoint json.RawMessage
	var handoffCheckpoint *agent.HandoffCheckpoint
	var checkpointValidationErr error
	if turn.Status == "incomplete" && turn.ContinuationAvailable && safeLocalHandoffStopReason(turn.StopReason) && receipt.TurnID == turn.ID && receipt.ConversationID == conversation.ID && message.ID != "" && message.ID == turn.ResultMessageID {
		continuable = true
		if !sourceOverlay {
			checkpoint, exportErr := e.agent.ExportContinuationCheckpoint(ctx, e.workspace, turn.ID)
			if exportErr == nil {
				handoffCheckpoint = &checkpoint
			} else {
				continuable = false
				checkpointValidationErr = fmt.Errorf("validate local continuation checkpoint: %w", exportErr)
			}
		}
	}
	if (runErr != nil && !continuable) || turn.Status == "needs_reconciliation" {
		reason := "local Agent turn has an unresolved effect and requires reconciliation"
		if runErr != nil {
			reason = runErr.Error()
		}
		if checkpointValidationErr != nil {
			reason = errors.Join(errors.New(reason), checkpointValidationErr).Error()
		}
		return nil, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: errors.New(reason)}
	}
	if turn.Status == "cancelled" {
		return nil, &NodeTaskOutcomeError{Status: "cancelled", Err: errors.New("local Agent turn was cancelled and read back as cancelled")}
	}
	if receipt.TurnID == "" || receipt.ConversationID != conversation.ID || turn.ID != receipt.TurnID || turn.ConversationID != conversation.ID || (turn.Status != "completed" && !continuable) || message.ID == "" || message.ID != turn.ResultMessageID {
		return nil, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: errors.New("local Agent turn did not read back as a matching completed or safely resumable result")}
	}
	var projectDelta agent.ProjectDeltaBundle
	if localProject.ID != "" {
		deltaBaseProject := localProject
		deltaBaseProject.ResolvedCommit = projectCommit
		projectDelta, err = e.agent.CreateProjectDeltaBundle(ctx, deltaBaseProject, task.ID)
		if err != nil {
			return nil, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: fmt.Errorf("local Agent project delta could not be verified: %w", err)}
		}
	}
	if handoffCheckpoint != nil {
		if lease != "" && projectDelta.Path == "" {
			encryptedHandoffCheckpoint, checkpointValidationErr = sealNodeHandoffCheckpoint(task.ID, task.NodeID, lease, *handoffCheckpoint)
			if checkpointValidationErr != nil {
				checkpointValidationErr = fmt.Errorf("encrypt local continuation checkpoint for its task lease: %w", checkpointValidationErr)
			}
		}
		for index := range handoffCheckpoint.Checkpoint {
			handoffCheckpoint.Checkpoint[index] = 0
		}
	}
	detail, err := e.agent.Get(ctx, e.workspace, conversation.ID)
	if err != nil {
		return nil, fmt.Errorf("read local conversation after task: %w", err)
	}
	found := false
	for _, persisted := range detail.Messages {
		if persisted.ID == message.ID && persisted.Role == "assistant" && persisted.Content == message.Content {
			found = true
			break
		}
	}
	if !found || detail.ID != conversation.ID || detail.UserID != e.workspace {
		return nil, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: errors.New("local assistant message did not read back from the conversation store")}
	}
	turns, err := e.agent.Turns(ctx, e.workspace, conversation.ID)
	if err != nil {
		return nil, fmt.Errorf("read local Agent turns after task: %w", err)
	}
	turnFound := false
	for _, persisted := range turns {
		if persisted.ID == turn.ID && persisted.ConversationID == conversation.ID && persisted.Status == turn.Status && persisted.ResultMessageID == message.ID && (persisted.Status == "completed" || (continuable && persisted.ContinuationAvailable && safeLocalHandoffStopReason(persisted.StopReason))) {
			turnFound = true
			break
		}
	}
	if !turnFound {
		return nil, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: errors.New("Agent turn did not read back as the reported completed or safely resumable state")}
	}
	trace, err := e.agent.Trace(ctx, e.workspace, conversation.ID)
	if err != nil {
		return nil, fmt.Errorf("read local Agent trace after task: %w", err)
	}
	for _, event := range trace {
		if event.ConversationID != conversation.ID || event.TurnID != turn.ID {
			return nil, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: errors.New("local Agent trace contains an event outside the task turn")}
		}
	}
	transcriptProject := sourceContext.Project
	if sourceOverlay && sourceContext.Project != nil {
		localTranscriptProject := *sourceContext.Project
		localTranscriptProject.ID = localProject.ID
		localTranscriptProject.RemoteBranch = localProject.RemoteBranch
		localTranscriptProject.ResolvedCommit = localProject.ResolvedCommit
		localTranscriptProject.Workdir = ""
		transcriptProject = &localTranscriptProject
	}
	transcript, err := json.Marshal(localAgentTranscript{Conversation: detail, Project: transcriptProject, Turns: turns, Trace: trace})
	if err != nil {
		return nil, fmt.Errorf("serialize local conversation transcript: %w", err)
	}
	if detail.ProjectID != projectID {
		return nil, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: errors.New("local task conversation did not read back with the requested project binding")}
	}
	deltaBase, deltaCommit := "", ""
	if projectDelta.Path != "" {
		deltaBase, deltaCommit = projectDelta.BaseCommit, projectDelta.Commit
	}
	reportProjectID := sourceProjectID
	if reportProjectID == "" {
		reportProjectID = projectID
	}
	resultJSON, err := json.Marshal(localAgentTaskResult{SourceConversationID: input.SourceConversationID, ProjectID: reportProjectID, ProjectCommit: projectCommit, ProjectDeltaPath: projectDelta.Path, ProjectLFSPath: projectDelta.LFSObjectsPath, ProjectSubmoduleDeltasPath: projectDelta.SubmoduleDeltasPath, ProjectDeltaBase: deltaBase, ProjectDeltaCommit: deltaCommit, ConversationID: conversation.ID, AgentTurnID: turn.ID, ResultMessageID: message.ID, AgentStatus: turn.Status, AgentStopReason: turn.StopReason, AssistantText: message.Content, Transcript: transcript, HandoffCheckpoint: encryptedHandoffCheckpoint})
	if err != nil {
		return nil, fmt.Errorf("serialize local task result: %w", err)
	}
	return resultJSON, nil
}

func safeLocalHandoffStopReason(reason string) bool {
	return domain.SafeAgentContinuationStopReason(reason)
}
