package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

const nodeArtifactChunkSize int64 = 8 << 20

const nodeArtifactSyncRetryWindow = 2 * time.Minute

type localTranscriptEnvelope struct {
	SourceConversationID       string          `json:"sourceConversationId"`
	ProjectID                  string          `json:"projectId,omitempty"`
	ProjectCommit              string          `json:"projectCommit,omitempty"`
	ConversationID             string          `json:"conversationId"`
	AgentTurnID                string          `json:"agentTurnId"`
	ResultMessageID            string          `json:"resultMessageId"`
	AgentStatus                string          `json:"agentStatus"`
	AgentStopReason            string          `json:"agentStopReason,omitempty"`
	AssistantText              string          `json:"assistantText"`
	AssistantTextTruncated     bool            `json:"assistantTextTruncated,omitempty"`
	Transcript                 json.RawMessage `json:"transcript"`
	ProjectDeltaPath           string          `json:"projectDeltaPath,omitempty"`
	ProjectLFSPath             string          `json:"projectLfsPath,omitempty"`
	ProjectSubmoduleDeltasPath string          `json:"projectSubmoduleDeltasPath,omitempty"`
	ProjectDeltaBaseCommit     string          `json:"projectDeltaBaseCommit,omitempty"`
	ProjectDeltaCommit         string          `json:"projectDeltaCommit,omitempty"`
	HandoffCheckpoint          json.RawMessage `json:"handoffCheckpoint,omitempty"`
}

type nodeArtifactUploadResponse struct {
	Upload         storage.ArtifactUpload `json:"upload"`
	ChunkSize      int64                  `json:"chunkSize"`
	DirectUpload   bool                   `json:"directUpload"`
	UploadURL      string                 `json:"uploadURL,omitempty"`
	RequiredHeader map[string]string      `json:"requiredHeaders,omitempty"`
	ExpiresAt      time.Time              `json:"expiresAt,omitempty"`
}

type nodeArtifactStatusResponse struct {
	Upload      storage.ArtifactUpload  `json:"upload"`
	Chunks      []storage.ArtifactChunk `json:"chunks"`
	ChunkOffset int64                   `json:"chunkOffset"`
	ChunkLimit  int64                   `json:"chunkLimit"`
}

// syncTranscriptArtifactWithRetry retries only transport failures and server
// responses that conventionally indicate temporary unavailability. Artifact
// upload IDs and chunk hashes make each replay idempotent; the worker keeps
// pulsing the task lease while this function waits or retries.
func (c *nodeAPIClient) syncTranscriptArtifactWithRetry(ctx context.Context, taskID, lease string, result json.RawMessage) (json.RawMessage, error) {
	return c.syncTranscriptArtifactWithRetryMode(ctx, taskID, lease, result, false, false)
}

func (c *nodeAPIClient) syncTranscriptArtifactWithRetryPreservingLocalFiles(ctx context.Context, taskID, lease string, result json.RawMessage) (json.RawMessage, error) {
	return c.syncTranscriptArtifactWithRetryMode(ctx, taskID, lease, result, true, false)
}

func (c *nodeAPIClient) syncTranscriptArtifactForRecovery(ctx context.Context, taskID, lease string, result json.RawMessage) (json.RawMessage, error) {
	manifest, err := c.syncTranscriptArtifactWithRetryMode(ctx, taskID, lease, result, true, true)
	if err != nil {
		return nil, err
	}
	var recoveryArtifacts struct {
		HandoffCheckpointArtifact *storage.ExecutionTaskArtifact `json:"handoffCheckpointArtifact"`
	}
	if err := json.Unmarshal(manifest, &recoveryArtifacts); err != nil {
		return nil, fmt.Errorf("decode recovered checkpoint reference: %w", err)
	}
	if recoveryArtifacts.HandoffCheckpointArtifact != nil {
		var checkpointReadBack storage.ExecutionTaskHandoffCheckpoint
		if err := c.callJSON(ctx, http.MethodPost, "/api/v1/nodes/tasks/"+url.PathEscape(taskID)+"/recovery-evidence/checkpoint", lease, map[string]string{"artifactId": recoveryArtifacts.HandoffCheckpointArtifact.ID}, &checkpointReadBack); err != nil {
			return nil, fmt.Errorf("rebind recovered checkpoint to cloud vault: %w", err)
		}
		if checkpointReadBack.TaskID != taskID || len(checkpointReadBack.ContentSHA256) != 64 || len(checkpointReadBack.Ciphertext) != 0 || len(checkpointReadBack.Nonce) != 0 {
			return nil, errors.New("cloud recovery checkpoint record did not read back with its verified manifest")
		}
	}
	var readBack storage.ExecutionTask
	if err := c.callJSON(ctx, http.MethodPost, "/api/v1/nodes/tasks/"+url.PathEscape(taskID)+"/recovery-evidence/result", lease, json.RawMessage(manifest), &readBack); err != nil {
		return nil, fmt.Errorf("persist verified recovery result manifest: %w", err)
	}
	if readBack.ID != taskID || readBack.Status != "needs_reconciliation" || !bytes.Equal(readBack.RecoveryResult, manifest) {
		return nil, errors.New("cloud recovery manifest did not read back in the unreconciled task")
	}
	return manifest, nil
}

