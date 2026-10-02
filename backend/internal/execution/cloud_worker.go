package execution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

const cloudLeaseDuration = 60 * time.Second
const cloudLeasePulseInterval = 15 * time.Second
const cloudLeaseReconciliationInterval = 15 * time.Second

type CloudWorker struct {
	store         *storage.Store
	agent         AgentTaskRuntime
	userID        string
	nodeID        string
	pulseInterval time.Duration
	concurrency   int
	capacity      cloudResourceCapacity
	loopLimit     int
}

type AgentTaskRuntime interface {
	ExecuteTaskTurn(context.Context, string, string, string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error)
}

// AgentExecutionTaskRuntime receives the durable execution-task ID so the
// Agent host can bind a turn or continuation to its isolated workspace. Older
// runtimes remain compatible until they implement task-workspace isolation.
type AgentExecutionTaskRuntime interface {
	ExecuteExecutionTaskTurn(context.Context, string, string, string, string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error)
	ExecuteExecutionTaskContinuation(context.Context, string, string, string, string, string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error)
}

type AgentExecutionTaskWorkspaceReader interface {
	ExecutionTaskWorkspace(context.Context, string, string, string) (domain.Project, error)
}

type AgentExecutionTaskWorkspaceFinalizer interface {
	FinalizeExecutionTaskWorkspace(context.Context, string, string, string) (domain.Project, *storage.Artifact, string, string, error)
}

type AgentContinuationTaskRuntime interface {
	ExecuteTaskContinuation(context.Context, string, string, string, string) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error)
}

type cloudTaskPayload struct {
	Kind           string                   `json:"kind"`
	ConversationID string                   `json:"conversationId"`
	Content        string                   `json:"content"`
	TurnID         string                   `json:"turnId,omitempty"`
	IdempotencyKey string                   `json:"idempotencyKey,omitempty"`
	Attachments    []cloudTaskInputArtifact `json:"attachments,omitempty"`
}

type cloudTaskInputArtifact struct {
	ArtifactID string `json:"artifactId"`
	SHA256     string `json:"sha256"`
	ByteSize   int64  `json:"byteSize"`
	FileName   string `json:"fileName"`
	MediaType  string `json:"mediaType"`
}

func NewCloudWorker(store *storage.Store, runtime AgentTaskRuntime, userID, nodeID string) (*CloudWorker, error) {
	if store == nil || runtime == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(nodeID) == "" {
		return nil, domain.ErrInvalid
	}
	return &CloudWorker{store: store, agent: runtime, userID: userID, nodeID: nodeID, pulseInterval: cloudLeasePulseInterval, concurrency: 1, capacity: hostCloudTaskSlots, loopLimit: hostCloudWorkerLoopLimit()}, nil
}

// SetConcurrency sets the operator's upper bound for independent cloud tasks.
// Run applies live CPU and memory admission below that ceiling, so spare host
// capacity can be used without starting more maximum-sized sandboxes than fit.
func (w *CloudWorker) SetConcurrency(limit int) error {
	if w == nil || limit < 1 || limit > domain.MaxCloudWorkerConcurrency {
		return domain.ErrInvalid
	}
	w.concurrency = limit
	return nil
}

// SetDiskAdmissionBudget makes live worker admission reserve one configured
// task workspace quota per slot and preserve shared filesystem headroom. Low
// disk capacity queues tasks instead of claiming work that cannot fit.
func (w *CloudWorker) SetDiskAdmissionBudget(workspaceRoot string, taskDiskBudgetBytes, diskReserveBytes int64) error {
	if w == nil || strings.TrimSpace(workspaceRoot) == "" || taskDiskBudgetBytes <= 0 || diskReserveBytes < 0 {
		return domain.ErrInvalid
	}
	root, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("inspect cloud task workspace root for disk admission: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cloud task workspace root for disk admission is not a directory: %w", domain.ErrInvalid)
	}
	w.capacity = func() (int, error) {
		return hostCloudTaskSlotsWithDisk(root, taskDiskBudgetBytes, diskReserveBytes)
	}
	return nil
}

func (w *CloudWorker) Run(ctx context.Context) {
	workers := w.concurrency
	if workers < 1 {
		workers = 1
	}
	if hostLimit := w.loopLimit; hostLimit > 0 && workers > hostLimit {
		workers = hostLimit
	}
	var group sync.WaitGroup
	capacity := w.capacity
	if capacity == nil {
		capacity = hostCloudTaskSlots
	}
	gate := newCloudResourceAdmission(workers, capacity)
	group.Add(workers)
	group.Add(1)
	go func() {
		defer group.Done()
		w.reconcileExpiredLeasesLoop(ctx)
	}()
	for range workers {
		go func() {
			defer group.Done()
			w.runLoop(ctx, gate)
		}()
	}
	group.Wait()
}

