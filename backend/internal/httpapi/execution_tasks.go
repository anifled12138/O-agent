package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

type localTaskConversationContext struct {
	Conversation domain.ConversationDetail `json:"conversation"`
	Project      *domain.Project           `json:"project,omitempty"`
	Turns        []domain.AgentTurn        `json:"turns"`
	Trace        []domain.TraceEvent       `json:"trace"`
}

type nodeAgentTaskProjectDelta struct {
	ArtifactID      string                 `json:"artifactId"`
	SHA256          string                 `json:"sha256"`
	ByteSize        int64                  `json:"byteSize"`
	BaseCommit      string                 `json:"baseCommit"`
	Commit          string                 `json:"commit"`
	LFSObjects      *nodeAgentTaskArtifact `json:"lfsObjects,omitempty"`
	SubmoduleDeltas *nodeAgentTaskArtifact `json:"submoduleDeltas,omitempty"`
}

type nodeAgentTaskArtifact struct {
	ArtifactID string `json:"artifactId"`
	SHA256     string `json:"sha256"`
	ByteSize   int64  `json:"byteSize"`
}

type taskInputArtifact struct {
	ArtifactID string `json:"artifactId"`
	SHA256     string `json:"sha256"`
	ByteSize   int64  `json:"byteSize"`
	FileName   string `json:"fileName"`
	MediaType  string `json:"mediaType"`
}

type nodeAgentTaskPayload struct {
	Kind                    string                     `json:"kind"`
	SourceConversationID    string                     `json:"sourceConversationId"`
	Content                 string                     `json:"content"`
	Title                   string                     `json:"title"`
	SourceContextArtifactID string                     `json:"sourceContextArtifactId"`
	SourceContextSHA256     string                     `json:"sourceContextSHA256"`
	SourceContextByteSize   int64                      `json:"sourceContextByteSize"`
	SourceProjectDelta      *nodeAgentTaskProjectDelta `json:"sourceProjectDelta,omitempty"`
	Attachments             []taskInputArtifact        `json:"attachments,omitempty"`
}

func (s *Server) resolveTaskInputArtifacts(ctx context.Context, artifactIDs []string) ([]taskInputArtifact, []storage.ExecutionTaskArtifactLink, error) {
	if len(artifactIDs) > 128 {
		return nil, nil, fmt.Errorf("a task can include at most 128 file attachments: %w", domain.ErrInvalid)
	}
	if len(artifactIDs) > 0 && s.artifacts == nil {
		return nil, nil, fmt.Errorf("task input artifact storage is unavailable")
	}
	refs := make([]taskInputArtifact, 0, len(artifactIDs))
	links := make([]storage.ExecutionTaskArtifactLink, 0, len(artifactIDs))
	seen := make(map[string]struct{}, len(artifactIDs))
	for _, rawID := range artifactIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return nil, nil, domain.ErrInvalid
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		artifact, file, err := s.artifacts.Open(ctx, s.workspaceID, id)
		if err != nil {
			return nil, nil, fmt.Errorf("verify task input artifact %q: %w", id, err)
		}
		closeErr := file.Close()
		if closeErr != nil {
			return nil, nil, fmt.Errorf("close verified task input artifact %q: %w", id, closeErr)
		}
		if artifact.ID != id || artifact.ByteSize < 0 || len(artifact.SHA256) != 64 || strings.TrimSpace(artifact.FileName) == "" {
			return nil, nil, fmt.Errorf("task input artifact manifest did not read back: %w", domain.ErrConflict)
		}
		refs = append(refs, taskInputArtifact{ArtifactID: artifact.ID, SHA256: artifact.SHA256, ByteSize: artifact.ByteSize, FileName: artifact.FileName, MediaType: artifact.MediaType})
		links = append(links, storage.ExecutionTaskArtifactLink{ArtifactID: artifact.ID, Role: "user_input"})
	}
	return refs, links, nil
}

func sameTaskInputArtifacts(left, right []taskInputArtifact) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *Server) verifyTaskInputArtifactLinks(ctx context.Context, taskID string, expected []taskInputArtifact) error {
	items, err := s.store.ExecutionTaskArtifacts(ctx, s.workspaceID, taskID)
	if err != nil {
		return err
	}
	actual := make(map[string]storage.ExecutionTaskArtifact, len(expected))
	for _, item := range items {
		if item.Role == "user_input" {
			actual[item.ID] = item
		}
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("task input artifact links did not read back: %w", domain.ErrConflict)
	}
	for _, want := range expected {
		got, ok := actual[want.ArtifactID]
		if !ok || got.SHA256 != want.SHA256 || got.ByteSize != want.ByteSize || got.FileName != want.FileName || got.MediaType != want.MediaType {
			return fmt.Errorf("task input artifact %q did not read back: %w", want.ArtifactID, domain.ErrConflict)
		}
	}
	return nil
}