func (c *nodeAPIClient) syncTranscriptArtifactWithRetryMode(ctx context.Context, taskID, lease string, result json.RawMessage, preserveLocalFiles, recovery bool) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	delay := 250 * time.Millisecond
	started := time.Now()
	for {
		readBack, err := c.syncTranscriptArtifactMode(ctx, taskID, lease, result, preserveLocalFiles, recovery)
		if err == nil {
			return readBack, nil
		}
		if ctx.Err() != nil {
			return nil, nodeArtifactSyncOutcomeError(errors.Join(err, ctx.Err()))
		}
		if !retryableNodeArtifactSyncError(err) {
			return nil, nodeArtifactSyncOutcomeError(err)
		}
		if time.Since(started) >= nodeArtifactSyncRetryWindow {
			return nil, nodeArtifactSyncOutcomeError(fmt.Errorf("artifact sync retry window expired after %s: %w", nodeArtifactSyncRetryWindow, err))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, nodeArtifactSyncOutcomeError(errors.Join(err, ctx.Err()))
		case <-timer.C:
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
}

func nodeArtifactSyncOutcomeError(err error) error {
	return &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: fmt.Errorf("local execution finished but cloud artifacts were not verified: %w", err)}
}

func (c *nodeAPIClient) persistHandoffCheckpointWithRetry(ctx context.Context, taskID, lease, artifactID string) error {
	delay := 250 * time.Millisecond
	started := time.Now()
	for {
		var readBack storage.ExecutionTaskHandoffCheckpoint
		err := c.callJSON(ctx, http.MethodPost, "/api/v1/nodes/tasks/"+url.PathEscape(taskID)+"/handoff-checkpoint", lease, map[string]string{"artifactId": artifactID}, &readBack)
		if err == nil {
			if readBack.TaskID != taskID || readBack.ArtifactID != artifactID || readBack.Version != 1 || readBack.ContentSHA256 == "" {
				return nodeArtifactSyncOutcomeError(errors.New("cloud handoff checkpoint did not read back its task and artifact binding"))
			}
			return nil
		}
		if ctx.Err() != nil {
			return nodeArtifactSyncOutcomeError(errors.Join(err, ctx.Err()))
		}
		if !retryableNodeArtifactSyncError(err) {
			return nodeArtifactSyncOutcomeError(err)
		}
		if time.Since(started) >= nodeArtifactSyncRetryWindow {
			return nodeArtifactSyncOutcomeError(fmt.Errorf("handoff checkpoint transfer retry window expired after %s: %w", nodeArtifactSyncRetryWindow, err))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nodeArtifactSyncOutcomeError(errors.Join(err, ctx.Err()))
		case <-timer.C:
		}
		if delay < 5*time.Second {
			delay *= 2
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
		}
	}
}

// syncTranscriptArtifact persists a full local conversation snapshot using the
// existing resumable artifact protocol. The task is only reported successful
// after the cloud has read back the task-bound manifest and content hash.
func (c *nodeAPIClient) syncTranscriptArtifact(ctx context.Context, taskID, lease string, result json.RawMessage) (json.RawMessage, error) {
	return c.syncTranscriptArtifactMode(ctx, taskID, lease, result, false, false)
}

