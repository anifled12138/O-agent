package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"axiom.local/agent/internal/domain"
)

type taskTranscriptEnvelope struct {
	SourceConversationID string `json:"sourceConversationId"`
	ConversationID       string `json:"conversationId"`
	AgentTurnID          string `json:"agentTurnId"`
	ResultMessageID      string `json:"resultMessageId"`
	AgentStatus          string `json:"agentStatus"`
	AgentStopReason      string `json:"agentStopReason"`
	Transcript           struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
		Trace []domain.TraceEvent `json:"trace"`
	} `json:"transcript"`
}

func (s *Server) executionTaskHandoffPreview(w http.ResponseWriter, r *http.Request) {
	if s.artifacts == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "Task artifact storage is unavailable"})
		return
	}
	task, err := s.store.ExecutionTask(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	recoveryContinuation := task.Status == "needs_reconciliation" && len(task.RecoveryResult) > 0
	if task.Status != "reported_succeeded" && !recoveryContinuation {
		write(w, http.StatusConflict, map[string]string{"error": "only a durably reported local task can be previewed for cloud continuation"})
		return
	}
	resultJSON := task.Result
	if recoveryContinuation {
		resultJSON = task.RecoveryResult
	}
	var result struct {
		SourceConversationID string `json:"sourceConversationId"`
		ConversationID       string `json:"conversationId"`
		AgentTurnID          string `json:"agentTurnId"`
		ResultMessageID      string `json:"resultMessageId"`
		AgentStatus          string `json:"agentStatus"`
		AgentStopReason      string `json:"agentStopReason"`
		TranscriptArtifact   *struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"transcriptArtifact"`
	}
	if err := json.Unmarshal(resultJSON, &result); err != nil || !validLocalTaskContinuationState(result.AgentStatus, result.AgentStopReason) || result.SourceConversationID == "" || result.ConversationID == "" || result.AgentTurnID == "" || result.ResultMessageID == "" || result.TranscriptArtifact == nil || result.TranscriptArtifact.ID == "" || len(result.TranscriptArtifact.SHA256) != 64 || result.TranscriptArtifact.ByteSize <= 0 {
		write(w, http.StatusConflict, map[string]string{"error": "task result has no valid transcript to audit for cloud continuation"})
		return
	}
	links, err := s.store.ExecutionTaskArtifacts(r.Context(), s.workspaceID, task.ID)
	if err != nil {
		fail(w, err)
		return
	}
	attached := false
	for _, link := range links {
		role := "conversation_transcript"
		if recoveryContinuation {
			role = "recovery_transcript"
		}
		if link.ID == result.TranscriptArtifact.ID && link.Role == role && link.SHA256 == result.TranscriptArtifact.SHA256 && link.ByteSize == result.TranscriptArtifact.ByteSize {
			attached = true
			break
		}
	}
	if !attached {
		write(w, http.StatusConflict, map[string]string{"error": "task transcript is not attached with its durable manifest"})
		return
	}
	artifact, file, err := s.artifacts.Open(r.Context(), s.workspaceID, result.TranscriptArtifact.ID)
	if err != nil {
		fail(w, err)
		return
	}
	var transcript taskTranscriptEnvelope
	decodeErr := json.NewDecoder(file).Decode(&transcript)
	closeErr := file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		fail(w, fmt.Errorf("read verified task transcript for handoff preview: %w", err))
		return
	}
	if artifact.ID != result.TranscriptArtifact.ID || artifact.SHA256 != result.TranscriptArtifact.SHA256 || artifact.ByteSize != result.TranscriptArtifact.ByteSize || transcript.SourceConversationID != result.SourceConversationID || transcript.ConversationID != result.ConversationID || transcript.AgentTurnID != result.AgentTurnID || transcript.ResultMessageID != result.ResultMessageID || transcript.AgentStatus != result.AgentStatus || transcript.AgentStopReason != result.AgentStopReason || transcript.Transcript.Conversation.ID != result.ConversationID {
		write(w, http.StatusConflict, map[string]string{"error": "verified task transcript identity differs from its durable result"})
		return
	}
	effects, err := summarizeTaskHandoffEffects(transcript.Transcript.Trace, transcript.ConversationID, transcript.AgentTurnID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"taskId": task.ID, "effects": effects, "requiresAcknowledgement": len(effects) > 0, "requiresOldNodeStopConfirmation": recoveryContinuation, "recoveryContinuation": recoveryContinuation})
}
