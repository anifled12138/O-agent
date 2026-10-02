package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func (s *Server) artifactRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/system/artifact-storage", s.artifactStorageCapacity)
	mux.HandleFunc("POST /api/v1/artifacts/uploads", s.artifactUploadBegin)
	mux.HandleFunc("GET /api/v1/artifacts/uploads/{id}", s.artifactUploadStatus)
	mux.HandleFunc("PUT /api/v1/artifacts/uploads/{id}/chunks/{index}", s.artifactUploadChunk)
	mux.HandleFunc("POST /api/v1/artifacts/uploads/{id}/finalize", s.artifactUploadFinalize)
	mux.HandleFunc("GET /api/v1/artifacts/{id}", s.artifactDownload)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/share-links", s.artifactShareLinkCreate)
	mux.HandleFunc("GET /api/v1/artifact-share-links", s.artifactShareLinkList)
	mux.HandleFunc("DELETE /api/v1/artifact-share-links/{id}", s.artifactShareLinkRevoke)
	mux.HandleFunc("GET /api/v1/shared/artifacts/{id}", s.sharedArtifactDownload)
	mux.HandleFunc("GET /api/v1/tasks/{id}/artifacts", s.executionTaskArtifactsList)
	mux.HandleFunc("POST /api/v1/tasks/{id}/artifacts", s.executionTaskArtifactAttach)
	mux.HandleFunc("GET /api/v1/nodes/tasks/{id}/artifacts", s.nodeTaskArtifactsList)
	mux.HandleFunc("GET /api/v1/nodes/tasks/{id}/artifacts/{artifactID}", s.nodeTaskArtifactDownload)
	mux.HandleFunc("GET /api/v1/nodes/tasks/{id}/artifact-downloads/{artifactID}/ticket", s.nodeTaskArtifactDownloadURL)
	mux.HandleFunc("POST /api/v1/nodes/tasks/{id}/artifacts/uploads", s.nodeTaskArtifactUploadBegin)
	mux.HandleFunc("GET /api/v1/nodes/tasks/{id}/artifacts/uploads/{uploadID}", s.nodeTaskArtifactUploadStatus)
	mux.HandleFunc("PUT /api/v1/nodes/tasks/{id}/artifacts/uploads/{uploadID}/chunks/{index}", s.nodeTaskArtifactUploadChunk)
	mux.HandleFunc("POST /api/v1/nodes/tasks/{id}/artifacts/uploads/{uploadID}/finalize", s.nodeTaskArtifactUploadFinalize)
	mux.HandleFunc("POST /api/v1/nodes/tasks/{id}/recovery-evidence/artifacts/uploads", s.nodeTaskRecoveryArtifactUploadBegin)
	mux.HandleFunc("GET /api/v1/nodes/tasks/{id}/recovery-evidence/artifacts/uploads/{uploadID}", s.nodeTaskRecoveryArtifactUploadStatus)
	mux.HandleFunc("PUT /api/v1/nodes/tasks/{id}/recovery-evidence/artifacts/uploads/{uploadID}/chunks/{index}", s.nodeTaskRecoveryArtifactUploadChunk)
	mux.HandleFunc("POST /api/v1/nodes/tasks/{id}/recovery-evidence/artifacts/uploads/{uploadID}/finalize", s.nodeTaskRecoveryArtifactUploadFinalize)
}

const (
	defaultArtifactShareLinkTTL = time.Hour
	minArtifactShareLinkTTL     = 5 * time.Minute
	maxArtifactShareLinkTTL     = 7 * 24 * time.Hour
)

type artifactShareLinkResponse struct {
	Link  storage.ArtifactShareLink `json:"link"`
	Token string                    `json:"token,omitempty"`
	Path  string                    `json:"path,omitempty"`
}