func (c *nodeAPIClient) syncTranscriptArtifactMode(ctx context.Context, taskID, lease string, result json.RawMessage, preserveLocalFiles, recovery bool) (json.RawMessage, error) {
	var transcript localTranscriptEnvelope
	if err := json.Unmarshal(result, &transcript); err != nil {
		return nil, err
	}
	if len(transcript.Transcript) == 0 || bytes.Equal(bytes.TrimSpace(transcript.Transcript), []byte("null")) {
		if recovery {
			return nil, errors.New("local outbox has no transcript to upload as recovery evidence")
		}
		return result, nil
	}
	if !json.Valid(transcript.Transcript) || strings.TrimSpace(transcript.ConversationID) == "" {
		return nil, errors.New("local conversation transcript is not valid JSON or has no identity")
	}
	digest := sha256.Sum256(transcript.Transcript)
	sha := hex.EncodeToString(digest[:])
	transcriptKey, transcriptRole := "local-transcript", "conversation_transcript"
	if recovery {
		transcriptKey, transcriptRole = "recovery-transcript", "recovery_transcript"
	}
	artifact, err := c.uploadTaskArtifact(ctx, taskID, lease, transcriptKey, "conversation-"+safeNodeFilePart(transcript.ConversationID)+".json", "application/json", transcriptRole, int64(len(transcript.Transcript)), sha, bytes.NewReader(transcript.Transcript), recovery)
	if err != nil {
		return nil, err
	}
	var projectDeltaArtifact *storage.ExecutionTaskArtifact
	var projectLFSArtifact *storage.ExecutionTaskArtifact
	var projectSubmoduleDeltasArtifact *storage.ExecutionTaskArtifact
	if transcript.ProjectDeltaPath != "" {
		if transcript.ProjectID == "" || !validCommitSHA(transcript.ProjectDeltaBaseCommit) || !validCommitSHA(transcript.ProjectDeltaCommit) {
			return nil, errors.New("project delta is missing its project identity or valid base/result commit")
		}
		deltaPath, err := filepath.Abs(filepath.Clean(transcript.ProjectDeltaPath))
		if err != nil {
			return nil, fmt.Errorf("resolve local project delta path: %w", err)
		}
		deltaFile, err := os.Open(deltaPath)
		if err != nil {
			return nil, fmt.Errorf("open local project delta bundle: %w", err)
		}
		fileInfo, statErr := deltaFile.Stat()
		if statErr != nil || !fileInfo.Mode().IsRegular() || fileInfo.Size() <= 0 {
			_ = deltaFile.Close()
			return nil, errors.Join(errors.New("local project delta is not a non-empty regular file"), statErr)
		}
		hasher := sha256.New()
		if _, err := io.Copy(hasher, deltaFile); err != nil {
			_ = deltaFile.Close()
			return nil, fmt.Errorf("hash local project delta bundle: %w", err)
		}
		if _, err := deltaFile.Seek(0, io.SeekStart); err != nil {
			_ = deltaFile.Close()
			return nil, fmt.Errorf("rewind local project delta bundle: %w", err)
		}
		sha := hex.EncodeToString(hasher.Sum(nil))
		deltaKey, deltaRole := "project-delta", "project_delta"
		if recovery {
			deltaKey, deltaRole = "recovery-project-delta", "recovery_project_delta"
		}
		bundle, uploadErr := c.uploadTaskArtifact(ctx, taskID, lease, deltaKey, "project-delta-"+safeNodeFilePart(transcript.ProjectID)+".bundle", "application/x-git-bundle", deltaRole, fileInfo.Size(), sha, deltaFile, recovery)
		closeErr := deltaFile.Close()
		if !preserveLocalFiles {
			removeErr := os.Remove(deltaPath)
			if uploadErr != nil || closeErr != nil {
				return nil, errors.Join(wrapArtifactUploadError(uploadErr), closeErr)
			}
			if removeErr != nil {
				return nil, fmt.Errorf("remove cloud-verified local project delta bundle: %w", removeErr)
			}
		} else if uploadErr != nil || closeErr != nil {
			return nil, errors.Join(wrapArtifactUploadError(uploadErr), closeErr)
		}
		projectDeltaArtifact = &bundle
	} else if transcript.ProjectDeltaBaseCommit != "" || transcript.ProjectDeltaCommit != "" {
		return nil, errors.New("project delta commit metadata was supplied without a bundle")
	}
	if transcript.ProjectLFSPath != "" {
		if projectDeltaArtifact == nil {
			return nil, errors.New("Git LFS objects were supplied without their project delta")
		}
		lfsPath, err := filepath.Abs(filepath.Clean(transcript.ProjectLFSPath))
		if err != nil {
			return nil, fmt.Errorf("resolve local project Git LFS archive path: %w", err)
		}
		lfsFile, err := os.Open(lfsPath)
		if err != nil {
			return nil, fmt.Errorf("open local project Git LFS archive: %w", err)
		}
		fileInfo, statErr := lfsFile.Stat()
		if statErr != nil || !fileInfo.Mode().IsRegular() || fileInfo.Size() <= 0 {
			_ = lfsFile.Close()
			return nil, errors.Join(errors.New("local project Git LFS archive is not a non-empty regular file"), statErr)
		}
		hasher := sha256.New()
		if _, err := io.Copy(hasher, lfsFile); err != nil {
			_ = lfsFile.Close()
			return nil, fmt.Errorf("hash local project Git LFS archive: %w", err)
		}
		if _, err := lfsFile.Seek(0, io.SeekStart); err != nil {
			_ = lfsFile.Close()
			return nil, fmt.Errorf("rewind local project Git LFS archive: %w", err)
		}
		bundle, uploadErr := c.uploadTaskArtifact(ctx, taskID, lease, "project-lfs-objects", "project-lfs-objects.tar", "application/x-tar", "project_lfs_objects", fileInfo.Size(), hex.EncodeToString(hasher.Sum(nil)), lfsFile, recovery)
		closeErr := lfsFile.Close()
		if !preserveLocalFiles && uploadErr == nil && closeErr == nil {
			if err := os.Remove(lfsPath); err != nil {
				return nil, fmt.Errorf("remove cloud-verified local Git LFS archive: %w", err)
			}
			if _, err := os.Lstat(lfsPath); !errors.Is(err, os.ErrNotExist) {
				if err == nil {
					err = errors.New("local Git LFS archive remains after cleanup")
				}
				return nil, fmt.Errorf("verify local Git LFS archive cleanup: %w", err)
			}
		}
		if uploadErr != nil || closeErr != nil {
			return nil, errors.Join(fmt.Errorf("upload local project Git LFS archive: %w", uploadErr), closeErr)
		}
		projectLFSArtifact = &bundle
	}
	if transcript.ProjectSubmoduleDeltasPath != "" {
		if projectDeltaArtifact == nil {
			return nil, errors.New("submodule changes were supplied without their project delta")
		}
		submodulePath, err := filepath.Abs(filepath.Clean(transcript.ProjectSubmoduleDeltasPath))
		if err != nil {
			return nil, fmt.Errorf("resolve local project submodule delta archive path: %w", err)
		}
		submoduleFile, err := os.Open(submodulePath)
		if err != nil {
			return nil, fmt.Errorf("open local project submodule delta archive: %w", err)
		}
		fileInfo, statErr := submoduleFile.Stat()
		if statErr != nil || !fileInfo.Mode().IsRegular() || fileInfo.Size() <= 0 {
			_ = submoduleFile.Close()
			return nil, errors.Join(errors.New("local project submodule delta archive is not a non-empty regular file"), statErr)
		}
		hasher := sha256.New()
		if _, err := io.Copy(hasher, submoduleFile); err != nil {
			_ = submoduleFile.Close()
			return nil, fmt.Errorf("hash local project submodule delta archive: %w", err)
		}
		if _, err := submoduleFile.Seek(0, io.SeekStart); err != nil {
			_ = submoduleFile.Close()
			return nil, fmt.Errorf("rewind local project submodule delta archive: %w", err)
		}
		role, key := "project_submodule_deltas", "project-submodule-deltas"
		if recovery {
			role, key = "recovery_project_submodule_deltas", "recovery-project-submodule-deltas"
		}
		bundle, uploadErr := c.uploadTaskArtifact(ctx, taskID, lease, key, "project-submodule-deltas.tar", "application/x-tar", role, fileInfo.Size(), hex.EncodeToString(hasher.Sum(nil)), submoduleFile, recovery)
		closeErr := submoduleFile.Close()
		if !preserveLocalFiles && uploadErr == nil && closeErr == nil {
			if err := os.Remove(submodulePath); err != nil {
				return nil, fmt.Errorf("remove cloud-verified local submodule delta archive: %w", err)
			}
			if _, err := os.Lstat(submodulePath); !errors.Is(err, os.ErrNotExist) {
				if err == nil {
					err = errors.New("local submodule delta archive remains after cleanup")
				}
				return nil, fmt.Errorf("verify local submodule delta archive cleanup: %w", err)
			}
		}
		if uploadErr != nil || closeErr != nil {
			return nil, errors.Join(fmt.Errorf("upload local project submodule delta archive: %w", uploadErr), closeErr)
		}
		projectSubmoduleDeltasArtifact = &bundle
	}
	var handoffCheckpointArtifact *storage.ExecutionTaskArtifact
	if len(transcript.HandoffCheckpoint) > 0 {
		if len(transcript.HandoffCheckpoint) > nodeHandoffCheckpointMaxBytes+(1<<20) || !json.Valid(transcript.HandoffCheckpoint) {
			return nil, fmt.Errorf("encrypted continuation checkpoint artifact is invalid or too large: %w", domain.ErrInvalid)
		}
		digest := sha256.Sum256(transcript.HandoffCheckpoint)
		sha := hex.EncodeToString(digest[:])
		checkpointKey, checkpointRole := "continuation-checkpoint", "continuation_checkpoint"
		if recovery {
			checkpointKey, checkpointRole = "recovery-checkpoint", "recovery_checkpoint"
		}
		checkpointArtifact, err := c.uploadTaskArtifact(ctx, taskID, lease, checkpointKey, "continuation-checkpoint.json", "application/vnd.o-agent.continuation-checkpoint+json", checkpointRole, int64(len(transcript.HandoffCheckpoint)), sha, bytes.NewReader(transcript.HandoffCheckpoint), recovery)
		if err != nil {
			return nil, fmt.Errorf("upload encrypted continuation checkpoint: %w", err)
		}
		if !recovery {
			if err := c.persistHandoffCheckpointWithRetry(ctx, taskID, lease, checkpointArtifact.ID); err != nil {
				return nil, err
			}
		}
		handoffCheckpointArtifact = &checkpointArtifact
	}
	transcript.Transcript = nil
	preview, previewTruncated := truncateUTF8(transcript.AssistantText, 16<<10)
	return json.Marshal(struct {
		SourceConversationID           string                         `json:"sourceConversationId"`
		ProjectID                      string                         `json:"projectId,omitempty"`
		ProjectCommit                  string                         `json:"projectCommit,omitempty"`
		ConversationID                 string                         `json:"conversationId"`
		AgentTurnID                    string                         `json:"agentTurnId"`
		ResultMessageID                string                         `json:"resultMessageId"`
		AgentStatus                    string                         `json:"agentStatus"`
		AgentStopReason                string                         `json:"agentStopReason,omitempty"`
		AssistantText                  string                         `json:"assistantText"`
		AssistantTextTruncated         bool                           `json:"assistantTextTruncated,omitempty"`
		TranscriptArtifact             storage.ExecutionTaskArtifact  `json:"transcriptArtifact"`
		ProjectDeltaBaseCommit         string                         `json:"projectDeltaBaseCommit,omitempty"`
		ProjectDeltaCommit             string                         `json:"projectDeltaCommit,omitempty"`
		ProjectDeltaArtifact           *storage.ExecutionTaskArtifact `json:"projectDeltaArtifact,omitempty"`
		ProjectLFSArtifact             *storage.ExecutionTaskArtifact `json:"projectLfsArtifact,omitempty"`
		ProjectSubmoduleDeltasArtifact *storage.ExecutionTaskArtifact `json:"projectSubmoduleDeltasArtifact,omitempty"`
		HandoffCheckpointArtifact      *storage.ExecutionTaskArtifact `json:"handoffCheckpointArtifact,omitempty"`
	}{SourceConversationID: transcript.SourceConversationID, ProjectID: transcript.ProjectID, ProjectCommit: transcript.ProjectCommit, ConversationID: transcript.ConversationID, AgentTurnID: transcript.AgentTurnID, ResultMessageID: transcript.ResultMessageID, AgentStatus: transcript.AgentStatus, AgentStopReason: transcript.AgentStopReason, AssistantText: preview, AssistantTextTruncated: previewTruncated, TranscriptArtifact: artifact, ProjectDeltaBaseCommit: transcript.ProjectDeltaBaseCommit, ProjectDeltaCommit: transcript.ProjectDeltaCommit, ProjectDeltaArtifact: projectDeltaArtifact, ProjectLFSArtifact: projectLFSArtifact, ProjectSubmoduleDeltasArtifact: projectSubmoduleDeltasArtifact, HandoffCheckpointArtifact: handoffCheckpointArtifact})
}

func validCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func wrapArtifactUploadError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("upload local project delta bundle: %w", err)
}

// uploadTaskArtifact sends any seekable local result through the lease-bound,
// resumable artifact protocol. The caller supplies the role and stable
// idempotency key so project bundles can share the transcript transport.
func (c *nodeAPIClient) uploadTaskArtifact(ctx context.Context, taskID, lease, idempotencyKey, fileName, mediaType, role string, size int64, sha string, source io.ReaderAt, recovery bool) (storage.ExecutionTaskArtifact, error) {
	if size <= 0 || source == nil || len(sha) != 64 || idempotencyKey == "" || role == "" {
		return storage.ExecutionTaskArtifact{}, errors.New("task artifact source metadata is invalid")
	}
	if _, err := hex.DecodeString(sha); err != nil {
		return storage.ExecutionTaskArtifact{}, errors.New("task artifact source hash is not valid hexadecimal")
	}
	uploadBase := "/api/v1/nodes/tasks/" + url.PathEscape(taskID) + "/artifacts/uploads"
	if recovery {
		uploadBase = "/api/v1/nodes/tasks/" + url.PathEscape(taskID) + "/recovery-evidence/artifacts/uploads"
	}
	beginBodyValue := map[string]any{
		"fileName":       fileName,
		"mediaType":      mediaType,
		"expectedSize":   size,
		"expectedSha256": sha,
	}
	if recovery {
		beginBodyValue["role"] = role
	}
	beginBody, err := json.Marshal(beginBodyValue)
	if err != nil {
		return storage.ExecutionTaskArtifact{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+uploadBase, bytes.NewReader(beginBody))
	if err != nil {
		return storage.ExecutionTaskArtifact{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)
	request.Header.Set("X-O-Task-Lease", lease)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return storage.ExecutionTaskArtifact{}, err
	}
	var begin nodeArtifactUploadResponse
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err = nodeHTTPError(response)
		_ = response.Body.Close()
		return storage.ExecutionTaskArtifact{}, err
	}
	err = decodeNodeResponse(response, &begin)
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil {
		return storage.ExecutionTaskArtifact{}, errors.Join(err, closeErr)
	}
	if begin.Upload.ID == "" || begin.Upload.ExpectedSHA256 != sha || begin.Upload.ExpectedSize != size || (begin.Upload.Status != "uploading" && begin.Upload.Status != "complete") || begin.ChunkSize <= 0 || begin.ChunkSize > nodeArtifactChunkSize {
		return storage.ExecutionTaskArtifact{}, errors.New("cloud task artifact upload did not read back the requested durable state")
	}
	if begin.Upload.Status != "complete" {
		if begin.DirectUpload {
			if !begin.Upload.DirectUpload || begin.UploadURL == "" || len(begin.RequiredHeader) == 0 {
				return storage.ExecutionTaskArtifact{}, errors.New("cloud direct-upload mode did not return its durable checksum-bound capability")
			}
			if err := c.uploadDirectArtifact(ctx, begin.UploadURL, begin.RequiredHeader, size, source); err != nil {
				return storage.ExecutionTaskArtifact{}, fmt.Errorf("upload artifact directly to object storage: %w", err)
			}
		} else {
			if begin.Upload.DirectUpload {
				return storage.ExecutionTaskArtifact{}, errors.New("cloud upload checkpoint selected direct mode without returning a direct capability")
			}
			if err := c.uploadMissingArtifactChunks(ctx, taskID, lease, uploadBase, begin.Upload, begin.ChunkSize, size, source); err != nil {
				return storage.ExecutionTaskArtifact{}, err
			}
		}
	}
	finalizePath := uploadBase + "/" + url.PathEscape(begin.Upload.ID) + "/finalize"
	finalizeBody, err := json.Marshal(map[string]string{"role": role})
	if err != nil {
		return storage.ExecutionTaskArtifact{}, err
	}
	finalizeRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+finalizePath, bytes.NewReader(finalizeBody))
	if err != nil {
		return storage.ExecutionTaskArtifact{}, err
	}
	finalizeRequest.Header.Set("Authorization", "Bearer "+c.credential)
	finalizeRequest.Header.Set("X-O-Task-Lease", lease)
	finalizeRequest.Header.Set("Content-Type", "application/json")
	finalizedResponse, err := c.http.Do(finalizeRequest)
	if err != nil {
		return storage.ExecutionTaskArtifact{}, err
	}
	var artifact storage.ExecutionTaskArtifact
	if finalizedResponse.StatusCode < 200 || finalizedResponse.StatusCode >= 300 {
		err = nodeHTTPError(finalizedResponse)
		_ = finalizedResponse.Body.Close()
		return storage.ExecutionTaskArtifact{}, err
	}
	err = decodeNodeResponse(finalizedResponse, &artifact)
	closeErr = finalizedResponse.Body.Close()
	if err != nil || closeErr != nil {
		return storage.ExecutionTaskArtifact{}, errors.Join(err, closeErr)
	}
	if artifact.ID == "" || artifact.Role != role || artifact.SHA256 != sha || artifact.ByteSize != size {
		return storage.ExecutionTaskArtifact{}, errors.New("cloud task artifact manifest did not read back with matching task association and content hash")
	}
	return artifact, nil
}

