package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/exechandoff"
	"axiom.local/agent/internal/storage"
)

type executionNodeContextKey struct{}

func (s *Server) executionNodeRoutes(mux *http.ServeMux) {
	s.nodeWebSocketRoute(mux)
	mux.HandleFunc("GET /api/v1/nodes", s.executionNodeList)
	mux.HandleFunc("POST /api/v1/nodes", s.executionNodePair)
	mux.HandleFunc("DELETE /api/v1/nodes/{id}", s.executionNodeRevoke)
	mux.HandleFunc("POST /api/v1/nodes/heartbeat", s.executionNodeHeartbeat)
	mux.HandleFunc("POST /api/v1/nodes/tasks/claim", s.executionTaskClaim)
	mux.HandleFunc("POST /api/v1/nodes/tasks/{id}/report", s.executionTaskReport)
	mux.HandleFunc("POST /api/v1/nodes/tasks/{id}/pulse", s.executionTaskPulse)
	mux.HandleFunc("POST /api/v1/nodes/tasks/{id}/recovery-evidence/result", s.executionNodeTaskRecoveryResult)
	mux.HandleFunc("POST /api/v1/nodes/tasks/{id}/recovery-evidence/checkpoint", s.executionNodeRecoveryHandoffCheckpoint)
	mux.HandleFunc("GET /api/v1/nodes/tasks/{id}/status", s.executionNodeTaskStatus)
	mux.HandleFunc("POST /api/v1/nodes/tasks/{id}/handoff-checkpoint", s.executionNodeHandoffCheckpoint)
	mux.HandleFunc("GET /api/v1/tasks", s.executionTaskList)
	mux.HandleFunc("POST /api/v1/tasks", s.executionTaskSubmit)
	mux.HandleFunc("POST /api/v1/conversations/{id}/tasks/cloud", s.cloudAgentTaskSubmit)
	mux.HandleFunc("POST /api/v1/conversations/{id}/tasks/nodes", s.nodeAgentTaskSubmit)
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.executionTaskGet)
	mux.HandleFunc("GET /api/v1/tasks/{id}/continuation-preview", s.executionTaskHandoffPreview)
	mux.HandleFunc("POST /api/v1/tasks/{id}/import-project-delta", s.executionTaskImportProjectDelta)
	mux.HandleFunc("POST /api/v1/tasks/{id}/continue-in-cloud", s.executionTaskContinueInCloud)
	mux.HandleFunc("GET /api/v1/tasks/{id}/events", s.executionTaskEvents)
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.executionTaskCancel)
	mux.HandleFunc("POST /api/v1/tasks/{id}/handoff", s.executionTaskHandoffRequest)
}

func (s *Server) executionNodeTaskRecoveryResult(w http.ResponseWriter, r *http.Request) {
	task, nodeID, err := s.validateNodeTaskRecoveryLease(r)
	if err != nil {
		fail(w, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		fail(w, err)
		return
	}
	if len(body) > 1<<20 || !json.Valid(body) {
		fail(w, domain.ErrInvalid)
		return
	}
	if task.NodeID != nodeID {
		fail(w, domain.ErrUnauthorized)
		return
	}
	persisted, err := s.store.SaveExecutionTaskRecoveryResult(r.Context(), nodeID, task.ID, hashToken(strings.TrimSpace(r.Header.Get("X-O-Task-Lease"))), body, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, persisted)
}

func (s *Server) executionNodeTaskStatus(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := r.Context().Value(executionNodeContextKey{}).(string)
	taskID := strings.TrimSpace(r.PathValue("id"))
	if nodeID == "" || taskID == "" {
		write(w, http.StatusUnauthorized, map[string]string{"error": "active node credential required"})
		return
	}
	task, err := s.store.ExecutionTaskForNode(r.Context(), nodeID, taskID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, task)
}

func (s *Server) executionNodeHandoffCheckpoint(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil || s.agent == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "handoff checkpoint storage is unavailable"})
		return
	}
	task, nodeID, err := s.validateNodeTaskLease(r)
	if err != nil {
		fail(w, err)
		return
	}
	var input struct {
		ArtifactID string `json:"artifactId"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.ArtifactID = strings.TrimSpace(input.ArtifactID)
	if input.ArtifactID == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	s.persistExecutionNodeHandoffCheckpoint(w, r, task, nodeID, input.ArtifactID, false)
}

func (s *Server) executionNodeRecoveryHandoffCheckpoint(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil || s.agent == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "recovery checkpoint storage is unavailable"})
		return
	}
	task, nodeID, err := s.validateNodeTaskRecoveryLease(r)
	if err != nil {
		fail(w, err)
		return
	}
	var input struct {
		ArtifactID string `json:"artifactId"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.ArtifactID = strings.TrimSpace(input.ArtifactID)
	if input.ArtifactID == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	s.persistExecutionNodeHandoffCheckpoint(w, r, task, nodeID, input.ArtifactID, true)
}