func (s *Server) artifactShareLinkCreate(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	var input struct {
		ExpiresInSeconds int64 `json:"expiresInSeconds,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	ttl := defaultArtifactShareLinkTTL
	if input.ExpiresInSeconds != 0 {
		if input.ExpiresInSeconds < int64(minArtifactShareLinkTTL/time.Second) || input.ExpiresInSeconds > int64(maxArtifactShareLinkTTL/time.Second) {
			fail(w, domain.ErrInvalid)
			return
		}
		ttl = time.Duration(input.ExpiresInSeconds) * time.Second
	}
	artifactID := r.PathValue("id")
	artifact, file, err := s.artifacts.Open(r.Context(), s.workspaceID, artifactID)
	if err != nil {
		fail(w, err)
		return
	}
	if err := file.Close(); err != nil {
		fail(w, err)
		return
	}
	now := time.Now().UTC()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		fail(w, err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		fail(w, err)
		return
	}
	link, err := s.store.CreateArtifactShareLink(r.Context(), s.workspaceID, artifact.ID, "share_"+hex.EncodeToString(idBytes), hashToken(token), now.Add(ttl), now)
	if err != nil {
		fail(w, err)
		return
	}
	path := "/api/v1/shared/artifacts/" + url.PathEscape(artifact.ID) + "?token=" + url.QueryEscape(token)
	// The raw bearer appears only in this create response. It is not persisted
	// and should not be copied into a referrer header by consumers.
	write(w, http.StatusCreated, artifactShareLinkResponse{Link: link, Token: token, Path: path})
}

func (s *Server) artifactShareLinkList(w http.ResponseWriter, r *http.Request) {
	links, err := s.store.ArtifactShareLinks(r.Context(), s.workspaceID, strings.TrimSpace(r.URL.Query().Get("artifactId")))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, links)
}

func (s *Server) artifactShareLinkRevoke(w http.ResponseWriter, r *http.Request) {
	link, err := s.store.RevokeArtifactShareLink(r.Context(), s.workspaceID, r.PathValue("id"), time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, link)
}

func (s *Server) sharedArtifactDownload(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	token := r.URL.Query().Get("token")
	if len(token) < 32 || len(token) > 128 {
		fail(w, domain.ErrNotFound)
		return
	}
	manifest, err := s.store.ArtifactByShareToken(r.Context(), hashToken(token), time.Now().UTC())
	if err != nil || manifest.ID != r.PathValue("id") {
		fail(w, domain.ErrNotFound)
		return
	}
	artifact, file, err := s.artifacts.Open(r.Context(), manifest.UserID, manifest.ID)
	if err != nil {
		fail(w, err)
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			slog.Error("shared artifact download cleanup failed", "artifact_id", artifact.ID, "error", closeErr)
		}
	}()
	if artifact.SHA256 != manifest.SHA256 || artifact.ByteSize != manifest.ByteSize {
		fail(w, domain.ErrConflict)
		return
	}
	setArtifactDownloadHeaders(w, artifact.FileName, artifact.MediaType, artifact.SHA256, artifact.ByteSize)
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.ServeContent(w, r, artifact.FileName, artifact.CreatedAt, file)
}

func (s *Server) artifactStorageCapacity(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	capacity, err := s.artifacts.Capacity(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, capacity)
}

func (s *Server) validateNodeTaskLease(r *http.Request) (storage.ExecutionTask, string, error) {
	nodeID, _ := r.Context().Value(executionNodeContextKey{}).(string)
	lease := strings.TrimSpace(r.Header.Get("X-O-Task-Lease"))
	if nodeID == "" || lease == "" {
		return storage.ExecutionTask{}, "", domain.ErrUnauthorized
	}
	task, err := s.store.ValidateExecutionTaskLease(r.Context(), nodeID, r.PathValue("id"), hashToken(lease), time.Now().UTC())
	return task, nodeID, err
}

func (s *Server) validateNodeTaskRecoveryLease(r *http.Request) (storage.ExecutionTask, string, error) {
	nodeID, _ := r.Context().Value(executionNodeContextKey{}).(string)
	lease := strings.TrimSpace(r.Header.Get("X-O-Task-Lease"))
	if nodeID == "" || lease == "" {
		return storage.ExecutionTask{}, "", domain.ErrUnauthorized
	}
	task, err := s.store.ValidateExecutionTaskRecoveryLease(r.Context(), nodeID, r.PathValue("id"), hashToken(lease))
	return task, nodeID, err
}

func (s *Server) nodeTaskArtifactsList(w http.ResponseWriter, r *http.Request) {
	task, _, err := s.validateNodeTaskLease(r)
	if err != nil {
		fail(w, err)
		return
	}
	items, err := s.store.ExecutionTaskArtifacts(r.Context(), task.UserID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) nodeTaskArtifactDownload(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	task, _, err := s.validateNodeTaskLease(r)
	if err != nil {
		fail(w, err)
		return
	}
	items, err := s.store.ExecutionTaskArtifacts(r.Context(), task.UserID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	artifactID := r.PathValue("artifactID")
	var manifest *storage.Artifact
	for i := range items {
		if items[i].ID == artifactID {
			manifest = &items[i].Artifact
			break
		}
	}
	if manifest == nil {
		fail(w, domain.ErrNotFound)
		return
	}
	artifact, file, err := s.artifacts.Open(r.Context(), task.UserID, artifactID)
	if err != nil {
		fail(w, err)
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			slog.Error("task artifact download cleanup failed", "artifact_id", artifact.ID, "task_id", task.ID, "error", closeErr)
		}
	}()
	if artifact.SHA256 != manifest.SHA256 || artifact.ByteSize != manifest.ByteSize {
		fail(w, domain.ErrConflict)
		return
	}
	mediaType := artifact.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(artifact.FileName)))
	w.Header().Set("X-Artifact-SHA256", artifact.SHA256)
	w.Header().Set("X-Artifact-Byte-Size", strconv.FormatInt(artifact.ByteSize, 10))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, artifact.FileName, artifact.CreatedAt, file)
}

type nodeTaskArtifactDownloadTicket struct {
	TaskID     string    `json:"taskId"`
	ArtifactID string    `json:"artifactId"`
	SHA256     string    `json:"sha256"`
	ByteSize   int64     `json:"byteSize"`
	Direct     bool      `json:"direct"`
	URL        string    `json:"url,omitempty"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
}

func (s *Server) nodeTaskArtifactDownloadURL(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	task, _, err := s.validateNodeTaskLease(r)
	if err != nil {
		fail(w, err)
		return
	}
	items, err := s.store.ExecutionTaskArtifacts(r.Context(), task.UserID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	artifactID := r.PathValue("artifactID")
	var attached *storage.ExecutionTaskArtifact
	for index := range items {
		if items[index].ID == artifactID {
			attached = &items[index]
			break
		}
	}
	if attached == nil {
		fail(w, domain.ErrNotFound)
		return
	}
	artifact, presigned, direct, err := s.artifacts.CreatePresignedDownloadURL(r.Context(), task.UserID, artifactID, 5*time.Minute)
	if err != nil {
		fail(w, err)
		return
	}
	if artifact.ID != attached.ID || artifact.SHA256 != attached.SHA256 || artifact.ByteSize != attached.ByteSize || artifact.ByteSize < 0 {
		fail(w, domain.ErrConflict)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	write(w, http.StatusOK, nodeTaskArtifactDownloadTicket{TaskID: task.ID, ArtifactID: artifact.ID, SHA256: artifact.SHA256, ByteSize: artifact.ByteSize, Direct: direct, URL: presigned.URL, ExpiresAt: presigned.ExpiresAt})
}

func nodeTaskArtifactKeyPrefix(nodeID, taskID string) string {
	return "node-task-artifact:" + nodeID + ":" + taskID + ":"
}

func (s *Server) nodeTaskArtifactUploadBegin(w http.ResponseWriter, r *http.Request) {
	s.nodeTaskArtifactUploadBeginMode(w, r, false)
}

func (s *Server) nodeTaskRecoveryArtifactUploadBegin(w http.ResponseWriter, r *http.Request) {
	s.nodeTaskArtifactUploadBeginMode(w, r, true)
}

func (s *Server) nodeTaskArtifactUploadBeginMode(w http.ResponseWriter, r *http.Request, recovery bool) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	var task storage.ExecutionTask
	var nodeID string
	var err error
	if recovery {
		task, nodeID, err = s.validateNodeTaskRecoveryLease(r)
	} else {
		task, nodeID, err = s.validateNodeTaskLease(r)
	}
	if err != nil {
		fail(w, err)
		return
	}
	clientKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !validNodeArtifactClientKey(clientKey) {
		fail(w, domain.ErrInvalid)
		return
	}
	var input struct {
		FileName       string `json:"fileName"`
		MediaType      string `json:"mediaType"`
		ExpectedSize   int64  `json:"expectedSize"`
		ExpectedSHA256 string `json:"expectedSha256"`
		Role           string `json:"role,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.ExpectedSHA256) != 64 {
		fail(w, domain.ErrInvalid)
		return
	}
	if _, err := hex.DecodeString(input.ExpectedSHA256); err != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	if recovery {
		key, ok := recoveryEvidenceIdempotencyKey(input.Role)
		if !ok || clientKey != key {
			fail(w, domain.ErrInvalid)
			return
		}
	}
	upload, created, err := s.artifacts.Begin(r.Context(), task.UserID, artifactstore.BeginRequest{FileName: input.FileName, MediaType: input.MediaType, ExpectedSize: input.ExpectedSize, ExpectedSHA256: input.ExpectedSHA256, IdempotencyKey: nodeTaskArtifactKeyPrefix(nodeID, task.ID) + clientKey}, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, _, _, err := s.artifacts.Status(r.Context(), task.UserID, upload.ID, 0, 1)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.ID != upload.ID || readBack.Status != upload.Status || !strings.HasPrefix(readBack.IdempotencyKey, nodeTaskArtifactKeyPrefix(nodeID, task.ID)) {
		write(w, http.StatusConflict, map[string]string{"error": "task artifact upload did not read back for this node and task"})
		return
	}
	var directCapability artifactstore.PresignedArtifactUpload
	var direct bool
	readBack, directCapability, direct, err = s.artifacts.CreateDirectUploadCapability(r.Context(), task.UserID, upload.ID, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.ID != upload.ID || readBack.Status != upload.Status || (direct && !readBack.DirectUpload) || (readBack.Status == "uploading" && readBack.DirectUpload && !direct) {
		write(w, http.StatusConflict, map[string]string{"error": "task artifact direct-upload mode did not read back"})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	response := map[string]any{"upload": readBack, "created": created, "chunkSize": artifactstore.ChunkSize, "directUpload": direct}
	if direct {
		response["uploadURL"] = directCapability.URL
		response["requiredHeaders"] = directCapability.RequiredHeaders
		response["expiresAt"] = directCapability.ExpiresAt
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
	}
	write(w, status, response)
}

func (s *Server) nodeTaskArtifactUploadChunk(w http.ResponseWriter, r *http.Request) {
	s.nodeTaskArtifactUploadChunkMode(w, r, false)
}

func (s *Server) nodeTaskRecoveryArtifactUploadChunk(w http.ResponseWriter, r *http.Request) {
	s.nodeTaskArtifactUploadChunkMode(w, r, true)
}

func (s *Server) nodeTaskArtifactUploadChunkMode(w http.ResponseWriter, r *http.Request, recovery bool) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	var task storage.ExecutionTask
	var nodeID string
	var err error
	if recovery {
		task, nodeID, err = s.validateNodeTaskRecoveryLease(r)
	} else {
		task, nodeID, err = s.validateNodeTaskLease(r)
	}
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.validateNodeArtifactUpload(r, task.UserID, nodeID, task.ID); err != nil {
		fail(w, err)
		return
	}
	index, err := strconv.ParseInt(r.PathValue("index"), 10, 64)
	if err != nil || index < 0 {
		fail(w, domain.ErrInvalid)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, artifactstore.ChunkSize+1)
	chunk, err := s.artifacts.Upload(r.Context(), task.UserID, r.PathValue("uploadID"), index, r.Header.Get("X-Chunk-SHA256"), r.Body, time.Now().UTC())
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			write(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "chunk exceeds the 8 MiB transfer batch size"})
			return
		}
		fail(w, err)
		return
	}
	readBack, err := s.store.ArtifactChunk(r.Context(), task.UserID, chunk.UploadID, chunk.Index)
	if err != nil || readBack.SHA256 != chunk.SHA256 || readBack.ByteSize != chunk.ByteSize {
		fail(w, errors.Join(domain.ErrConflict, err))
		return
	}
	write(w, http.StatusOK, readBack)
}

func (s *Server) nodeTaskArtifactUploadStatus(w http.ResponseWriter, r *http.Request) {
	s.nodeTaskArtifactUploadStatusMode(w, r, false)
}

func (s *Server) nodeTaskRecoveryArtifactUploadStatus(w http.ResponseWriter, r *http.Request) {
	s.nodeTaskArtifactUploadStatusMode(w, r, true)
}

func (s *Server) nodeTaskArtifactUploadStatusMode(w http.ResponseWriter, r *http.Request, recovery bool) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	var task storage.ExecutionTask
	var nodeID string
	var err error
	if recovery {
		task, nodeID, err = s.validateNodeTaskRecoveryLease(r)
	} else {
		task, nodeID, err = s.validateNodeTaskLease(r)
	}
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.validateNodeArtifactUpload(r, task.UserID, nodeID, task.ID); err != nil {
		fail(w, err)
		return
	}
	offset, err := queryInt64(r, "chunkOffset", 0)
	if err != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	limit, err := queryInt64(r, "chunkLimit", 100)
	if err != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	upload, chunks, count, err := s.artifacts.Status(r.Context(), task.UserID, r.PathValue("uploadID"), offset, limit)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"upload": upload, "chunks": chunks, "receivedChunkCount": count, "chunkOffset": offset, "chunkLimit": limit})
}

func (s *Server) nodeTaskArtifactUploadFinalize(w http.ResponseWriter, r *http.Request) {
	s.nodeTaskArtifactUploadFinalizeMode(w, r, false)
}

func (s *Server) nodeTaskRecoveryArtifactUploadFinalize(w http.ResponseWriter, r *http.Request) {
	s.nodeTaskArtifactUploadFinalizeMode(w, r, true)
}

func (s *Server) nodeTaskArtifactUploadFinalizeMode(w http.ResponseWriter, r *http.Request, recovery bool) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	var task storage.ExecutionTask
	var nodeID string
	var err error
	if recovery {
		task, nodeID, err = s.validateNodeTaskRecoveryLease(r)
	} else {
		task, nodeID, err = s.validateNodeTaskLease(r)
	}
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.validateNodeArtifactUpload(r, task.UserID, nodeID, task.ID); err != nil {
		fail(w, err)
		return
	}
	var input struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &input) {
		return
	}
	if recovery {
		key, ok := recoveryEvidenceIdempotencyKey(strings.TrimSpace(input.Role))
		if !ok {
			fail(w, domain.ErrInvalid)
			return
		}
		upload, _, _, statusErr := s.artifacts.Status(r.Context(), task.UserID, r.PathValue("uploadID"), 0, 1)
		if statusErr != nil || upload.IdempotencyKey != nodeTaskArtifactKeyPrefix(nodeID, task.ID)+key {
			fail(w, errors.Join(domain.ErrUnauthorized, statusErr))
			return
		}
	}
	artifact, err := s.artifacts.Finalize(r.Context(), task.UserID, r.PathValue("uploadID"), time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	leaseHash := hashToken(strings.TrimSpace(r.Header.Get("X-O-Task-Lease")))
	if recovery {
		err = s.store.AttachExecutionTaskRecoveryEvidence(r.Context(), nodeID, task.ID, leaseHash, artifact.ID, strings.TrimSpace(input.Role), time.Now().UTC())
	} else {
		err = s.store.AttachExecutionTaskArtifactForLease(r.Context(), nodeID, task.ID, leaseHash, artifact.ID, input.Role, time.Now().UTC())
	}
	if err != nil {
		fail(w, err)
		return
	}
	readBack, file, err := s.artifacts.Open(r.Context(), task.UserID, artifact.ID)
	if err != nil {
		fail(w, err)
		return
	}
	closeErr := file.Close()
	if closeErr != nil {
		fail(w, closeErr)
		return
	}
	items, err := s.store.ExecutionTaskArtifacts(r.Context(), task.UserID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	for _, item := range items {
		if item.ID == readBack.ID && item.Role == strings.TrimSpace(input.Role) && item.SHA256 == artifact.SHA256 && item.ByteSize == artifact.ByteSize {
			write(w, http.StatusOK, item)
			return
		}
	}
	write(w, http.StatusConflict, map[string]string{"error": "task output artifact association did not read back"})
}

func recoveryEvidenceIdempotencyKey(role string) (string, bool) {
	switch role {
	case "recovery_transcript":
		return "recovery-transcript", true
	case "recovery_project_delta":
		return "recovery-project-delta", true
	case "recovery_project_lfs_objects":
		return "recovery-project-lfs-objects", true
	case "recovery_project_submodule_deltas":
		return "recovery-project-submodule-deltas", true
	case "recovery_checkpoint":
		return "recovery-checkpoint", true
	default:
		return "", false
	}
}

func (s *Server) validateNodeArtifactUpload(r *http.Request, userID, nodeID, taskID string) error {
	upload, _, _, err := s.artifacts.Status(r.Context(), userID, r.PathValue("uploadID"), 0, 1)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(upload.IdempotencyKey, nodeTaskArtifactKeyPrefix(nodeID, taskID)) {
		return domain.ErrUnauthorized
	}
	return nil
}

func validNodeArtifactClientKey(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func (s *Server) executionTaskArtifactsList(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ExecutionTaskArtifacts(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) executionTaskArtifactAttach(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	var input struct {
		ArtifactID string `json:"artifactId"`
		Role       string `json:"role"`
	}
	if !decode(w, r, &input) {
		return
	}
	artifact, file, err := s.artifacts.Open(r.Context(), s.workspaceID, input.ArtifactID)
	if err != nil {
		fail(w, err)
		return
	}
	if err := file.Close(); err != nil {
		fail(w, err)
		return
	}
	if err := s.store.AttachExecutionTaskArtifact(r.Context(), s.workspaceID, r.PathValue("id"), artifact.ID, input.Role, time.Now().UTC()); err != nil {
		fail(w, err)
		return
	}
	items, err := s.store.ExecutionTaskArtifacts(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	for _, item := range items {
		if item.ID == artifact.ID && item.Role == input.Role {
			write(w, http.StatusOK, item)
			return
		}
	}
	write(w, http.StatusConflict, map[string]string{"error": "artifact-task association did not read back"})
}

func (s *Server) artifactUploadBegin(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	var input struct {
		FileName       string `json:"fileName"`
		MediaType      string `json:"mediaType"`
		ExpectedSize   int64  `json:"expectedSize"`
		ExpectedSHA256 string `json:"expectedSha256"`
	}
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	upload, created, err := s.artifacts.Begin(r.Context(), s.workspaceID, artifactstore.BeginRequest{FileName: input.FileName, MediaType: input.MediaType, ExpectedSize: input.ExpectedSize, ExpectedSHA256: input.ExpectedSHA256, IdempotencyKey: r.Header.Get("Idempotency-Key")}, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, _, _, err := s.artifacts.Status(r.Context(), s.workspaceID, upload.ID, 0, 1)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.ID != upload.ID || readBack.Status != upload.Status || readBack.ExpectedSize != upload.ExpectedSize {
		write(w, http.StatusConflict, map[string]string{"error": "artifact upload did not read back from durable storage"})
		return
	}
	readBack, directCapability, direct, err := s.artifacts.CreateDirectUploadCapability(r.Context(), s.workspaceID, upload.ID, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.ID != upload.ID || readBack.Status != upload.Status || (direct && !readBack.DirectUpload) || (readBack.Status == "uploading" && readBack.DirectUpload && !direct) {
		write(w, http.StatusConflict, map[string]string{"error": "artifact direct-upload mode did not read back"})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	response := map[string]any{"upload": readBack, "created": created, "chunkSize": artifactstore.ChunkSize, "directUpload": direct}
	if direct {
		response["uploadURL"] = directCapability.URL
		response["requiredHeaders"] = directCapability.RequiredHeaders
		response["expiresAt"] = directCapability.ExpiresAt
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
	}
	write(w, status, response)
}

func (s *Server) artifactUploadStatus(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	offset, err := queryInt64(r, "chunkOffset", 0)
	if err != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	limit, err := queryInt64(r, "chunkLimit", 100)
	if err != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	upload, chunks, count, err := s.artifacts.Status(r.Context(), s.workspaceID, r.PathValue("id"), offset, limit)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"upload": upload, "chunks": chunks, "receivedChunkCount": count, "chunkOffset": offset, "chunkLimit": limit})
}

func (s *Server) artifactUploadChunk(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	index, err := strconv.ParseInt(r.PathValue("index"), 10, 64)
	if err != nil || index < 0 {
		fail(w, domain.ErrInvalid)
		return
	}
	digest := strings.TrimSpace(r.Header.Get("X-Chunk-SHA256"))
	if digest == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, artifactstore.ChunkSize+1)
	chunk, err := s.artifacts.Upload(r.Context(), s.workspaceID, r.PathValue("id"), index, digest, r.Body, time.Now().UTC())
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			write(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "chunk exceeds the 8 MiB transfer batch size"})
			return
		}
		fail(w, err)
		return
	}
	readBack, err := s.store.ArtifactChunk(r.Context(), s.workspaceID, chunk.UploadID, chunk.Index)
	if err != nil {
		fail(w, err)
		return
	}
	if readBack.SHA256 != chunk.SHA256 || readBack.ByteSize != chunk.ByteSize {
		write(w, http.StatusConflict, map[string]string{"error": "artifact chunk metadata did not read back"})
		return
	}
	write(w, http.StatusOK, readBack)
}

func (s *Server) artifactUploadFinalize(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	artifact, err := s.artifacts.Finalize(r.Context(), s.workspaceID, r.PathValue("id"), time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	readBack, file, err := s.artifacts.Open(r.Context(), s.workspaceID, artifact.ID)
	if err != nil {
		fail(w, err)
		return
	}
	closeErr := file.Close()
	if closeErr != nil {
		fail(w, closeErr)
		return
	}
	if readBack.ID != artifact.ID || readBack.SHA256 != artifact.SHA256 || readBack.ByteSize != artifact.ByteSize {
		write(w, http.StatusConflict, map[string]string{"error": "final artifact failed authoritative read-back"})
		return
	}
	write(w, http.StatusOK, readBack)
}

func (s *Server) artifactDownload(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "artifact storage is unavailable"})
		return
	}
	// For remote object storage, redirect the authenticated browser to a
	// five-minute, single-object GET capability. This keeps artifact bytes out
	// of the control server's bandwidth path; local storage still uses the
	// verified proxy below.
	artifactID := r.PathValue("id")
	metadata, err := s.store.Artifact(r.Context(), s.workspaceID, artifactID)
	if err != nil {
		fail(w, err)
		return
	}
	remoteArtifact, ticket, direct, err := s.artifacts.CreatePresignedDownloadURLWithDisposition(
		r.Context(), s.workspaceID, artifactID,
		fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(metadata.FileName)), 5*time.Minute,
	)
	if err != nil {
		fail(w, err)
		return
	}
	if direct {
		if metadata.ID != remoteArtifact.ID || metadata.SHA256 != remoteArtifact.SHA256 || metadata.ByteSize != remoteArtifact.ByteSize || metadata.FileName == "" || ticket.URL == "" {
			fail(w, errors.Join(domain.ErrConflict, err))
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, ticket.URL, http.StatusTemporaryRedirect)
		return
	}
	artifact, file, err := s.artifacts.Open(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			slog.Error("artifact download cleanup failed", "artifact_id", artifact.ID, "error", closeErr)
		}
	}()
	setArtifactDownloadHeaders(w, artifact.FileName, artifact.MediaType, artifact.SHA256, artifact.ByteSize)
	http.ServeContent(w, r, artifact.FileName, artifact.CreatedAt, file)
}

func setArtifactDownloadHeaders(w http.ResponseWriter, fileName, mediaType, sha256 string, byteSize int64) {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(fileName)))
	w.Header().Set("X-Artifact-SHA256", sha256)
	w.Header().Set("X-Artifact-Byte-Size", strconv.FormatInt(byteSize, 10))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func queryInt64(r *http.Request, key string, fallback int64) (int64, error) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback, nil
	}
	return strconv.ParseInt(value, 10, 64)
}