func (c *nodeAPIClient) uploadDirectArtifact(ctx context.Context, target string, headers map[string]string, size int64, source io.ReaderAt) error {
	parsed, err := url.Parse(strings.TrimSpace(target))
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && directUploadLoopbackHost(parsed.Hostname()))) {
		return errors.New("cloud direct-upload URL is not a secure absolute object URL")
	}
	checksum, ok := headers["x-amz-checksum-sha256"]
	decodedChecksum, decodeErr := base64.StdEncoding.DecodeString(checksum)
	if !ok || decodeErr != nil || len(decodedChecksum) != sha256.Size || len(headers) != 1 {
		return errors.New("cloud direct-upload capability did not return exactly one valid SHA-256 header")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, parsed.String(), io.NewSectionReader(source, 0, size))
	if err != nil {
		return err
	}
	request.ContentLength = size
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	client := http.Client{}
	if c.http != nil {
		client = *c.http
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return sanitizePresignedObjectTransportError(err, parsed, "upload")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
		closeErr := response.Body.Close()
		providerErr := fmt.Errorf("object provider rejected direct upload with HTTP %d", response.StatusCode)
		if readErr != nil {
			providerErr = errors.Join(providerErr, fmt.Errorf("discard provider error response failed (%T)", readErr))
		}
		if closeErr != nil {
			providerErr = errors.Join(providerErr, fmt.Errorf("close provider error response failed (%T)", closeErr))
		}
		return providerErr
	}
	closeErr := response.Body.Close()
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func sanitizePresignedObjectTransportError(err error, signedURL *url.URL, operation string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if err == nil || signedURL == nil {
		return errors.New("direct artifact upload transport failed")
	}
	safeTarget := signedURL.Scheme + "://" + signedURL.Host + signedURL.EscapedPath()
	message := err.Error()
	message = strings.ReplaceAll(message, signedURL.String(), safeTarget)
	if signedURL.RawQuery != "" {
		message = strings.ReplaceAll(message, "?"+signedURL.RawQuery, "?[redacted]")
		message = strings.ReplaceAll(message, signedURL.RawQuery, "[redacted]")
	}
	return fmt.Errorf("direct artifact %s to %s failed: %s", operation, safeTarget, message)
}