func (w *CloudWorker) reconcileExpiredLeasesLoop(ctx context.Context) {
	if _, err := w.reconcileExpiredLeases(ctx, time.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("expired execution task lease reconciliation failed; will retry", "error", err)
	}
	ticker := time.NewTicker(cloudLeaseReconciliationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if _, err := w.reconcileExpiredLeases(ctx, now.UTC()); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("expired execution task lease reconciliation failed; will retry", "error", err)
			}
		}
	}
}

func (w *CloudWorker) reconcileExpiredLeases(ctx context.Context, now time.Time) (int, error) {
	if w == nil || w.store == nil || strings.TrimSpace(w.userID) == "" {
		return 0, domain.ErrInvalid
	}
	return w.store.ReconcileExpiredExecutionTaskLeasesForUser(ctx, w.userID, now)
}

func (w *CloudWorker) runLoop(ctx context.Context, gate *cloudResourceAdmission) {
	delay := 500 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		release, err := gate.Acquire(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.Error("cloud resource admission failed; worker will retry", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if delay < 30*time.Second {
				delay *= 2
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
			}
			continue
		}
		err = w.RunOne(ctx)
		release()
		if err != nil {
			if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, context.Canceled) {
				slog.Error("cloud execution task failed", "error", err)
				if delay < 30*time.Second {
					delay *= 2
					if delay > 30*time.Second {
						delay = 30 * time.Second
					}
				}
			} else {
				delay = 500 * time.Millisecond
			}
		} else {
			delay = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (w *CloudWorker) RunOne(ctx context.Context) error {
	leaseBytes := make([]byte, 32)
	if _, err := rand.Read(leaseBytes); err != nil {
		return err
	}
	leaseToken := base64.RawURLEncoding.EncodeToString(leaseBytes)
	leaseHashBytes := sha256.Sum256([]byte(leaseToken))
	leaseHash := hex.EncodeToString(leaseHashBytes[:])
	now := time.Now().UTC()
	task, err := w.store.ClaimExecutionTask(ctx, w.nodeID, leaseHash, now.Add(cloudLeaseDuration), now)
	if err != nil {
		return err
	}
	if _, err := w.store.ReportExecutionTask(ctx, w.nodeID, task.ID, leaseHash, "accepted", nil, "", time.Now().UTC()); err != nil {
		return fmt.Errorf("record cloud task acceptance: %w", err)
	}
	if _, err := w.store.ReportExecutionTask(ctx, w.nodeID, task.ID, leaseHash, "running", nil, "", time.Now().UTC()); err != nil {
		return fmt.Errorf("record cloud task start: %w", err)
	}
	var input cloudTaskPayload
	if err := json.Unmarshal(task.Payload, &input); err != nil || !validCloudTaskPayload(input) {
		return w.reportFailure(ctx, task.ID, leaseHash, "task payload is missing valid Agent turn or continuation identity", false)
	}
	if input.Kind == "agent_turn" {
		if err := w.verifyTaskInputArtifacts(ctx, task.ID, input.Attachments); err != nil {
			return w.reportFailure(ctx, task.ID, leaseHash, "task input artifacts failed durable manifest verification: "+err.Error(), false)
		}
	}
	turnReceipt, turn, _, runErr, leaseErr := w.runTurnAndPulse(ctx, task.ID, leaseHash, input)
	if leaseErr != nil {
		return leaseErr
	}
	if turn.Status == "needs_reconciliation" {
		return w.reportFailure(ctx, task.ID, leaseHash, "Agent turn has an unresolved external effect and requires reconciliation", true)
	}
	if turn.Status == "cancelled" {
		_, err := w.store.ReportExecutionTask(ctx, w.nodeID, task.ID, leaseHash, "cancelled", nil, "", time.Now().UTC())
		if err != nil {
			return fmt.Errorf("persist cancelled cloud task: %w", err)
		}
		return nil
	}
	if turn.Status == "incomplete" && turn.ContinuationAvailable && runErr == nil && turnReceipt.TurnID != "" && turnReceipt.ConversationID == input.ConversationID {
		paused, err := w.store.PauseAgentExecutionTask(ctx, w.nodeID, task.ID, leaseHash, turn.ID, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("persist safe non-complete Agent task state: %w", err)
		}
		if paused.Status != "incomplete" || paused.LeaseUntil != nil || paused.Result == nil {
			return domain.ErrConflict
		}
		return nil
	}
	if runErr != nil || turn.Status != "completed" || turnReceipt.TurnID == "" || turnReceipt.ConversationID != input.ConversationID {
		reason := "Agent runtime did not verify a completed turn"
		if runErr != nil {
			reason = runErr.Error()
		} else if turn.Status != "" {
			reason = "Agent turn ended in state " + turn.Status
		}
		return w.reportFailure(ctx, task.ID, leaseHash, reason, false)
	}
	resultFields := map[string]any{"agentTurnId": turn.ID, "conversationId": turn.ConversationID, "resultMessageId": turn.ResultMessageID, "agentStatus": turn.Status}
	if finalizer, ok := w.agent.(AgentExecutionTaskWorkspaceFinalizer); ok {
		project, artifact, baseCommit, commit, workspaceErr := finalizer.FinalizeExecutionTaskWorkspace(ctx, w.userID, task.ID, turn.ConversationID)
		if workspaceErr != nil {
			return w.reportFailure(ctx, task.ID, leaseHash, "workspace_output_finalization_failed: "+workspaceErr.Error(), true)
		}
		if project.ID != "" {
			resultFields["executionProjectId"] = project.ID
		}
		if artifact != nil {
			readBackArtifact, artifactErr := w.store.Artifact(ctx, w.userID, artifact.ID)
			if artifactErr != nil || readBackArtifact.SHA256 != artifact.SHA256 || readBackArtifact.ByteSize != artifact.ByteSize || readBackArtifact.FileName != artifact.FileName {
				return w.reportFailure(ctx, task.ID, leaseHash, "workspace_output_artifact_readback_failed", true)
			}
			attachedArtifacts, attachErr := w.store.ExecutionTaskArtifacts(ctx, w.userID, task.ID)
			if attachErr != nil {
				return w.reportFailure(ctx, task.ID, leaseHash, "workspace_output_attachment_readback_failed: "+attachErr.Error(), true)
			}
			attached := false
			for _, item := range attachedArtifacts {
				if item.ID == artifact.ID && item.Role == "workspace_output" && item.SHA256 == artifact.SHA256 && item.ByteSize == artifact.ByteSize {
					attached = true
					break
				}
			}
			if !attached {
				return w.reportFailure(ctx, task.ID, leaseHash, "workspace_output_attachment_not_persisted", true)
			}
			resultFields["workspaceOutputArtifact"] = map[string]any{"id": artifact.ID, "fileName": artifact.FileName, "sha256": artifact.SHA256, "byteSize": artifact.ByteSize, "mediaType": artifact.MediaType}
		}
		if baseCommit != "" {
			resultFields["workspaceOutputBaseCommit"] = baseCommit
		}
		if commit != "" {
			resultFields["workspaceOutputCommit"] = commit
		}
	} else if workspaceReader, ok := w.agent.(AgentExecutionTaskWorkspaceReader); ok {
		project, workspaceErr := workspaceReader.ExecutionTaskWorkspace(ctx, w.userID, task.ID, turn.ConversationID)
		if workspaceErr != nil {
			return fmt.Errorf("read back cloud execution task workspace: %w", workspaceErr)
		}
		if project.ID != "" {
			resultFields["executionProjectId"] = project.ID
		}
	}
	result, err := json.Marshal(resultFields)
	if err != nil {
		return err
	}
	completed, err := w.store.CompleteAgentExecutionTask(ctx, w.nodeID, task.ID, leaseHash, turn.ID, result, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("commit Agent-verified cloud task completion: %w", err)
	}
	if completed.Status != "completed" || string(completed.Result) != string(result) {
		return domain.ErrConflict
	}
	return nil
}

func validCloudTaskPayload(input cloudTaskPayload) bool {
	switch input.Kind {
	case "agent_turn":
		if strings.TrimSpace(input.ConversationID) == "" || strings.TrimSpace(input.Content) == "" || len(input.Content) > 100000 || input.TurnID != "" || input.IdempotencyKey != "" || len(input.Attachments) > 128 {
			return false
		}
		seen := make(map[string]struct{}, len(input.Attachments))
		for _, attachment := range input.Attachments {
			if strings.TrimSpace(attachment.ArtifactID) == "" || len(attachment.SHA256) != 64 || attachment.ByteSize < 0 || strings.TrimSpace(attachment.FileName) == "" {
				return false
			}
			if _, err := hex.DecodeString(attachment.SHA256); err != nil {
				return false
			}
			if _, exists := seen[attachment.ArtifactID]; exists {
				return false
			}
			seen[attachment.ArtifactID] = struct{}{}
		}
		return true
	case "agent_continuation":
		return strings.TrimSpace(input.ConversationID) != "" && strings.TrimSpace(input.TurnID) != "" && strings.TrimSpace(input.Content) != "" && len(input.Content) <= 20<<10 && strings.TrimSpace(input.IdempotencyKey) != "" && len(input.IdempotencyKey) <= 200
	default:
		return false
	}
}

func (w *CloudWorker) verifyTaskInputArtifacts(ctx context.Context, taskID string, expected []cloudTaskInputArtifact) error {
	attached, err := w.store.ExecutionTaskArtifacts(ctx, w.userID, taskID)
	if err != nil {
		return fmt.Errorf("read task artifact links: %w", err)
	}
	byID := make(map[string]storage.ExecutionTaskArtifact, len(attached))
	for _, artifact := range attached {
		if artifact.Role == "user_input" {
			byID[artifact.ID] = artifact
		}
	}
	if len(byID) != len(expected) {
		return fmt.Errorf("user input artifact link count differs from task payload: %w", domain.ErrConflict)
	}
	for _, manifest := range expected {
		artifact, ok := byID[manifest.ArtifactID]
		if !ok || artifact.SHA256 != manifest.SHA256 || artifact.ByteSize != manifest.ByteSize || artifact.FileName != manifest.FileName || artifact.MediaType != manifest.MediaType {
			return fmt.Errorf("user input artifact %q does not match its durable task link: %w", manifest.ArtifactID, domain.ErrConflict)
		}
	}
	return nil
}

func (w *CloudWorker) executeTaskTurn(ctx context.Context, executionTaskID string, input cloudTaskPayload) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error) {
	switch input.Kind {
	case "agent_turn":
		if runtime, ok := w.agent.(AgentExecutionTaskRuntime); ok {
			return runtime.ExecuteExecutionTaskTurn(ctx, w.userID, executionTaskID, input.ConversationID, input.Content)
		}
		return w.agent.ExecuteTaskTurn(ctx, w.userID, input.ConversationID, input.Content)
	case "agent_continuation":
		if runtime, ok := w.agent.(AgentExecutionTaskRuntime); ok {
			return runtime.ExecuteExecutionTaskContinuation(ctx, w.userID, executionTaskID, input.TurnID, input.Content, input.IdempotencyKey)
		}
		runtime, ok := w.agent.(AgentContinuationTaskRuntime)
		if !ok {
			return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, errors.New("cloud Agent runtime does not support durable continuation tasks")
		}
		return runtime.ExecuteTaskContinuation(ctx, w.userID, input.TurnID, input.Content, input.IdempotencyKey)
	default:
		return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, domain.ErrInvalid
	}
}

func (w *CloudWorker) runTurnAndPulse(ctx context.Context, taskID, leaseHash string, input cloudTaskPayload) (domain.TurnReceipt, domain.AgentTurn, domain.Message, error, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		receipt domain.TurnReceipt
		turn    domain.AgentTurn
		message domain.Message
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		receipt, turn, message, err := w.executeTaskTurn(runCtx, taskID, input)
		finished <- result{receipt: receipt, turn: turn, message: message, err: err}
	}()
	ticker := time.NewTicker(w.pulseInterval)
	defer ticker.Stop()
	for {
		select {
		case done := <-finished:
			return done.receipt, done.turn, done.message, done.err, nil
		case <-ctx.Done():
			cancel()
			done := <-finished
			return done.receipt, done.turn, done.message, done.err, ctx.Err()
		case <-ticker.C:
			current, err := w.store.PulseExecutionTask(ctx, w.nodeID, taskID, leaseHash, time.Now().UTC())
			if err != nil {
				cancel()
				<-finished
				return domain.TurnReceipt{}, domain.AgentTurn{}, domain.Message{}, nil, fmt.Errorf("cloud task lease lost; Agent turn was stopped: %w", err)
			}
			if current.CancelRequested {
				cancel()
			}
		}
	}
}

func (w *CloudWorker) reportFailure(ctx context.Context, taskID, leaseHash, reason string, reconcile bool) error {
	status := "reported_failed"
	if reconcile {
		status = "needs_reconciliation"
	}
	_, err := w.store.ReportExecutionTask(ctx, w.nodeID, taskID, leaseHash, status, nil, reason, time.Now().UTC())
	if err != nil {
		return errors.Join(fmt.Errorf("persist cloud execution state %s: %s", status, reason), err)
	}
	return nil
}