func (s *Server) executionTaskSubmit(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TargetNodeID string          `json:"targetNodeId"`
		Payload      json.RawMessage `json:"payload"`
		WaitForNode  bool            `json:"waitForNode"`
	}
	if !decode(w, r, &input) {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 || strings.TrimSpace(input.TargetNodeID) == "" || len(input.Payload) == 0 || !json.Valid(input.Payload) || string(input.Payload) == "null" || input.Payload[0] != '{' {
		fail(w, domain.ErrInvalid)
		return
	}
	rawID := make([]byte, 16)
	if _, err := rand.Read(rawID); err != nil {
		fail(w, err)
		return
	}
	task, created, err := s.store.CreateExecutionTask(r.Context(), storage.ExecutionTask{ID: "task_" + hex.EncodeToString(rawID), UserID: s.workspaceID, NodeID: input.TargetNodeID, IdempotencyKey: key, Payload: input.Payload}, input.WaitForNode, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionTask(r.Context(), s.workspaceID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.NodeID != input.TargetNodeID || string(readBack.Payload) != string(input.Payload) {
		write(w, http.StatusConflict, map[string]string{"error": "task submission idempotency read-back does not match the submitted task"})
		return
	}
	s.notifyExecutionNode(readBack.NodeID)
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	write(w, status, map[string]any{"task": readBack, "created": created})
}

// cloudAgentTaskSubmit provides the user-facing bridge from a conversation to
// the durable cloud execution queue. Completion remains the worker's job and
// is reported only after the Agent turn has been read back from storage.
func (s *Server) cloudAgentTaskSubmit(w http.ResponseWriter, r *http.Request) {
	conversationID := strings.TrimSpace(r.PathValue("id"))
	var input struct {
		Content     string   `json:"content"`
		ArtifactIDs []string `json:"artifactIds,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Content = strings.TrimSpace(input.Content)
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if conversationID == "" || input.Content == "" || len(input.Content) > 100000 || key == "" || len(key) > 128 {
		fail(w, domain.ErrInvalid)
		return
	}
	conversation, err := s.store.Conversation(r.Context(), s.workspaceID, conversationID)
	if err != nil {
		fail(w, err)
		return
	}
	if conversation.ID != conversationID || conversation.UserID != s.workspaceID {
		write(w, http.StatusConflict, map[string]string{"error": "conversation ownership did not read back"})
		return
	}
	attachments, links, err := s.resolveTaskInputArtifacts(r.Context(), input.ArtifactIDs)
	if err != nil {
		fail(w, err)
		return
	}
	payload, err := json.Marshal(struct {
		Kind           string              `json:"kind"`
		ConversationID string              `json:"conversationId"`
		Content        string              `json:"content"`
		Attachments    []taskInputArtifact `json:"attachments,omitempty"`
	}{"agent_turn", conversationID, input.Content, attachments})
	if err != nil {
		fail(w, err)
		return
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		fail(w, err)
		return
	}
	task, created, err := s.store.CreateExecutionTaskWithArtifacts(r.Context(), storage.ExecutionTask{
		ID: "task_" + hex.EncodeToString(id), UserID: s.workspaceID,
		NodeID: storage.CloudExecutionNodeID(s.workspaceID), IdempotencyKey: key, Payload: payload,
	}, false, links, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionTask(r.Context(), s.workspaceID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.NodeID != storage.CloudExecutionNodeID(s.workspaceID) || string(readBack.Payload) != string(payload) {
		write(w, http.StatusConflict, map[string]string{"error": "cloud task submission did not read back as requested"})
		return
	}
	if err := s.verifyTaskInputArtifactLinks(r.Context(), readBack.ID, attachments); err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	write(w, status, map[string]any{"task": readBack, "created": created})
}

// nodeAgentTaskSubmit snapshots the source conversation, atomically attaches
// that context to an independent local Agent task, and routes it to one
// explicitly selected paired computer.
func (s *Server) nodeAgentTaskSubmit(w http.ResponseWriter, r *http.Request) {
	conversationID := strings.TrimSpace(r.PathValue("id"))
	var input struct {
		TargetNodeID string   `json:"targetNodeId"`
		Content      string   `json:"content"`
		WaitForNode  bool     `json:"waitForNode"`
		ArtifactIDs  []string `json:"artifactIds,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.TargetNodeID = strings.TrimSpace(input.TargetNodeID)
	input.Content = strings.TrimSpace(input.Content)
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if conversationID == "" || input.TargetNodeID == "" || input.Content == "" || len(input.Content) > 100000 || key == "" || len(key) > 128 {
		fail(w, domain.ErrInvalid)
		return
	}
	conversation, err := s.store.Conversation(r.Context(), s.workspaceID, conversationID)
	if err != nil {
		fail(w, err)
		return
	}
	if conversation.ID != conversationID || conversation.UserID != s.workspaceID {
		write(w, http.StatusConflict, map[string]string{"error": "source conversation ownership did not read back"})
		return
	}
	attachments, attachmentLinks, err := s.resolveTaskInputArtifacts(r.Context(), input.ArtifactIDs)
	if err != nil {
		fail(w, err)
		return
	}
	if existing, lookupErr := s.store.ExecutionTaskByIdempotencyKey(r.Context(), s.workspaceID, key); lookupErr == nil {
		var saved nodeAgentTaskPayload
		if existing.NodeID != input.TargetNodeID || json.Unmarshal(existing.Payload, &saved) != nil || saved.Kind != "agent_prompt" || saved.SourceConversationID != conversationID || saved.Content != input.Content || !sameTaskInputArtifacts(saved.Attachments, attachments) {
			write(w, http.StatusConflict, map[string]string{"error": "idempotency key is already bound to a different local task request"})
			return
		}
		attached, err := s.store.ExecutionTaskArtifacts(r.Context(), s.workspaceID, existing.ID)
		if err != nil {
			fail(w, err)
			return
		}
		contextFound, deltaFound, lfsFound, submoduleFound := false, saved.SourceProjectDelta == nil, saved.SourceProjectDelta == nil || saved.SourceProjectDelta.LFSObjects == nil, saved.SourceProjectDelta == nil || saved.SourceProjectDelta.SubmoduleDeltas == nil
		inputArtifactsFound := make(map[string]bool, len(saved.Attachments))
		for _, artifact := range attached {
			if artifact.ID == saved.SourceContextArtifactID && artifact.Role == "conversation_context" && artifact.SHA256 == saved.SourceContextSHA256 && artifact.ByteSize > 0 && (saved.SourceContextByteSize == 0 || artifact.ByteSize == saved.SourceContextByteSize) {
				contextFound = true
			}
			if saved.SourceProjectDelta != nil && artifact.ID == saved.SourceProjectDelta.ArtifactID && artifact.Role == "source_project_delta" && artifact.SHA256 == saved.SourceProjectDelta.SHA256 && artifact.ByteSize == saved.SourceProjectDelta.ByteSize {
				deltaFound = true
			}
			if saved.SourceProjectDelta != nil && saved.SourceProjectDelta.LFSObjects != nil && artifact.ID == saved.SourceProjectDelta.LFSObjects.ArtifactID && artifact.Role == "source_project_lfs_objects" && artifact.SHA256 == saved.SourceProjectDelta.LFSObjects.SHA256 && artifact.ByteSize == saved.SourceProjectDelta.LFSObjects.ByteSize {
				lfsFound = true
			}
			if saved.SourceProjectDelta != nil && saved.SourceProjectDelta.SubmoduleDeltas != nil && artifact.ID == saved.SourceProjectDelta.SubmoduleDeltas.ArtifactID && artifact.Role == "source_project_submodule_deltas" && artifact.SHA256 == saved.SourceProjectDelta.SubmoduleDeltas.SHA256 && artifact.ByteSize == saved.SourceProjectDelta.SubmoduleDeltas.ByteSize {
				submoduleFound = true
			}
			for _, expected := range saved.Attachments {
				if artifact.Role == "user_input" && artifact.ID == expected.ArtifactID && artifact.SHA256 == expected.SHA256 && artifact.ByteSize == expected.ByteSize && artifact.FileName == expected.FileName && artifact.MediaType == expected.MediaType {
					inputArtifactsFound[expected.ArtifactID] = true
				}
			}
		}
		if !contextFound || !deltaFound || !lfsFound || !submoduleFound || len(inputArtifactsFound) != len(saved.Attachments) {
			write(w, http.StatusConflict, map[string]string{"error": "original local task artifacts did not read back from durable storage"})
			return
		}
		write(w, http.StatusOK, map[string]any{"task": existing, "created": false})
		return
	} else if !errors.Is(lookupErr, domain.ErrNotFound) {
		fail(w, lookupErr)
		return
	}
	rawTaskID := make([]byte, 16)
	if _, err := rand.Read(rawTaskID); err != nil {
		fail(w, err)
		return
	}
	taskID := "task_" + hex.EncodeToString(rawTaskID)
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "conversation context artifact storage is unavailable"})
		return
	}
	turns, err := s.store.AgentTurns(r.Context(), s.workspaceID, conversationID)
	if err != nil {
		fail(w, err)
		return
	}
	for _, turn := range turns {
		if turn.Status == "running" || turn.Status == "cancelling" || turn.Status == "awaiting_approval" {
			write(w, http.StatusConflict, map[string]string{"error": "finish or cancel the active cloud turn before copying this conversation to a local node"})
			return
		}
	}
	var sourceProject *domain.Project
	var sourceProjectRecord *domain.Project
	if conversation.ProjectID != "" {
		project, err := s.store.Project(r.Context(), s.workspaceID, conversation.ProjectID)
		if err != nil {
			fail(w, fmt.Errorf("read source conversation project: %w", err))
			return
		}
		if project.ID != conversation.ProjectID || project.UserID != s.workspaceID {
			write(w, http.StatusConflict, map[string]string{"error": "source project ownership did not read back"})
			return
		}
		sourceProjectRecord = &project
		// A host-specific path is never sent to another computer. Only stable
		// repository identity and commit metadata cross the node boundary.
		sourceProject = &domain.Project{ID: project.ID, Name: project.Name, Instructions: project.Instructions, InstructionsEnabled: project.InstructionsEnabled, RemoteRepoURL: project.RemoteRepoURL, RemoteBranch: project.RemoteBranch, RepositoryProvider: project.RepositoryProvider, ResolvedCommit: project.ResolvedCommit, MeasuredBytes: project.MeasuredBytes}
	}
	keyDigest := sha256.Sum256([]byte(key))
	var sourceProjectDelta *nodeAgentTaskProjectDelta
	var sourceProjectDeltaArtifact storage.Artifact
	var sourceProjectLFSArtifact storage.Artifact
	var sourceProjectSubmoduleDeltasArtifact storage.Artifact
	if sourceProjectRecord != nil && sourceProjectRecord.ResolvedCommit != "" && strings.TrimSpace(sourceProjectRecord.Workdir) != "" {
		snapshotter := s.projectDeltas
		if snapshotter == nil {
			snapshotter = s.agent
		}
		if snapshotter == nil {
			write(w, http.StatusServiceUnavailable, map[string]string{"error": "project workspace snapshot service is unavailable"})
			return
		}
		delta, err := snapshotter.CreateProjectDeltaBundle(r.Context(), *sourceProjectRecord, taskID)
		if err != nil {
			fail(w, fmt.Errorf("snapshot current cloud project workspace for the local task: %w", err))
			return
		}
		if !validPublicationSHA(delta.BaseCommit) || !validPublicationSHA(delta.Commit) || !strings.EqualFold(delta.BaseCommit, sourceProjectRecord.ResolvedCommit) {
			cleanupErr := errors.Join(cleanupNodeTaskProjectDelta(delta.Path), cleanupNodeTaskProjectDelta(delta.LFSObjectsPath), cleanupNodeTaskProjectDelta(delta.SubmoduleDeltasPath))
			fail(w, errors.Join(fmt.Errorf("cloud project snapshot did not match the pinned repository commit: %w", domain.ErrConflict), cleanupErr))
			return
		}
		delta.BaseCommit = sourceProjectRecord.ResolvedCommit
		delta.Commit = strings.ToLower(delta.Commit)
		if !strings.EqualFold(delta.Commit, delta.BaseCommit) {
			sourceProjectDeltaArtifact, err = s.storeNodeTaskSourceProjectDelta(r.Context(), s.workspaceID, hex.EncodeToString(keyDigest[:]), delta)
			if err != nil {
				fail(w, err)
				return
			}
			var lfsRef *nodeAgentTaskArtifact
			var submoduleRef *nodeAgentTaskArtifact
			if delta.LFSObjectsPath != "" {
				sourceProjectLFSArtifact, err = s.storeNodeTaskSourceProjectLFS(r.Context(), s.workspaceID, hex.EncodeToString(keyDigest[:]), delta.LFSObjectsPath)
				if err != nil {
					fail(w, err)
					return
				}
				lfsRef = &nodeAgentTaskArtifact{ArtifactID: sourceProjectLFSArtifact.ID, SHA256: sourceProjectLFSArtifact.SHA256, ByteSize: sourceProjectLFSArtifact.ByteSize}
			}
			if delta.SubmoduleDeltasPath != "" {
				sourceProjectSubmoduleDeltasArtifact, err = s.storeNodeTaskSourceProjectSubmoduleDeltas(r.Context(), s.workspaceID, hex.EncodeToString(keyDigest[:]), delta.SubmoduleDeltasPath)
				if err != nil {
					fail(w, err)
					return
				}
				submoduleRef = &nodeAgentTaskArtifact{ArtifactID: sourceProjectSubmoduleDeltasArtifact.ID, SHA256: sourceProjectSubmoduleDeltasArtifact.SHA256, ByteSize: sourceProjectSubmoduleDeltasArtifact.ByteSize}
			}
			sourceProjectDelta = &nodeAgentTaskProjectDelta{ArtifactID: sourceProjectDeltaArtifact.ID, SHA256: sourceProjectDeltaArtifact.SHA256, ByteSize: sourceProjectDeltaArtifact.ByteSize, BaseCommit: delta.BaseCommit, Commit: delta.Commit, LFSObjects: lfsRef, SubmoduleDeltas: submoduleRef}
		} else if delta.Path != "" {
			cleanupErr := errors.Join(cleanupNodeTaskProjectDelta(delta.Path), cleanupNodeTaskProjectDelta(delta.LFSObjectsPath), cleanupNodeTaskProjectDelta(delta.SubmoduleDeltasPath))
			fail(w, errors.Join(fmt.Errorf("unchanged cloud project snapshot unexpectedly returned a bundle: %w", domain.ErrConflict), cleanupErr))
			return
		}
	}
	trace, err := s.store.TraceEvents(r.Context(), s.workspaceID, conversationID)
	if err != nil {
		fail(w, err)
		return
	}
	contextBytes, err := json.Marshal(localTaskConversationContext{Conversation: conversation, Project: sourceProject, Turns: turns, Trace: trace})
	if err != nil {
		fail(w, err)
		return
	}
	contextDigest := sha256.Sum256(contextBytes)
	contextSHA256 := hex.EncodeToString(contextDigest[:])
	contextArtifact, err := s.artifacts.StoreFromReader(r.Context(), s.workspaceID, "conversation-context.json", "application/json", "local-task-context:"+hex.EncodeToString(keyDigest[:]), int64(len(contextBytes)), contextSHA256, bytes.NewReader(contextBytes), time.Now().UTC())
	if err != nil {
		fail(w, fmt.Errorf("persist cloud conversation context artifact: %w", err))
		return
	}
	payload, err := json.Marshal(nodeAgentTaskPayload{Kind: "agent_prompt", SourceConversationID: conversationID, Content: input.Content, Title: conversation.Title, SourceContextArtifactID: contextArtifact.ID, SourceContextSHA256: contextArtifact.SHA256, SourceContextByteSize: contextArtifact.ByteSize, SourceProjectDelta: sourceProjectDelta, Attachments: attachments})
	if err != nil {
		fail(w, err)
		return
	}
	links := []storage.ExecutionTaskArtifactLink{{ArtifactID: contextArtifact.ID, Role: "conversation_context"}}
	if sourceProjectDelta != nil {
		links = append(links, storage.ExecutionTaskArtifactLink{ArtifactID: sourceProjectDeltaArtifact.ID, Role: "source_project_delta"})
		if sourceProjectDelta.LFSObjects != nil {
			links = append(links, storage.ExecutionTaskArtifactLink{ArtifactID: sourceProjectLFSArtifact.ID, Role: "source_project_lfs_objects"})
		}
		if sourceProjectDelta.SubmoduleDeltas != nil {
			links = append(links, storage.ExecutionTaskArtifactLink{ArtifactID: sourceProjectSubmoduleDeltasArtifact.ID, Role: "source_project_submodule_deltas"})
		}
	}
	links = append(links, attachmentLinks...)
	task, created, err := s.store.CreateExecutionTaskWithArtifacts(r.Context(), storage.ExecutionTask{
		ID: taskID, UserID: s.workspaceID,
		NodeID: input.TargetNodeID, IdempotencyKey: key, Payload: payload,
	}, input.WaitForNode, links, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionTask(r.Context(), s.workspaceID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.NodeID != input.TargetNodeID || string(readBack.Payload) != string(payload) {
		write(w, http.StatusConflict, map[string]string{"error": "local task submission did not read back as the requested task"})
		return
	}
	attached, err := s.store.ExecutionTaskArtifacts(r.Context(), s.workspaceID, readBack.ID)
	if err != nil {
		fail(w, err)
		return
	}
	contextAttached := false
	for _, artifact := range attached {
		if artifact.ID == contextArtifact.ID && artifact.Role == "conversation_context" && artifact.SHA256 == contextSHA256 && artifact.ByteSize == int64(len(contextBytes)) {
			contextAttached = true
			break
		}
	}
	if !contextAttached {
		write(w, http.StatusConflict, map[string]string{"error": "local task source conversation context did not read back from durable task artifacts"})
		return
	}
	if err := s.verifyTaskInputArtifactLinks(r.Context(), readBack.ID, attachments); err != nil {
		fail(w, err)
		return
	}
	s.notifyExecutionNode(readBack.NodeID)
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	write(w, status, map[string]any{"task": readBack, "created": created})
}

func (s *Server) storeNodeTaskSourceProjectDelta(ctx context.Context, userID, keyDigest string, delta agent.ProjectDeltaBundle) (artifact storage.Artifact, retErr error) {
	if s == nil || s.artifacts == nil || userID == "" || len(keyDigest) != 64 || delta.Path == "" || delta.Commit == delta.BaseCommit {
		return storage.Artifact{}, domain.ErrInvalid
	}
	defer func() {
		removeErr := os.Remove(delta.Path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		if _, err := os.Lstat(delta.Path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = errors.New("cloud project delta file remains after artifact storage")
			}
			removeErr = errors.Join(removeErr, err)
		}
		retErr = errors.Join(retErr, removeErr)
	}()
	file, err := os.Open(delta.Path)
	if err != nil {
		return storage.Artifact{}, fmt.Errorf("open cloud project workspace delta: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		closeErr := file.Close()
		return storage.Artifact{}, errors.Join(fmt.Errorf("cloud project workspace delta is not a regular non-empty file: %w", domain.ErrConflict), statErr, closeErr)
	}
	hasher := sha256.New()
	_, hashErr := io.Copy(hasher, file)
	_, seekErr := file.Seek(0, io.SeekStart)
	if hashErr != nil || seekErr != nil {
		closeErr := file.Close()
		return storage.Artifact{}, errors.Join(fmt.Errorf("hash cloud project workspace delta: %w", errors.Join(hashErr, seekErr)), closeErr)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	stableKeyDigest := sha256.Sum256([]byte(keyDigest + ":" + digest))
	artifactKey := "node-task-project-delta:" + hex.EncodeToString(stableKeyDigest[:])
	artifact, storeErr := s.artifacts.StoreFromReader(ctx, userID, "cloud-project-workspace.bundle", "application/x-git-bundle", artifactKey, info.Size(), digest, file, time.Now().UTC())
	closeErr := file.Close()
	if storeErr != nil || closeErr != nil {
		return storage.Artifact{}, errors.Join(fmt.Errorf("persist cloud project workspace delta artifact: %w", storeErr), closeErr)
	}
	readBack, readFile, readErr := s.artifacts.Open(ctx, userID, artifact.ID)
	if readErr != nil {
		return storage.Artifact{}, fmt.Errorf("read back cloud project workspace delta artifact: %w", readErr)
	}
	closeReadErr := readFile.Close()
	if readBack.ID != artifact.ID || readBack.SHA256 != digest || readBack.ByteSize != info.Size() || closeReadErr != nil {
		return storage.Artifact{}, errors.Join(fmt.Errorf("cloud project workspace delta artifact failed authoritative read-back: %w", domain.ErrConflict), closeReadErr)
	}
	return readBack, nil
}

func (s *Server) storeNodeTaskSourceProjectLFS(ctx context.Context, userID, keyDigest, archivePath string) (artifact storage.Artifact, retErr error) {
	if s == nil || s.artifacts == nil || userID == "" || len(keyDigest) != 64 || strings.TrimSpace(archivePath) == "" {
		return storage.Artifact{}, domain.ErrInvalid
	}
	defer func() {
		removeErr := os.Remove(archivePath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		if _, err := os.Lstat(archivePath); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = errors.New("cloud project LFS archive remains after artifact storage")
			}
			removeErr = errors.Join(removeErr, err)
		}
		retErr = errors.Join(retErr, removeErr)
	}()
	file, err := os.Open(archivePath)
	if err != nil {
		return storage.Artifact{}, fmt.Errorf("open cloud project Git LFS archive: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return storage.Artifact{}, errors.Join(fmt.Errorf("cloud project Git LFS archive is not a regular non-empty file: %w", domain.ErrConflict), statErr, file.Close())
	}
	hasher := sha256.New()
	_, hashErr := io.Copy(hasher, file)
	_, seekErr := file.Seek(0, io.SeekStart)
	if hashErr != nil || seekErr != nil {
		return storage.Artifact{}, errors.Join(fmt.Errorf("hash cloud project Git LFS archive: %w", errors.Join(hashErr, seekErr)), file.Close())
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	stableKeyDigest := sha256.Sum256([]byte(keyDigest + ":" + digest))
	artifactKey := "node-task-project-lfs:" + hex.EncodeToString(stableKeyDigest[:])
	artifact, storeErr := s.artifacts.StoreFromReader(ctx, userID, "cloud-project-lfs-objects.tar", "application/x-tar", artifactKey, info.Size(), digest, file, time.Now().UTC())
	closeErr := file.Close()
	if storeErr != nil || closeErr != nil {
		return storage.Artifact{}, errors.Join(fmt.Errorf("persist cloud project Git LFS archive artifact: %w", storeErr), closeErr)
	}
	readBack, readFile, readErr := s.artifacts.Open(ctx, userID, artifact.ID)
	if readErr != nil {
		return storage.Artifact{}, fmt.Errorf("read back cloud project Git LFS archive artifact: %w", readErr)
	}
	closeReadErr := readFile.Close()
	if readBack.ID != artifact.ID || readBack.SHA256 != digest || readBack.ByteSize != info.Size() || closeReadErr != nil {
		return storage.Artifact{}, errors.Join(fmt.Errorf("cloud project Git LFS artifact failed authoritative read-back: %w", domain.ErrConflict), closeReadErr)
	}
	return readBack, nil
}

func (s *Server) storeNodeTaskSourceProjectSubmoduleDeltas(ctx context.Context, userID, keyDigest, archivePath string) (artifact storage.Artifact, retErr error) {
	if s == nil || s.artifacts == nil || userID == "" || len(keyDigest) != 64 || strings.TrimSpace(archivePath) == "" {
		return storage.Artifact{}, domain.ErrInvalid
	}
	defer func() { retErr = errors.Join(retErr, cleanupNodeTaskProjectDelta(archivePath)) }()
	file, err := os.Open(archivePath)
	if err != nil {
		return storage.Artifact{}, fmt.Errorf("open cloud project submodule delta archive: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return storage.Artifact{}, errors.Join(fmt.Errorf("cloud project submodule archive is not a regular non-empty file: %w", domain.ErrConflict), statErr, file.Close())
	}
	hasher := sha256.New()
	_, hashErr := io.Copy(hasher, file)
	_, seekErr := file.Seek(0, io.SeekStart)
	if hashErr != nil || seekErr != nil {
		return storage.Artifact{}, errors.Join(fmt.Errorf("hash cloud project submodule delta archive: %w", errors.Join(hashErr, seekErr)), file.Close())
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	keyDigestHash := sha256.Sum256([]byte(keyDigest + ":" + digest))
	artifact, storeErr := s.artifacts.StoreFromReader(ctx, userID, "cloud-project-submodule-deltas.tar", "application/x-tar", "node-task-project-submodule-deltas:"+hex.EncodeToString(keyDigestHash[:]), info.Size(), digest, file, time.Now().UTC())
	closeErr := file.Close()
	if storeErr != nil || closeErr != nil {
		return storage.Artifact{}, errors.Join(fmt.Errorf("persist cloud project submodule delta archive: %w", storeErr), closeErr)
	}
	readBack, readFile, readErr := s.artifacts.Open(ctx, userID, artifact.ID)
	if readErr != nil {
		return storage.Artifact{}, fmt.Errorf("read back cloud project submodule delta archive: %w", readErr)
	}
	closeReadErr := readFile.Close()
	if readBack.ID != artifact.ID || readBack.SHA256 != digest || readBack.ByteSize != info.Size() || closeReadErr != nil {
		return storage.Artifact{}, errors.Join(fmt.Errorf("cloud project submodule archive failed authoritative read-back: %w", domain.ErrConflict), closeReadErr)
	}
	return readBack, nil
}

func cleanupNodeTaskProjectDelta(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	removeErr := os.Remove(path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if removeErr == nil {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			if statErr == nil {
				statErr = errors.New("temporary cloud project bundle remains after cleanup")
			}
			removeErr = statErr
		}
	}
	if removeErr != nil {
		return fmt.Errorf("remove temporary cloud project bundle: %w", removeErr)
	}
	return nil
}

func (s *Server) executionTaskList(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ExecutionTasks(r.Context(), s.workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	for i := range tasks {
		if err := s.attachImportedProject(r, &tasks[i]); err != nil {
			fail(w, err)
			return
		}
	}
	write(w, http.StatusOK, tasks)
}

func (s *Server) executionTaskGet(w http.ResponseWriter, r *http.Request) {
	task, err := s.store.ExecutionTask(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.attachImportedProject(r, &task); err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, task)
}

func (s *Server) attachImportedProject(r *http.Request, task *storage.ExecutionTask) error {
	if task == nil || (task.Status != "reported_succeeded" && task.Status != "completed" && !(task.Status == "needs_reconciliation" && len(task.RecoveryResult) > 0)) {
		return nil
	}
	resultJSON := task.Result
	if task.Status == "needs_reconciliation" {
		resultJSON = task.RecoveryResult
	}
	var result struct {
		SourceConversationID   string `json:"sourceConversationId"`
		ConversationID         string `json:"conversationId"`
		ProjectID              string `json:"projectId"`
		ExecutionProjectID     string `json:"executionProjectId"`
		ProjectDeltaBaseCommit string `json:"projectDeltaBaseCommit"`
		ProjectDeltaCommit     string `json:"projectDeltaCommit"`
		ProjectDeltaArtifact   *struct {
			ID string `json:"id"`
		} `json:"projectDeltaArtifact"`
		ProjectLFSArtifact *struct {
			ID string `json:"id"`
		} `json:"projectLfsArtifact"`
	}
	if json.Unmarshal(resultJSON, &result) != nil {
		return nil
	}
	continuedProjectID := result.ProjectID
	if result.ExecutionProjectID != "" {
		conversation, err := s.store.Conversation(r.Context(), s.workspaceID, result.ConversationID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if conversation.ProjectID == "" {
			return fmt.Errorf("cloud execution result names a project workspace for a projectless conversation: %w", domain.ErrConflict)
		}
		source, err := s.store.Project(r.Context(), s.workspaceID, conversation.ProjectID)
		if err != nil {
			return err
		}
		workspace, err := s.store.Project(r.Context(), s.workspaceID, result.ExecutionProjectID)
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("cloud execution result project workspace is missing: %w", domain.ErrConflict)
		}
		if err != nil {
			return err
		}
		if workspace.ID != agent.ProjectDeltaProjectID(source.ID, task.ID) || workspace.RemoteRepoURL != source.RemoteRepoURL || workspace.RemoteBranch != "codex/task-"+task.ID || !strings.EqualFold(workspace.ResolvedCommit, source.ResolvedCommit) {
			return fmt.Errorf("cloud execution result project workspace does not match its task and source: %w", domain.ErrConflict)
		}
		task.ImportedProject = &workspace
	}
	if result.ProjectID != "" && result.ProjectDeltaCommit != "" && result.ProjectDeltaArtifact != nil && result.ProjectDeltaArtifact.ID != "" {
		source, err := s.store.Project(r.Context(), s.workspaceID, result.ProjectID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		imported, err := s.store.Project(r.Context(), s.workspaceID, agent.ProjectDeltaProjectID(source.ID, task.ID))
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		expectedBranch := "codex/task-" + task.ID
		if imported.RemoteRepoURL != source.RemoteRepoURL || imported.RemoteBranch != expectedBranch || !strings.EqualFold(imported.ResolvedCommit, result.ProjectDeltaCommit) {
			return fmt.Errorf("durable task project import does not match the task result: %w", domain.ErrConflict)
		}
		if s.agent != nil {
			if err := s.agent.VerifyProjectLFSImport(r.Context(), imported, result.ProjectDeltaBaseCommit, result.ProjectDeltaCommit); err != nil {
				return fmt.Errorf("task project branch is not ready for continuation because Git LFS content did not verify: %w", err)
			}
		}
		task.ImportedProject = &imported
		continuedProjectID = imported.ID
	}
	source, err := s.store.Conversation(r.Context(), s.workspaceID, result.SourceConversationID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	continued, err := s.store.Conversation(r.Context(), s.workspaceID, agent.TaskContinuationConversationID(source.ID, task.ID))
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if continued.ParentConversationID != source.ID || continued.ProjectID != continuedProjectID {
		return fmt.Errorf("durable task continuation does not match its source and selected project: %w", domain.ErrConflict)
	}
	task.ContinuedConversation = &continued.Conversation
	cloudTask, err := s.store.ExecutionTaskByIdempotencyKey(r.Context(), s.workspaceID, "cloud-handoff:"+task.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err == nil {
		var payload struct {
			ConversationID string `json:"conversationId"`
			Kind           string `json:"kind"`
		}
		if json.Unmarshal(cloudTask.Payload, &payload) != nil || cloudTask.NodeID != storage.CloudExecutionNodeID(s.workspaceID) || payload.ConversationID != continued.ID || (payload.Kind != "agent_turn" && payload.Kind != "agent_continuation") {
			return fmt.Errorf("durable cloud continuation task does not match its conversation: %w", domain.ErrConflict)
		}
		task.ContinuationTaskID = cloudTask.ID
		task.ContinuationTaskStatus = cloudTask.Status
	}
	return nil
}

func (s *Server) executionTaskImportProjectDelta(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	task, err := s.store.ExecutionTask(r.Context(), s.workspaceID, taskID)
	if err != nil {
		fail(w, err)
		return
	}
	recoveryImport := task.Status == "needs_reconciliation" && len(task.RecoveryResult) > 0
	if task.Status != "reported_succeeded" && !recoveryImport {
		write(w, http.StatusConflict, map[string]string{"error": "only a durably successful local task can be imported"})
		return
	}
	resultJSON := task.Result
	if recoveryImport {
		resultJSON = task.RecoveryResult
	}
	var result struct {
		ProjectID              string `json:"projectId"`
		ProjectCommit          string `json:"projectCommit"`
		AgentStatus            string `json:"agentStatus"`
		AgentStopReason        string `json:"agentStopReason"`
		ProjectDeltaBaseCommit string `json:"projectDeltaBaseCommit"`
		ProjectDeltaCommit     string `json:"projectDeltaCommit"`
		ProjectDeltaArtifact   *struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"projectDeltaArtifact"`
		ProjectLFSArtifact *struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"projectLfsArtifact"`
		ProjectSubmoduleDeltasArtifact *struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"projectSubmoduleDeltasArtifact"`
	}
	if err := json.Unmarshal(resultJSON, &result); err != nil || !validLocalTaskContinuationState(result.AgentStatus, result.AgentStopReason) || result.ProjectID == "" || result.ProjectCommit == "" || result.ProjectDeltaBaseCommit == "" || result.ProjectDeltaCommit == "" || result.ProjectDeltaArtifact == nil {
		write(w, http.StatusConflict, map[string]string{"error": "task result has no complete project delta from a completed or safely resumable turn"})
		return
	}
	if recoveryImport {
		body, err := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
		if err != nil {
			fail(w, fmt.Errorf("read recovery project import confirmation: %w", err))
			return
		}
		var confirmation struct {
			ConfirmOldNodeStopped bool `json:"confirmOldNodeStopped"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if len(body) > 16<<10 || decoder.Decode(&confirmation) != nil || !confirmation.ConfirmOldNodeStopped {
			write(w, http.StatusPreconditionRequired, map[string]any{"error": "confirm that the original node has been stopped before importing its project snapshot", "requiresOldNodeStopConfirmation": true})
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			fail(w, fmt.Errorf("decode trailing recovery project import data: %w", domain.ErrInvalid))
			return
		}
	}
	if result.ProjectCommit != result.ProjectDeltaBaseCommit || result.ProjectDeltaArtifact.ID == "" || result.ProjectDeltaArtifact.ByteSize <= 0 {
		write(w, http.StatusConflict, map[string]string{"error": "task project baseline or delta artifact manifest is inconsistent"})
		return
	}
	links, err := s.store.ExecutionTaskArtifacts(r.Context(), s.workspaceID, taskID)
	if err != nil {
		fail(w, err)
		return
	}
	var manifest *storage.ExecutionTaskArtifact
	var lfsManifest *storage.ExecutionTaskArtifact
	var submoduleManifest *storage.ExecutionTaskArtifact
	deltaRole := "project_delta"
	lfsRole := "project_lfs_objects"
	submoduleRole := "project_submodule_deltas"
	if recoveryImport {
		deltaRole = "recovery_project_delta"
		lfsRole = "recovery_project_lfs_objects"
		submoduleRole = "recovery_project_submodule_deltas"
	}
	for i := range links {
		if links[i].Role == deltaRole && links[i].ID == result.ProjectDeltaArtifact.ID {
			manifest = &links[i]
		}
		if result.ProjectLFSArtifact != nil && links[i].Role == lfsRole && links[i].ID == result.ProjectLFSArtifact.ID {
			lfsManifest = &links[i]
		}
		if result.ProjectSubmoduleDeltasArtifact != nil && links[i].Role == submoduleRole && links[i].ID == result.ProjectSubmoduleDeltasArtifact.ID {
			submoduleManifest = &links[i]
		}
	}
	if manifest == nil || manifest.SHA256 != result.ProjectDeltaArtifact.SHA256 || manifest.ByteSize != result.ProjectDeltaArtifact.ByteSize {
		write(w, http.StatusConflict, map[string]string{"error": "project delta artifact is not bound to this task with the reported manifest"})
		return
	}
	if result.ProjectLFSArtifact != nil && (lfsManifest == nil || lfsManifest.SHA256 != result.ProjectLFSArtifact.SHA256 || lfsManifest.ByteSize != result.ProjectLFSArtifact.ByteSize || lfsManifest.ByteSize <= 0 || len(lfsManifest.SHA256) != 64) {
		write(w, http.StatusConflict, map[string]string{"error": "Git LFS object archive is not bound to this task with the reported manifest"})
		return
	}
	if result.ProjectSubmoduleDeltasArtifact != nil && (submoduleManifest == nil || submoduleManifest.SHA256 != result.ProjectSubmoduleDeltasArtifact.SHA256 || submoduleManifest.ByteSize != result.ProjectSubmoduleDeltasArtifact.ByteSize || submoduleManifest.ByteSize <= 0 || len(submoduleManifest.SHA256) != 64) {
		write(w, http.StatusConflict, map[string]string{"error": "submodule delta archive is not bound to this task with the reported manifest"})
		return
	}
	project, err := s.store.Project(r.Context(), s.workspaceID, result.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	if project.ResolvedCommit != result.ProjectDeltaBaseCommit {
		write(w, http.StatusConflict, map[string]string{"error": "cloud project no longer matches the local task baseline; its worktree was preserved"})
		return
	}
	if s.agent == nil || s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "Agent project import is unavailable"})
		return
	}
	artifact, file, err := s.artifacts.Open(r.Context(), s.workspaceID, manifest.ID)
	if err != nil {
		fail(w, err)
		return
	}
	var openedArtifacts []io.ReadCloser
	openedArtifacts = append(openedArtifacts, file)
	closeOpenedArtifacts := func() error {
		var closeErrors []error
		for index := len(openedArtifacts) - 1; index >= 0; index-- {
			closeErrors = append(closeErrors, openedArtifacts[index].Close())
		}
		openedArtifacts = nil
		return errors.Join(closeErrors...)
	}
	defer func() {
		if closeErr := closeOpenedArtifacts(); closeErr != nil {
			slog.Error("task project artifact reader cleanup failed", "task_id", taskID, "error", closeErr)
		}
	}()
	if artifact.ID != manifest.ID || artifact.SHA256 != manifest.SHA256 || artifact.ByteSize != manifest.ByteSize {
		fail(w, errors.Join(domain.ErrConflict, closeOpenedArtifacts()))
		return
	}
	var lfsFile io.Reader
	var lfsArtifact storage.Artifact
	if lfsManifest != nil {
		var opened io.ReadCloser
		lfsArtifact, opened, err = s.artifacts.Open(r.Context(), s.workspaceID, lfsManifest.ID)
		if err != nil {
			fail(w, errors.Join(err, closeOpenedArtifacts()))
			return
		}
		openedArtifacts = append(openedArtifacts, opened)
		if lfsArtifact.ID != lfsManifest.ID || lfsArtifact.SHA256 != lfsManifest.SHA256 || lfsArtifact.ByteSize != lfsManifest.ByteSize {
			fail(w, errors.Join(domain.ErrConflict, closeOpenedArtifacts()))
			return
		}
		lfsFile = opened
	}
	var submoduleFile io.Reader
	var submoduleArtifact storage.Artifact
	if submoduleManifest != nil {
		var opened io.ReadCloser
		submoduleArtifact, opened, err = s.artifacts.Open(r.Context(), s.workspaceID, submoduleManifest.ID)
		if err != nil {
			fail(w, errors.Join(err, closeOpenedArtifacts()))
			return
		}
		openedArtifacts = append(openedArtifacts, opened)
		if submoduleArtifact.ID != submoduleManifest.ID || submoduleArtifact.SHA256 != submoduleManifest.SHA256 || submoduleArtifact.ByteSize != submoduleManifest.ByteSize {
			fail(w, errors.Join(domain.ErrConflict, closeOpenedArtifacts()))
			return
		}
		submoduleFile = opened
	}
	imported, created, err := s.agent.ImportProjectDeltaBundleWithArtifacts(r.Context(), s.workspaceID, project, taskID, file, artifact.ByteSize, artifact.SHA256, result.ProjectDeltaBaseCommit, result.ProjectDeltaCommit, lfsFile, lfsArtifact.ByteSize, lfsArtifact.SHA256, submoduleFile, submoduleArtifact.ByteSize, submoduleArtifact.SHA256)
	closeErr := closeOpenedArtifacts()
	if err != nil {
		fail(w, errors.Join(fmt.Errorf("import and verify task Git project content: %w", err), closeErr))
		return
	}
	readBack, err := s.store.Project(r.Context(), s.workspaceID, imported.ID)
	if err != nil {
		fail(w, errors.Join(err, closeErr))
		return
	}
	if readBack.ID != imported.ID || readBack.ResolvedCommit != result.ProjectDeltaCommit || readBack.RemoteBranch != imported.RemoteBranch {
		fail(w, errors.Join(domain.ErrConflict, closeErr))
		return
	}
	if closeErr != nil {
		fail(w, fmt.Errorf("project import state was read back, but temporary artifact cleanup failed; retry the idempotent import to verify cleanup: %w", closeErr))
		return
	}
	write(w, http.StatusOK, map[string]any{"project": readBack, "created": created})
}

func (s *Server) executionTaskContinueInCloud(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "Task artifact storage is unavailable"})
		return
	}
	task, err := s.store.ExecutionTask(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	sourceArtifacts, err := s.store.ExecutionTaskArtifacts(r.Context(), s.workspaceID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	inputArtifactIDs := make([]string, 0)
	for _, artifact := range sourceArtifacts {
		if artifact.Role == "user_input" {
			inputArtifactIDs = append(inputArtifactIDs, artifact.ID)
		}
	}
	inputAttachments, inputArtifactLinks, err := s.resolveTaskInputArtifacts(r.Context(), inputArtifactIDs)
	if err != nil {
		fail(w, err)
		return
	}
	recoveryContinuation := task.Status == "needs_reconciliation" && len(task.RecoveryResult) > 0
	if task.Status != "reported_succeeded" && !recoveryContinuation {
		write(w, http.StatusConflict, map[string]string{"error": "only a durably successful local task can continue in the cloud"})
		return
	}
	resultJSON := task.Result
	if recoveryContinuation {
		resultJSON = task.RecoveryResult
	}
	var result struct {
		SourceConversationID   string `json:"sourceConversationId"`
		ProjectID              string `json:"projectId"`
		ProjectCommit          string `json:"projectCommit"`
		ProjectDeltaBaseCommit string `json:"projectDeltaBaseCommit"`
		ProjectDeltaCommit     string `json:"projectDeltaCommit"`
		ConversationID         string `json:"conversationId"`
		AgentTurnID            string `json:"agentTurnId"`
		ResultMessageID        string `json:"resultMessageId"`
		AgentStatus            string `json:"agentStatus"`
		AgentStopReason        string `json:"agentStopReason"`
		ProjectDeltaArtifact   *struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"projectDeltaArtifact"`
		HandoffCheckpointArtifact *struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"handoffCheckpointArtifact"`
		TranscriptArtifact *struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"transcriptArtifact"`
	}
	if err := json.Unmarshal(resultJSON, &result); err != nil || !validLocalTaskContinuationState(result.AgentStatus, result.AgentStopReason) || result.SourceConversationID == "" || result.ConversationID == "" || result.AgentTurnID == "" || result.ResultMessageID == "" || result.TranscriptArtifact == nil {
		write(w, http.StatusConflict, map[string]string{"error": "task result has no completed or safely resumable transcript to continue"})
		return
	}
	if (result.ProjectDeltaBaseCommit == "") != (result.ProjectDeltaCommit == "") || result.ProjectDeltaCommit != "" && (result.ProjectDeltaBaseCommit != result.ProjectCommit || result.ProjectDeltaArtifact == nil || result.ProjectID == "" || result.ProjectCommit == "") || result.ProjectDeltaArtifact != nil && result.ProjectDeltaCommit == "" {
		write(w, http.StatusConflict, map[string]string{"error": "task project delta metadata is incomplete or inconsistent"})
		return
	}
	if result.ProjectDeltaArtifact != nil && (result.ProjectDeltaArtifact.ID == "" || result.ProjectDeltaArtifact.ByteSize <= 0 || len(result.ProjectDeltaArtifact.SHA256) != 64) {
		write(w, http.StatusConflict, map[string]string{"error": "task project delta artifact manifest is incomplete"})
		return
	}
	if result.HandoffCheckpointArtifact != nil && (result.AgentStatus != "incomplete" || result.HandoffCheckpointArtifact.ID == "" || result.HandoffCheckpointArtifact.ByteSize <= 0 || len(result.HandoffCheckpointArtifact.SHA256) != 64) {
		write(w, http.StatusConflict, map[string]string{"error": "task handoff checkpoint is incomplete or is not bound to a safely resumable Agent turn"})
		return
	}
	if result.TranscriptArtifact.ID == "" || result.TranscriptArtifact.ByteSize <= 0 || len(result.TranscriptArtifact.SHA256) != 64 {
		write(w, http.StatusConflict, map[string]string{"error": "task transcript artifact manifest is incomplete"})
		return
	}
	continuationInstruction := "继续这个暂停任务。先检查当前工作状态，不要重复已完成的操作。"
	if result.AgentStatus == "completed" {
		continuationInstruction = "请阅读这段本地任务对话及当前项目状态，基于用户最后的请求继续协助；不要重复已经完成的操作。"
	}
	var continuationRequest struct {
		Instruction                string                        `json:"instruction"`
		AcknowledgeExternalEffects bool                          `json:"acknowledgeExternalEffects"` // Legacy hint only; never satisfies per-effect review.
		ConfirmOldNodeStopped      bool                          `json:"confirmOldNodeStopped"`
		EffectResolutions          []taskHandoffEffectResolution `json:"effectResolutions"`
	}
	if r.Body != nil && r.Body != http.NoBody {
		body, err := io.ReadAll(io.LimitReader(r.Body, (16<<20)+1))
		if err != nil {
			fail(w, fmt.Errorf("read cloud continuation request: %w", err))
			return
		}
		trimmedBody := bytes.TrimSpace(body)
		if len(body) > (16<<20) || len(trimmedBody) > 0 && trimmedBody[0] != '{' {
			fail(w, domain.ErrInvalid)
			return
		}
		if len(trimmedBody) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&continuationRequest); err != nil {
				fail(w, fmt.Errorf("decode cloud continuation instruction: %w", err))
				return
			}
			var trailing any
			if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
				fail(w, fmt.Errorf("decode trailing cloud continuation request data: %w", domain.ErrInvalid))
				return
			}
		}
	}
	if recoveryContinuation && !continuationRequest.ConfirmOldNodeStopped {
		write(w, http.StatusPreconditionRequired, map[string]any{"error": "confirm that the original node has been stopped before creating an independent cloud continuation", "requiresOldNodeStopConfirmation": true})
		return
	}
	if strings.TrimSpace(continuationRequest.Instruction) != "" {
		continuationInstruction = strings.TrimSpace(continuationRequest.Instruction)
	}
	if recoveryContinuation {
		continuationInstruction += "\n\n[恢复证据续接：原节点租约已过期，证据并不证明原进程停止。用户已确认原节点停止；这是独立云端续接段，不恢复或重放原任务。继续前检查当前状态并避免重复外部副作用。]"
	}
	if len(continuationInstruction) > 16<<10 {
		fail(w, domain.ErrInvalid)
		return
	}
	var payload struct {
		SourceConversationID    string `json:"sourceConversationId"`
		Content                 string `json:"content"`
		SourceContextArtifactID string `json:"sourceContextArtifactId"`
		SourceContextSHA256     string `json:"sourceContextSHA256"`
	}
	if err := json.Unmarshal(task.Payload, &payload); err != nil || strings.TrimSpace(payload.Content) == "" || payload.SourceConversationID != result.SourceConversationID || payload.SourceContextArtifactID == "" || len(payload.SourceContextSHA256) != 64 {
		write(w, http.StatusConflict, map[string]string{"error": "task input does not identify a verified cloud conversation snapshot"})
		return
	}
	source, err := s.store.Conversation(r.Context(), s.workspaceID, result.SourceConversationID)
	if err != nil {
		fail(w, err)
		return
	}
	if source.ProjectID != result.ProjectID {
		write(w, http.StatusConflict, map[string]string{"error": "task project identity differs from its cloud source conversation"})
		return
	}
	branchProjectID := source.ProjectID
	if source.ProjectID != "" {
		sourceProject, err := s.store.Project(r.Context(), s.workspaceID, source.ProjectID)
		if err != nil {
			fail(w, err)
			return
		}
		if !strings.EqualFold(sourceProject.ResolvedCommit, result.ProjectCommit) {
			write(w, http.StatusConflict, map[string]string{"error": "cloud source project commit changed after the local task snapshot"})
			return
		}
	}
	links, err := s.store.ExecutionTaskArtifacts(r.Context(), s.workspaceID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	openTaskArtifact := func(role, id, digest string, byteSize int64) (*artifactstore.ArtifactFile, error) {
		for _, link := range links {
			if link.Role != role || link.ID != id {
				continue
			}
			if link.SHA256 != digest || link.ByteSize != byteSize {
				return nil, fmt.Errorf("task %s artifact manifest mismatch: %w", role, domain.ErrConflict)
			}
			artifact, file, err := s.artifacts.Open(r.Context(), s.workspaceID, id)
			if err != nil {
				return nil, err
			}
			if artifact.ID != id || artifact.SHA256 != digest || artifact.ByteSize != byteSize {
				closeErr := file.Close()
				return nil, errors.Join(fmt.Errorf("task %s artifact did not read back with its attached manifest: %w", role, domain.ErrConflict), closeErr)
			}
			return file, nil
		}
		return nil, fmt.Errorf("task is missing its %s artifact: %w", role, domain.ErrNotFound)
	}
	var contextArtifactSize int64
	for _, link := range links {
		if link.ID == payload.SourceContextArtifactID && link.Role == "conversation_context" {
			contextArtifactSize = link.ByteSize
		}
	}
	if contextArtifactSize <= 0 {
		write(w, http.StatusConflict, map[string]string{"error": "cloud source conversation snapshot is not attached to this task"})
		return
	}
	contextFile, err := openTaskArtifact("conversation_context", payload.SourceContextArtifactID, payload.SourceContextSHA256, contextArtifactSize)
	if err != nil {
		fail(w, err)
		return
	}
	var contextSnapshot localTaskConversationContext
	contextDecodeErr := json.NewDecoder(contextFile).Decode(&contextSnapshot)
	contextCloseErr := contextFile.Close()
	if err := errors.Join(contextDecodeErr, contextCloseErr); err != nil {
		fail(w, err)
		return
	}
	if contextSnapshot.Conversation.ID != result.SourceConversationID {
		write(w, http.StatusConflict, map[string]string{"error": "task context snapshot belongs to another source conversation"})
		return
	}
	transcriptRole := "conversation_transcript"
	if recoveryContinuation {
		transcriptRole = "recovery_transcript"
	}
	transcriptFile, err := openTaskArtifact(transcriptRole, result.TranscriptArtifact.ID, result.TranscriptArtifact.SHA256, result.TranscriptArtifact.ByteSize)
	if err != nil {
		fail(w, err)
		return
	}
	var transcript struct {
		SourceConversationID string `json:"sourceConversationId"`
		ConversationID       string `json:"conversationId"`
		AgentTurnID          string `json:"agentTurnId"`
		ResultMessageID      string `json:"resultMessageId"`
		AgentStatus          string `json:"agentStatus"`
		AgentStopReason      string `json:"agentStopReason"`
		ProjectID            string `json:"projectId"`
		ProjectCommit        string `json:"projectCommit"`
		Transcript           struct {
			Conversation domain.ConversationDetail `json:"conversation"`
			Turns        []domain.AgentTurn        `json:"turns"`
			Trace        []domain.TraceEvent       `json:"trace"`
		} `json:"transcript"`
	}
	decodeErr := json.NewDecoder(transcriptFile).Decode(&transcript)
	closeErr := transcriptFile.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil || transcript.SourceConversationID != result.SourceConversationID || transcript.ConversationID != result.ConversationID || transcript.AgentTurnID != result.AgentTurnID || transcript.ResultMessageID != result.ResultMessageID || transcript.AgentStatus != result.AgentStatus || transcript.AgentStopReason != result.AgentStopReason || transcript.ProjectID != result.ProjectID || transcript.ProjectCommit != result.ProjectCommit || transcript.Transcript.Conversation.ID != result.ConversationID {
		if err != nil {
			fail(w, fmt.Errorf("read and clean up verified task transcript: %w", err))
			return
		}
		write(w, http.StatusConflict, map[string]string{"error": "task transcript identity or terminal state does not match the result manifest"})
		return
	}
	if len(transcript.Transcript.Conversation.Messages) != len(contextSnapshot.Conversation.Messages)+2 {
		write(w, http.StatusConflict, map[string]string{"error": "task transcript does not contain the source context and one complete task exchange"})
		return
	}
	for index, original := range contextSnapshot.Conversation.Messages {
		actual := transcript.Transcript.Conversation.Messages[index]
		if actual.Role != original.Role || actual.Content != original.Content {
			write(w, http.StatusConflict, map[string]string{"error": "task transcript does not preserve the cloud source conversation snapshot"})
			return
		}
	}
	messages := transcript.Transcript.Conversation.Messages
	if messages[len(messages)-2].Role != "user" || messages[len(messages)-2].Content != payload.Content || messages[len(messages)-1].Role != "assistant" || messages[len(messages)-1].ID != result.ResultMessageID {
		write(w, http.StatusConflict, map[string]string{"error": "task transcript does not end with the reported user request and assistant result"})
		return
	}
	validTurn := false
	for _, turn := range transcript.Transcript.Turns {
		if turn.ID == result.AgentTurnID && turn.Status == result.AgentStatus && turn.StopReason == result.AgentStopReason && turn.ResultMessageID == result.ResultMessageID && (result.AgentStatus == "completed" || (turn.ContinuationAvailable && safeLocalTaskStopReason(turn.StopReason))) {
			validTurn = true
			break
		}
	}
	if !validTurn {
		write(w, http.StatusConflict, map[string]string{"error": "task transcript has no matching completed or safely resumable Agent turn"})
		return
	}
	externalEffects, err := summarizeTaskHandoffEffects(transcript.Transcript.Trace, transcript.ConversationID, transcript.AgentTurnID)
	if err != nil {
		fail(w, err)
		return
	}
	effectResolutions, resolutionErr := validateTaskHandoffEffectResolutions(externalEffects, continuationRequest.EffectResolutions)
	if resolutionErr != nil {
		write(w, http.StatusPreconditionRequired, map[string]any{
			"error":                   "review every non-read-only tool effect and classify its actual outcome before continuing in the cloud",
			"requiresAcknowledgement": true,
			"effects":                 externalEffects,
		})
		return
	}
	if result.ProjectDeltaCommit != "" {
		if err := s.attachImportedProject(r, &task); err != nil {
			fail(w, err)
			return
		}
		if task.ImportedProject == nil || !strings.EqualFold(task.ImportedProject.ResolvedCommit, result.ProjectDeltaCommit) || result.ProjectCommit != result.ProjectDeltaBaseCommit {
			write(w, http.StatusConflict, map[string]string{"error": "import the task project delta before continuing this task in the cloud"})
			return
		}
		branchProjectID = task.ImportedProject.ID
	}
	if len(externalEffects) > 0 {
		outcomes := make(map[string]string, len(effectResolutions))
		for _, resolution := range effectResolutions {
			outcomes[resolution.ToolCallID] = resolution.Outcome
		}
		parts := make([]string, 0, len(externalEffects))
		for _, effect := range externalEffects {
			outcome := outcomes[effect.ToolCallID]
			guidance := ""
			switch outcome {
			case "confirmed_applied":
				guidance = "用户确认已生效；不得重复执行，后续只核验当前状态。"
			case "confirmed_not_applied":
				guidance = "用户确认未生效；若仍需该操作，可在检查当前状态后执行。"
			default:
				guidance = "结果仍未知；必须先检查外部当前状态，不得盲目重试。"
			}
			parts = append(parts, fmt.Sprintf("%s(%s): %s", effect.ToolCallID, outcome, guidance))
		}
		continuationInstruction += fmt.Sprintf("\n\n[跨节点交接审计：用户逐项核对了本地 trace 中 %d 个非只读工具调用。各项实际结果与后续约束如下：%s]", len(externalEffects), strings.Join(parts, "；"))
	}
	var handoffCheckpoint *agent.HandoffCheckpoint
	if result.HandoffCheckpointArtifact != nil {
		if s.agent == nil {
			write(w, http.StatusServiceUnavailable, map[string]string{"error": "cloud Agent continuation runtime is unavailable"})
			return
		}
		stored, err := s.store.ExecutionTaskHandoffCheckpoint(r.Context(), s.workspaceID, task.ID)
		if err != nil {
			fail(w, err)
			return
		}
		checkpointReferenceMatches := stored.ArtifactID == result.HandoffCheckpointArtifact.ID && stored.ArtifactSHA256 == result.HandoffCheckpointArtifact.SHA256 && stored.ArtifactByteSize == result.HandoffCheckpointArtifact.ByteSize
		if recoveryContinuation && !checkpointReferenceMatches {
			// An active lease may have already rewrapped the same safe checkpoint
			// before the node lost its terminal response. Recovery can reuse that
			// durable copy only when both artifact roles remain task-bound.
			storedCheckpointAttached, recoveryCheckpointAttached := false, false
			for _, link := range links {
				storedCheckpointAttached = storedCheckpointAttached || (link.ID == stored.ArtifactID && link.Role == "continuation_checkpoint" && link.SHA256 == stored.ArtifactSHA256 && link.ByteSize == stored.ArtifactByteSize)
				recoveryCheckpointAttached = recoveryCheckpointAttached || (link.ID == result.HandoffCheckpointArtifact.ID && link.Role == "recovery_checkpoint" && link.SHA256 == result.HandoffCheckpointArtifact.SHA256 && link.ByteSize == result.HandoffCheckpointArtifact.ByteSize)
			}
			checkpointReferenceMatches = storedCheckpointAttached && recoveryCheckpointAttached
		}
		if !checkpointReferenceMatches {
			write(w, http.StatusConflict, map[string]string{"error": "task handoff checkpoint reference differs from its durable record"})
			return
		}
		opened, err := s.agent.OpenStoredHandoffCheckpoint(r.Context(), s.workspaceID, stored)
		if err != nil {
			fail(w, err)
			return
		}
		if opened.SourceTurnID != result.AgentTurnID || opened.SourceConversationID != result.ConversationID || opened.SourceInputMessageID != messages[len(messages)-2].ID || opened.ProviderID != transcript.Transcript.Conversation.ProviderID || opened.GenerationID != transcript.Transcript.Conversation.AgentGenerationID || opened.DefinitionDigest != transcript.Transcript.Conversation.AgentDefinitionDigest || opened.PermissionProfile != transcript.Transcript.Conversation.PermissionProfile || opened.ProjectID != result.ProjectID || opened.ProjectCommit != result.ProjectCommit {
			clear(opened.Checkpoint)
			write(w, http.StatusConflict, map[string]string{"error": "task checkpoint does not match its completed local transcript identity"})
			return
		}
		if err := s.agent.ValidateHandoffCheckpointBindings(r.Context(), s.workspaceID, source.ID, opened); err != nil {
			clear(opened.Checkpoint)
			fail(w, err)
			return
		}
		handoffCheckpoint = &opened
		defer clear(handoffCheckpoint.Checkpoint)
	}
	if source.ProviderID != contextSnapshot.Conversation.ProviderID || source.PermissionProfile != contextSnapshot.Conversation.PermissionProfile || source.ProjectID != contextSnapshot.Conversation.ProjectID || source.AgentGenerationID != contextSnapshot.Conversation.AgentGenerationID || source.AgentDefinitionDigest != contextSnapshot.Conversation.AgentDefinitionDigest || len(source.Messages) < len(contextSnapshot.Conversation.Messages) {
		write(w, http.StatusConflict, map[string]string{"error": "cloud source conversation binding changed after the local task snapshot"})
		return
	}
	for index, original := range contextSnapshot.Conversation.Messages {
		if source.Messages[index].Role != original.Role || source.Messages[index].Content != original.Content || source.Messages[index].ID != original.ID {
			write(w, http.StatusConflict, map[string]string{"error": "cloud source conversation history no longer matches the local task snapshot"})
			return
		}
	}
	branchFromMessageID := ""
	if len(contextSnapshot.Conversation.Messages) > 0 {
		branchFromMessageID = contextSnapshot.Conversation.Messages[len(contextSnapshot.Conversation.Messages)-1].ID
	}
	now := time.Now().UTC()
	// The node's wall clock may drift. Rebase this copied transcript onto a
	// monotonic cloud timeline so later cloud turns always sort after it.
	messages = append([]domain.Message(nil), messages...)
	messageBase := now.Add(-time.Duration(len(messages)) * time.Millisecond)
	for index := range messages {
		messages[index].CreatedAt = messageBase.Add(time.Duration(index) * time.Millisecond)
	}
	branch := domain.Conversation{ID: agent.TaskContinuationConversationID(source.ID, task.ID), UserID: s.workspaceID, Title: strings.TrimSpace(source.Title + " · 云端续接"), ProviderID: source.ProviderID, ProjectID: branchProjectID, PermissionProfile: source.PermissionProfile, ParentConversationID: source.ID, ExecutionPaused: true, CreatedAt: now, UpdatedAt: now}
	continued, err := s.store.CreateTaskContinuationConversation(r.Context(), branch, branchFromMessageID, messages)
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.Conversation(r.Context(), s.workspaceID, continued.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.ProjectID != branchProjectID || readBack.ParentConversationID != source.ID || len(readBack.Messages) < len(messages) || readBack.Messages[len(messages)-1].Content != messages[len(messages)-1].Content {
		write(w, http.StatusConflict, map[string]string{"error": "cloud continuation conversation did not read back at the imported project and local result"})
		return
	}
	var cloudTask *storage.ExecutionTask
	continuationPayload := map[string]any{"kind": "agent_turn", "conversationId": readBack.ID, "content": continuationInstruction}
	if len(inputAttachments) > 0 {
		continuationPayload["attachments"] = inputAttachments
	}
	if len(externalEffects) > 0 {
		continuationPayload["handoffEffectsAcknowledged"] = true
		continuationPayload["handoffEffects"] = externalEffects
		continuationPayload["handoffEffectResolutions"] = effectResolutions
	}
	if recoveryContinuation {
		continuationPayload["recoveryContinuation"] = true
		continuationPayload["oldNodeStoppedConfirmed"] = continuationRequest.ConfirmOldNodeStopped
		continuationPayload["handoffEffectsAcknowledged"] = len(effectResolutions) == len(externalEffects)
	}
	if handoffCheckpoint != nil {
		if result.ProjectDeltaCommit != "" {
			rebound, err := s.agent.RebindHandoffCheckpointForTaskDelta(
				r.Context(), s.workspaceID, task.ID, source.ID, readBack.ID, *handoffCheckpoint,
				result.ProjectDeltaBaseCommit, result.ProjectDeltaCommit,
			)
			if err != nil {
				fail(w, err)
				return
			}
			clear(handoffCheckpoint.Checkpoint)
			*handoffCheckpoint = rebound
		}
		importedTurn, err := s.agent.ImportHandoffContinuation(r.Context(), s.workspaceID, readBack.ID, *handoffCheckpoint, domain.AgentTurn{
			ID: agent.TaskContinuationTurnID(source.ID, task.ID), ConversationID: readBack.ID, UserID: s.workspaceID,
			InputMessageID: readBack.Messages[len(messages)-2].ID, ProviderID: readBack.ProviderID,
			AgentGenerationID: readBack.AgentGenerationID, AgentDefinitionDigest: readBack.AgentDefinitionDigest,
			PermissionProfile: readBack.PermissionProfile, Status: "incomplete", StopReason: result.AgentStopReason,
		}, readBack.Messages[len(messages)-1].ID, now)
		if err != nil {
			fail(w, err)
			return
		}
		continuationPayload = map[string]any{
			"kind": "agent_continuation", "conversationId": readBack.ID, "turnId": importedTurn.ID,
			"content": continuationInstruction, "idempotencyKey": "cloud-task-handoff:" + task.ID,
		}
		if len(inputAttachments) > 0 {
			continuationPayload["attachments"] = inputAttachments
		}
		if len(externalEffects) > 0 {
			continuationPayload["handoffEffectsAcknowledged"] = true
			continuationPayload["handoffEffects"] = externalEffects
			continuationPayload["handoffEffectResolutions"] = effectResolutions
		}
	}
	continuationBytes, err := json.Marshal(continuationPayload)
	if err != nil {
		fail(w, err)
		return
	}
	cloudTaskID := make([]byte, 16)
	if _, err := rand.Read(cloudTaskID); err != nil {
		fail(w, err)
		return
	}
	createdTask, created, err := s.store.CreateCloudHandoffTaskWithArtifacts(r.Context(), storage.ExecutionTask{
		ID: "task_" + hex.EncodeToString(cloudTaskID), UserID: s.workspaceID,
		LogicalTaskID: task.LogicalTaskID, ParentTaskID: task.ID, SegmentIndex: task.SegmentIndex + 1,
		NodeID: storage.CloudExecutionNodeID(s.workspaceID), IdempotencyKey: "cloud-handoff:" + task.ID, Payload: continuationBytes,
	}, readBack.ID, inputArtifactLinks, time.Now().UTC())
	if err != nil {
		fail(w, fmt.Errorf("persist cloud continuation task: %w", err))
		return
	}
	cloudTaskReadBack, err := s.store.ExecutionTask(r.Context(), s.workspaceID, createdTask.ID)
	if err != nil {
		fail(w, fmt.Errorf("read back cloud continuation task: %w", err))
		return
	}
	if cloudTaskReadBack.NodeID != storage.CloudExecutionNodeID(s.workspaceID) || string(cloudTaskReadBack.Payload) != string(continuationBytes) || cloudTaskReadBack.ID != createdTask.ID || cloudTaskReadBack.LogicalTaskID != task.LogicalTaskID || cloudTaskReadBack.ParentTaskID != task.ID || cloudTaskReadBack.SegmentIndex != task.SegmentIndex+1 {
		write(w, http.StatusConflict, map[string]string{"error": "cloud continuation task did not read back as the requested durable task"})
		return
	}
	if err := s.verifyTaskInputArtifactLinks(r.Context(), cloudTaskReadBack.ID, inputAttachments); err != nil {
		fail(w, err)
		return
	}
	cloudTask = &cloudTaskReadBack
	readBack, err = s.store.Conversation(r.Context(), s.workspaceID, continued.ID)
	if err != nil {
		fail(w, fmt.Errorf("read back activated cloud continuation conversation: %w", err))
		return
	}
	if readBack.ExecutionPaused || readBack.ID != continued.ID {
		write(w, http.StatusConflict, map[string]string{"error": "cloud continuation task was queued but its conversation activation did not read back"})
		return
	}
	var project *domain.Project
	if branchProjectID != "" {
		persistedProject, err := s.store.Project(r.Context(), s.workspaceID, branchProjectID)
		if err != nil {
			fail(w, err)
			return
		}
		project = &persistedProject
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	write(w, status, map[string]any{"conversation": readBack, "project": project, "task": cloudTask})
}

func validLocalTaskContinuationState(status, stopReason string) bool {
	if status == "completed" {
		return stopReason == "assistant_response"
	}
	return status == "incomplete" && safeLocalTaskStopReason(stopReason)
}

func safeLocalTaskStopReason(stopReason string) bool {
	return domain.SafeAgentContinuationStopReason(stopReason)
}

func (s *Server) executionTaskEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ExecutionTaskEvents(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, events)
}

func (s *Server) executionTaskCancel(w http.ResponseWriter, r *http.Request) {
	task, err := s.store.CancelExecutionTask(r.Context(), s.workspaceID, r.PathValue("id"), time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionTask(r.Context(), s.workspaceID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.Sequence < task.Sequence || readBack.CancelRequested != task.CancelRequested || readBack.Status != task.Status {
		write(w, http.StatusConflict, map[string]string{"error": "task cancellation read-back does not match persisted state"})
		return
	}
	write(w, http.StatusOK, readBack)
}

func (s *Server) executionTaskHandoffRequest(w http.ResponseWriter, r *http.Request) {
	task, err := s.store.RequestExecutionTaskHandoff(r.Context(), s.workspaceID, r.PathValue("id"), time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionTask(r.Context(), s.workspaceID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if !readBack.HandoffRequested || readBack.Sequence < task.Sequence || readBack.NodeID != task.NodeID {
		write(w, http.StatusConflict, map[string]string{"error": "safe handoff request did not read back from durable task state"})
		return
	}
	write(w, http.StatusOK, readBack)
}

func (s *Server) executionTaskClaim(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := r.Context().Value(executionNodeContextKey{}).(string)
	if !ok || nodeID == "" {
		write(w, http.StatusUnauthorized, map[string]string{"error": "node credential required"})
		return
	}
	leaseMaterial := make([]byte, 32)
	if _, err := rand.Read(leaseMaterial); err != nil {
		fail(w, err)
		return
	}
	leaseToken := base64.RawURLEncoding.EncodeToString(leaseMaterial)
	now := time.Now().UTC()
	leaseUntil := now.Add(60 * time.Second)
	task, err := s.store.ClaimExecutionTask(r.Context(), nodeID, hashToken(leaseToken), leaseUntil, now)
	if errors.Is(err, domain.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionTask(r.Context(), task.UserID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.Status != "leased" || readBack.LeaseUntil == nil || !readBack.LeaseUntil.Equal(leaseUntil) {
		write(w, http.StatusConflict, map[string]string{"error": "claimed task lease did not read back"})
		return
	}
	write(w, http.StatusOK, map[string]any{"task": readBack, "leaseToken": leaseToken, "leaseUntil": leaseUntil})
}

func (s *Server) executionTaskReport(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := r.Context().Value(executionNodeContextKey{}).(string)
	if !ok || nodeID == "" {
		write(w, http.StatusUnauthorized, map[string]string{"error": "node credential required"})
		return
	}
	leaseToken := r.Header.Get("X-O-Task-Lease")
	if leaseToken == "" {
		write(w, http.StatusUnauthorized, map[string]string{"error": "task lease credential required"})
		return
	}
	var input struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if !decode(w, r, &input) {
		return
	}
	task, err := s.store.ReportExecutionTask(r.Context(), nodeID, r.PathValue("id"), hashToken(leaseToken), input.Status, input.Result, input.Error, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionTask(r.Context(), task.UserID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.Status != task.Status || readBack.Sequence != task.Sequence {
		write(w, http.StatusConflict, map[string]string{"error": "task event did not read back at the committed sequence"})
		return
	}
	write(w, http.StatusOK, readBack)
}

func (s *Server) executionTaskPulse(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := r.Context().Value(executionNodeContextKey{}).(string)
	if !ok || nodeID == "" {
		write(w, http.StatusUnauthorized, map[string]string{"error": "node credential required"})
		return
	}
	leaseToken := r.Header.Get("X-O-Task-Lease")
	if leaseToken == "" {
		write(w, http.StatusUnauthorized, map[string]string{"error": "task lease credential required"})
		return
	}
	var input struct {
		ProgressPhase string `json:"progressPhase"`
	}
	if !decode(w, r, &input) {
		return
	}
	task, err := s.store.PulseExecutionTaskWithPhase(r.Context(), nodeID, r.PathValue("id"), hashToken(leaseToken), input.ProgressPhase, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionTask(r.Context(), task.UserID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.LeaseUntil == nil || task.LeaseUntil == nil || !readBack.LeaseUntil.Equal(*task.LeaseUntil) {
		write(w, http.StatusConflict, map[string]string{"error": "task lease renewal did not read back"})
		return
	}
	if strings.TrimSpace(input.ProgressPhase) != "" && readBack.ProgressPhase != strings.TrimSpace(input.ProgressPhase) {
		write(w, http.StatusConflict, map[string]string{"error": "task progress phase did not read back"})
		return
	}
	write(w, http.StatusOK, readBack)
}