func presignedObjectHTTPError(response *http.Response, operation string) error {
	if response == nil {
		return errors.New("presigned object request returned no HTTP response")
	}
	return fmt.Errorf("object provider rejected direct artifact %s with HTTP %d", operation, response.StatusCode)
}

func directUploadLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func truncateUTF8(value string, maxBytes int) (string, bool) {
	if maxBytes < 0 || len(value) <= maxBytes {
		return value, false
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func (c *nodeAPIClient) uploadMissingTranscriptChunks(ctx context.Context, taskID, lease string, upload storage.ArtifactUpload, chunkSize int64, transcript []byte) error {
	base := "/api/v1/nodes/tasks/" + url.PathEscape(taskID) + "/artifacts/uploads"
	return c.uploadMissingArtifactChunks(ctx, taskID, lease, base, upload, chunkSize, int64(len(transcript)), bytes.NewReader(transcript))
}

func (c *nodeAPIClient) uploadMissingArtifactChunks(ctx context.Context, taskID, lease, uploadBase string, upload storage.ArtifactUpload, chunkSize, size int64, source io.ReaderAt) error {
	present := make(map[int64]string)
	for offset := int64(0); offset < upload.ChunkCount; {
		path := uploadBase + "/" + url.PathEscape(upload.ID) + "?chunkOffset=" + strconv.FormatInt(offset, 10) + "&chunkLimit=1000"
		var page nodeArtifactStatusResponse
		if err := c.callJSON(ctx, http.MethodGet, path, lease, nil, &page); err != nil {
			return fmt.Errorf("read transcript upload checkpoint: %w", err)
		}
		if page.Upload.ID != upload.ID || page.ChunkOffset != offset || page.ChunkLimit != 1000 {
			return errors.New("cloud transcript checkpoint did not read back for the requested upload")
		}
		for _, chunk := range page.Chunks {
			if chunk.Index < offset || chunk.Index >= upload.ChunkCount || chunk.ByteSize != expectedNodeArtifactChunkSize(upload, chunk.Index, chunkSize) || len(chunk.SHA256) != 64 {
				return errors.New("cloud transcript upload returned an invalid chunk checkpoint")
			}
			if _, err := hex.DecodeString(chunk.SHA256); err != nil {
				return errors.New("cloud transcript upload returned a malformed chunk digest")
			}
			present[chunk.Index] = chunk.SHA256
		}
		if len(page.Chunks) == 0 {
			break
		}
		offset = page.Chunks[len(page.Chunks)-1].Index + 1
	}
	for index := int64(0); index < upload.ChunkCount; index++ {
		start := index * chunkSize
		end := start + chunkSize
		if end > size {
			end = size
		}
		chunk := make([]byte, end-start)
		read, err := source.ReadAt(chunk, start)
		if read != len(chunk) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("read local task artifact chunk %d: %w", index, err)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read local task artifact chunk %d: %w", index, err)
		}
		digest := sha256.Sum256(chunk)
		sha := hex.EncodeToString(digest[:])
		if existing, ok := present[index]; ok {
			if existing != sha {
				return fmt.Errorf("cloud transcript chunk %d has a different content hash", index)
			}
			continue
		}
		path := uploadBase + "/" + url.PathEscape(upload.ID) + "/chunks/" + strconv.FormatInt(index, 10)
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, bytes.NewReader(chunk))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+c.credential)
		request.Header.Set("X-O-Task-Lease", lease)
		request.Header.Set("X-Chunk-SHA256", sha)
		response, err := c.http.Do(request)
		if err != nil {
			return err
		}
		var readBack storage.ArtifactChunk
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			err = nodeHTTPError(response)
			_ = response.Body.Close()
			return err
		}
		err = decodeNodeResponse(response, &readBack)
		closeErr := response.Body.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
		if readBack.UploadID != upload.ID || readBack.Index != index || readBack.SHA256 != sha || readBack.ByteSize != int64(len(chunk)) {
			return fmt.Errorf("cloud transcript chunk %d did not read back with matching content", index)
		}
	}
	return nil
}

func expectedNodeArtifactChunkSize(upload storage.ArtifactUpload, index, chunkSize int64) int64 {
	if index < 0 || index >= upload.ChunkCount {
		return -1
	}
	if index == upload.ChunkCount-1 {
		remaining := upload.ExpectedSize - index*chunkSize
		if remaining <= 0 || remaining > chunkSize {
			return -1
		}
		return remaining
	}
	return chunkSize
}

func safeNodeFilePart(value string) string {
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			result.WriteRune(character)
		}
	}
	if result.Len() == 0 {
		return "local"
	}
	if result.Len() > 64 {
		return result.String()[:64]
	}
	return result.String()
}