func (s *Server) persistExecutionNodeHandoffCheckpoint(w http.ResponseWriter, r *http.Request, task storage.ExecutionTask, nodeID, artifactID string, recovery bool) {
	links, err := s.store.ExecutionTaskArtifacts(r.Context(), task.UserID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	var manifest *storage.ExecutionTaskArtifact
	checkpointRole := "continuation_checkpoint"
	if recovery {
		checkpointRole = "recovery_checkpoint"
	}
	for index := range links {
		if links[index].ID == artifactID && links[index].Role == checkpointRole {
			manifest = &links[index]
			break
		}
	}
	if manifest == nil || manifest.ByteSize <= 0 || manifest.ByteSize > exechandoff.MaxPayloadBytes+(1<<20) {
		write(w, http.StatusConflict, map[string]string{"error": "task does not contain the declared checkpoint artifact"})
		return
	}
	artifact, file, err := s.artifacts.Open(r.Context(), task.UserID, artifactID)
	if err != nil {
		fail(w, err)
		return
	}
	content, readErr := io.ReadAll(io.LimitReader(file, exechandoff.MaxPayloadBytes+(1<<20)+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		fail(w, err)
		return
	}
	digest := sha256.Sum256(content)
	if int64(len(content)) != artifact.ByteSize || artifact.ID != manifest.ID || artifact.SHA256 != manifest.SHA256 || artifact.ByteSize != manifest.ByteSize || hex.EncodeToString(digest[:]) != manifest.SHA256 {
		write(w, http.StatusConflict, map[string]string{"error": "checkpoint artifact failed task-bound content verification"})
		return
	}
	lease := strings.TrimSpace(r.Header.Get("X-O-Task-Lease"))
	plain, err := exechandoff.Open(content, task.ID, nodeID, lease)
	if err != nil {
		fail(w, err)
		return
	}
	defer clear(plain)
	var checkpoint agent.HandoffCheckpoint
	if err := json.Unmarshal(plain, &checkpoint); err != nil {
		fail(w, fmt.Errorf("decode lease-authenticated Agent checkpoint: %w", err))
		return
	}
	defer clear(checkpoint.Checkpoint)
	var taskInput struct {
		SourceConversationID string `json:"sourceConversationId"`
	}
	if err := json.Unmarshal(task.Payload, &taskInput); err != nil || taskInput.SourceConversationID == "" || checkpoint.SourceTurnID == "" || checkpoint.SourceConversationID == "" || checkpoint.SourceInputMessageID == "" {
		write(w, http.StatusConflict, map[string]string{"error": "checkpoint or task is missing its source identity"})
		return
	}
	if checkpoint.ProviderID == "" || checkpoint.GenerationID == "" || checkpoint.DefinitionDigest == "" || !checkpoint.PermissionProfile.Valid() {
		write(w, http.StatusConflict, map[string]string{"error": "checkpoint runtime binding is incomplete"})
		return
	}
	cloudSource, err := s.store.Conversation(r.Context(), task.UserID, taskInput.SourceConversationID)
	if err != nil {
		fail(w, err)
		return
	}
	if checkpoint.ProviderID != cloudSource.ProviderID || checkpoint.GenerationID != cloudSource.AgentGenerationID || checkpoint.DefinitionDigest != cloudSource.AgentDefinitionDigest || checkpoint.PermissionProfile != cloudSource.PermissionProfile {
		write(w, http.StatusConflict, map[string]string{"error": "local checkpoint runtime does not match the cloud conversation binding"})
		return
	}
	projectID, projectCommit := "", ""
	if cloudSource.ProjectID != "" {
		project, err := s.store.Project(r.Context(), task.UserID, cloudSource.ProjectID)
		if err != nil {
			fail(w, err)
			return
		}
		projectID, projectCommit = project.ID, project.ResolvedCommit
	}
	if checkpoint.ProjectID != projectID || checkpoint.ProjectCommit != projectCommit {
		write(w, http.StatusConflict, map[string]string{"error": "local checkpoint project binding differs from the cloud task baseline"})
		return
	}
	if err := s.agent.ValidateHandoffCheckpointBindings(r.Context(), task.UserID, taskInput.SourceConversationID, checkpoint); err != nil {
		fail(w, err)
		return
	}
	ciphertext, nonce, err := s.agent.ReencryptHandoffCheckpoint(r.Context(), task.UserID, checkpoint)
	if err != nil {
		fail(w, err)
		return
	}
	leaseHash := hashToken(lease)
	handoff := storage.ExecutionTaskHandoffCheckpoint{
		TaskID: task.ID, NodeID: nodeID, ArtifactID: artifact.ID, ArtifactSHA256: artifact.SHA256, ArtifactByteSize: artifact.ByteSize,
		SourceTurnID: checkpoint.SourceTurnID, SourceConversationID: checkpoint.SourceConversationID, SourceInputMessageID: checkpoint.SourceInputMessageID,
		ProviderID: checkpoint.ProviderID, GenerationID: checkpoint.GenerationID, DefinitionDigest: checkpoint.DefinitionDigest,
		PermissionProfile: checkpoint.PermissionProfile, ProjectID: checkpoint.ProjectID, ProjectCommit: checkpoint.ProjectCommit,
		ContentSHA256: checkpoint.ContentSHA256, Version: 1, Ciphertext: ciphertext, Nonce: nonce,
	}
	var readBack storage.ExecutionTaskHandoffCheckpoint
	if recovery {
		readBack, err = s.store.SaveRecoveredExecutionTaskHandoffCheckpoint(r.Context(), handoff, leaseHash, time.Now().UTC())
	} else {
		readBack, err = s.store.SaveExecutionTaskHandoffCheckpoint(r.Context(), handoff, leaseHash, time.Now().UTC())
	}
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.TaskID != task.ID || readBack.NodeID != nodeID || (!recovery && (readBack.ArtifactID != artifact.ID || readBack.ArtifactSHA256 != artifact.SHA256 || readBack.ArtifactByteSize != artifact.ByteSize)) || readBack.ContentSHA256 != checkpoint.ContentSHA256 || len(readBack.Ciphertext) == 0 || len(readBack.Nonce) == 0 {
		write(w, http.StatusConflict, map[string]string{"error": "cloud-encrypted handoff checkpoint did not read back"})
		return
	}
	readBack.Ciphertext = nil
	readBack.Nonce = nil
	write(w, http.StatusCreated, readBack)
}

type executionNodeResponse struct {
	ID           string                         `json:"id"`
	Name         string                         `json:"name"`
	Platform     string                         `json:"platform"`
	Capabilities []string                       `json:"capabilities"`
	Resources    storage.ExecutionNodeResources `json:"resources"`
	Connectivity string                         `json:"connectivity"`
	LastSeen     *time.Time                     `json:"lastSeen,omitempty"`
	RevokedAt    *time.Time                     `json:"revokedAt,omitempty"`
	CreatedAt    time.Time                      `json:"createdAt"`
	UpdatedAt    time.Time                      `json:"updatedAt"`
}

func nodeResponse(node storage.ExecutionNode, now time.Time) executionNodeResponse {
	connectivity := "disconnected"
	if node.RevokedAt != nil {
		connectivity = "revoked"
	} else if node.LastSeen != nil && now.Sub(*node.LastSeen) <= 90*time.Second {
		connectivity = "connected"
	}
	return executionNodeResponse{ID: node.ID, Name: node.Name, Platform: node.Platform, Capabilities: node.Capabilities, Resources: node.Resources, Connectivity: connectivity, LastSeen: node.LastSeen, RevokedAt: node.RevokedAt, CreatedAt: node.CreatedAt, UpdatedAt: node.UpdatedAt}
}

func (s *Server) executionNodeList(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.ExecutionNodes(r.Context(), s.workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	now := time.Now().UTC()
	result := make([]executionNodeResponse, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, nodeResponse(node, now))
	}
	write(w, http.StatusOK, result)
}

func (s *Server) executionNodePair(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name         string   `json:"name"`
		Platform     string   `json:"platform"`
		Capabilities []string `json:"capabilities"`
	}
	if !decode(w, r, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	platform := strings.ToLower(strings.TrimSpace(input.Platform))
	capabilities, err := normalizeNodeCapabilities(input.Capabilities)
	if err != nil || name == "" || len(name) > 100 || !validNodePlatform(platform) {
		fail(w, domain.ErrInvalid)
		return
	}
	material := make([]byte, 12)
	if _, err := rand.Read(material); err != nil {
		fail(w, err)
		return
	}
	nodeID := "node_" + hex.EncodeToString(material)
	tokenMaterial := make([]byte, 32)
	if _, err := rand.Read(tokenMaterial); err != nil {
		fail(w, err)
		return
	}
	credential := base64.RawURLEncoding.EncodeToString(tokenMaterial)
	node, err := s.store.RegisterExecutionNode(r.Context(), storage.ExecutionNode{ID: nodeID, UserID: s.workspaceID, Name: name, Platform: platform, Capabilities: capabilities}, hashToken(credential))
	if err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionNode(r.Context(), s.workspaceID, node.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.ID != node.ID || readBack.Name != name || readBack.RevokedAt != nil {
		write(w, http.StatusConflict, map[string]string{"error": "paired node did not read back in the active state"})
		return
	}
	write(w, http.StatusCreated, map[string]any{"node": nodeResponse(readBack, time.Now().UTC()), "credential": credential, "credentialShownOnce": true})
}

func (s *Server) executionNodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := r.Context().Value(executionNodeContextKey{}).(string)
	if nodeID == "" {
		write(w, http.StatusUnauthorized, map[string]string{"error": "node credential required"})
		return
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var input struct {
		Platform     string                          `json:"platform"`
		Capabilities []string                        `json:"capabilities"`
		Resources    *storage.ExecutionNodeResources `json:"resources,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	platform := strings.ToLower(strings.TrimSpace(input.Platform))
	capabilities, err := normalizeNodeCapabilities(input.Capabilities)
	if err != nil || !validNodePlatform(platform) {
		fail(w, domain.ErrInvalid)
		return
	}
	resources := storage.ExecutionNodeResources{MaxConcurrentTasks: 1}
	if input.Resources != nil {
		resources = *input.Resources
	}
	node, err := s.store.HeartbeatExecutionNodeWithResources(r.Context(), hashToken(provided), platform, capabilities, resources, time.Now().UTC())
	if err != nil {
		if errors.Is(err, domain.ErrUnauthorized) {
			write(w, http.StatusUnauthorized, map[string]string{"error": "node credential was revoked during heartbeat"})
		} else {
			fail(w, err)
		}
		return
	}
	if node.ID != nodeID {
		write(w, http.StatusConflict, map[string]string{"error": "node identity changed during heartbeat"})
		return
	}
	readBack, err := s.store.ExecutionNode(r.Context(), node.UserID, node.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.LastSeen == nil || !readBack.LastSeen.Equal(*node.LastSeen) || readBack.Platform != platform || readBack.Resources != resources {
		write(w, http.StatusConflict, map[string]string{"error": "heartbeat did not read back as current node state"})
		return
	}
	write(w, http.StatusOK, nodeResponse(readBack, time.Now().UTC()))
}

func (s *Server) executionNodeRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	if id == storage.CloudExecutionNodeID(s.workspaceID) {
		write(w, http.StatusConflict, map[string]string{"error": "the built-in cloud execution node is managed by the server"})
		return
	}
	if err := s.store.RevokeExecutionNode(r.Context(), s.workspaceID, id, time.Now().UTC()); err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.ExecutionNode(r.Context(), s.workspaceID, id)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.RevokedAt == nil {
		write(w, http.StatusConflict, map[string]string{"error": "node revocation did not read back"})
		return
	}
	s.disconnectExecutionNode(id)
	write(w, http.StatusOK, nodeResponse(readBack, time.Now().UTC()))
}

func normalizeNodeCapabilities(capabilities []string) ([]string, error) {
	if len(capabilities) > 128 {
		return nil, domain.ErrInvalid
	}
	result := make([]string, 0, len(capabilities))
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		value := strings.TrimSpace(capability)
		if value == "" || len(value) > 128 {
			return nil, domain.ErrInvalid
		}
		if _, ok := seen[value]; ok {
			return nil, domain.ErrInvalid
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func validNodePlatform(platform string) bool {
	return platform == "windows" || platform == "linux" || platform == "darwin" || platform == "other"
}
