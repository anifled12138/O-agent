package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
	"github.com/coder/websocket"
)

const (
	nodeHeartbeatInterval  = 30 * time.Second
	nodeLeasePulseInterval = 15 * time.Second
	nodeRequestTimeout     = 20 * time.Second
	maxNodeResponseBytes   = 2 << 20
)

// NodeTaskExecutor performs a task on this computer. The returned status must
// distinguish a verified local failure from an outcome that needs human
// reconciliation after cancellation or an uncertain external side effect.
type NodeTaskExecutor interface {
	ExecuteNodeTask(context.Context, storage.ExecutionTask) (json.RawMessage, error)
}

type leasedNodeTaskPreparer interface {
	PrepareNodeTask(context.Context, storage.ExecutionTask, *nodeAPIClient, string) (storage.ExecutionTask, error)
}

type leaseAwareNodeTaskExecutor interface {
	ExecuteLeasedNodeTask(context.Context, storage.ExecutionTask, string) (json.RawMessage, error)
}

type safePauseNodeTaskExecutor interface {
	RequestSafePause(string) error
}

// NodeTaskOutcomeError lets an executor explicitly report an uncertain result.
// Ordinary errors become reported_failed; they are never silently ignored.
type NodeTaskOutcomeError struct {
	Status string
	Err    error
}

func (e *NodeTaskOutcomeError) Error() string {
	if e == nil || e.Err == nil {
		return "node task outcome requires reconciliation"
	}
	return e.Err.Error()
}

func (e *NodeTaskOutcomeError) Unwrap() error { return e.Err }

type NodeWorker struct {
	client         *nodeAPIClient
	executor       NodeTaskExecutor
	platform       string
	capabilities   []string
	heartbeatEvery time.Duration
	pulseEvery     time.Duration
	pollEvery      time.Duration
	concurrency    int
	resourceMu     sync.Mutex
	resourceAt     time.Time
	resources      storage.ExecutionNodeResources
	wakeMu         sync.Mutex
	wakeEvent      chan struct{}
	wakeOnline     atomic.Bool
	activeTasks    atomic.Int32
	resourceReader func() (storage.ExecutionNodeResources, error)
	outbox         *nodeTaskOutbox
}

// SetOutboxDirectory enables crash-safe local result persistence. The outbox
// contains short-lived node lease credentials and is restricted to this user.
func (w *NodeWorker) SetOutboxDirectory(path string) error {
	if w == nil {
		return domain.ErrInvalid
	}
	outbox, err := newNodeTaskOutbox(path)
	if err != nil {
		return err
	}
	w.outbox = outbox
	return nil
}

type nodeAPIClient struct {
	baseURL    string
	credential string
	http       *http.Client
}

type nodeClaim struct {
	Task       storage.ExecutionTask `json:"task"`
	LeaseToken string                `json:"leaseToken"`
	LeaseUntil time.Time             `json:"leaseUntil"`
}

type nodeTaskReport struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func NewNodeWorker(controlURL, credential, platform string, capabilities []string, executor NodeTaskExecutor) (*NodeWorker, error) {
	if executor == nil || len(strings.TrimSpace(credential)) < 32 || !validNodeWorkerPlatform(platform) {
		return nil, domain.ErrInvalid
	}
	base, err := url.Parse(strings.TrimSpace(controlURL))
	if err != nil || base.User != nil || base.Host == "" || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("node control URL must be an absolute HTTP(S) URL without credentials, query, or fragment: %w", domain.ErrInvalid)
	}
	if base.Scheme != "https" && !isLoopbackURLHost(base.Hostname()) {
		return nil, fmt.Errorf("node credentials may only be sent over HTTPS to a non-loopback control server: %w", domain.ErrInvalid)
	}
	base.Path = strings.TrimRight(base.Path, "/")
	httpClient := &http.Client{
		Transport: &http.Transport{ResponseHeaderTimeout: nodeRequestTimeout, IdleConnTimeout: 90 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &NodeWorker{
		client:   &nodeAPIClient{baseURL: strings.TrimRight(base.String(), "/"), credential: strings.TrimSpace(credential), http: httpClient},
		executor: executor, platform: platform, capabilities: normalizeCapabilities(capabilities),
		heartbeatEvery: nodeHeartbeatInterval, pulseEvery: nodeLeasePulseInterval, pollEvery: 2 * time.Second, concurrency: domain.MaxLocalNodeWorkerConcurrency, wakeEvent: make(chan struct{}),
	}, nil
}

// SetConcurrency configures the node's maximum worker ceiling. Live memory and
// CPU admission still determine how many tasks it can claim at a given time.
func (w *NodeWorker) SetConcurrency(limit int) error {
	if w == nil || limit < 1 || limit > domain.MaxLocalNodeWorkerConcurrency {
		return domain.ErrInvalid
	}
	w.concurrency = limit
	return nil
}

func (w *NodeWorker) Run(ctx context.Context) error {
	if w == nil || w.client == nil || w.executor == nil {
		return domain.ErrInvalid
	}
	resources, err := w.readResources()
	if err != nil {
		return fmt.Errorf("read initial local execution capacity: %w", err)
	}
	resources = w.limitResources(resources)
	if err := w.reportResources(ctx, resources, true); err != nil {
		return fmt.Errorf("initial node heartbeat failed: %w", err)
	}
	if err := w.recoverOutbox(ctx); err != nil {
		return fmt.Errorf("recover local node task outbox: %w", err)
	}
	heartbeatDone := make(chan error, 1)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go func() { heartbeatDone <- w.heartbeatLoop(heartbeatCtx) }()
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	wakeFailure := make(chan error, 1)
	go w.wakeLoop(workerCtx, wakeFailure)
	var workers sync.WaitGroup
	workerCount := nodeWorkerPollerCount(w.concurrency, resources.LogicalCPUs)
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w.pollLoop(workerCtx)
		}()
	}
	select {
	case <-ctx.Done():
		cancelWorkers()
		workers.Wait()
		return ctx.Err()
	case err := <-heartbeatDone:
		cancelHeartbeat()
		cancelWorkers()
		workers.Wait()
		if err == nil {
			return errors.New("node heartbeat loop stopped unexpectedly")
		}
		return err
	case err := <-wakeFailure:
		cancelHeartbeat()
		cancelWorkers()
		workers.Wait()
		if err == nil {
			return errors.New("node wake channel stopped unexpectedly")
		}
		return err
	}
}

func nodeWorkerPollerCount(configuredCeiling, logicalCPUs int) int {
	count := configuredCeiling
	if logicalCPUs > 0 && count > logicalCPUs {
		count = logicalCPUs
	}
	if count < 1 {
		return 1
	}
	return count
}

func (w *NodeWorker) pollLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		err := w.RunOne(ctx)
		if err != nil && !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrBusy) && !errors.Is(err, context.Canceled) {
			slog.Error("local execution node task failed", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.pollEvery):
			}
			continue
		}
		if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrBusy) {
			w.waitForWakeOrPoll(ctx)
		}
	}
}

func (w *NodeWorker) broadcastWake() {
	w.wakeMu.Lock()
	if w.wakeEvent == nil {
		w.wakeEvent = make(chan struct{})
	}
	close(w.wakeEvent)
	w.wakeEvent = make(chan struct{})
	w.wakeMu.Unlock()
}

func (w *NodeWorker) waitForWakeOrPoll(ctx context.Context) {
	w.wakeMu.Lock()
	wake := w.wakeEvent
	interval := w.pollEvery
	if w.wakeOnline.Load() {
		interval = 30 * time.Second
	}
	w.wakeMu.Unlock()
	if interval <= 0 {
		interval = 2 * time.Second
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-wake:
	case <-timer.C:
	}
}

func (w *NodeWorker) wakeLoop(ctx context.Context, fatal chan<- error) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := w.client.connectWake(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, domain.ErrUnauthorized) {
				fatal <- fmt.Errorf("local node wake connection authorization failed: %w", err)
				return
			}
			w.wakeOnline.Store(false)
			w.broadcastWake()
			slog.Warn("node WSS wake connection unavailable; polling remains active", "error", err)
			if !waitNodeWakeBackoff(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		w.wakeOnline.Store(true)
		w.broadcastWake()
		err = w.client.readWakeMessages(ctx, conn, w.broadcastWake)
		_ = conn.Close(websocket.StatusNormalClosure, "reconnecting node wake channel")
		w.wakeOnline.Store(false)
		w.broadcastWake()
		if ctx.Err() != nil {
			return
		}
		closeStatus := websocket.CloseStatus(err)
		if closeStatus == websocket.StatusPolicyViolation || closeStatus == websocket.StatusProtocolError {
			fatal <- fmt.Errorf("local node wake channel rejected this credential or protocol: %w", errors.Join(domain.ErrUnauthorized, err))
			return
		}
		if err != nil {
			slog.Warn("node WSS wake connection closed; polling remains active", "error", err)
		}
		if !waitNodeWakeBackoff(ctx, backoff) {
			return
		}
	}
}

func waitNodeWakeBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *NodeWorker) heartbeatLoop(ctx context.Context) error {
	ticker := time.NewTicker(w.heartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			resources, err := w.readResources()
			if err != nil {
				return fmt.Errorf("read local execution capacity: %w", err)
			}
			resources = w.limitResources(resources)
			if err := w.reportResources(ctx, resources, true); err != nil {
				return fmt.Errorf("node heartbeat failed: %w", err)
			}
		}
	}
}

// RunOne claims and executes at most one task. ErrNotFound means the server
// confirmed that no task was available for this node.
func (w *NodeWorker) RunOne(ctx context.Context) error {
	resources, err := w.readResources()
	if err != nil {
		return fmt.Errorf("read local execution capacity before claim: %w", err)
	}
	resources = w.limitResources(resources)
	if err := w.reportResources(ctx, resources, false); err != nil {
		return fmt.Errorf("publish local capacity before claim: %w", err)
	}
	if !w.reserveTaskSlot(resources.MaxConcurrentTasks) {
		return domain.ErrBusy
	}
	defer w.activeTasks.Add(-1)
	claim, err := w.client.claim(ctx)
	if err != nil {
		return err
	}
	if claim.LeaseToken == "" || claim.Task.ID == "" || claim.Task.NodeID == "" {
		return errors.New("node claim response omitted task or lease identity")
	}
	if err := w.client.report(ctx, claim.Task.ID, claim.LeaseToken, nodeTaskReport{Status: "accepted"}); err != nil {
		return fmt.Errorf("record node task acceptance: %w", err)
	}
	if err := w.client.report(ctx, claim.Task.ID, claim.LeaseToken, nodeTaskReport{Status: "running"}); err != nil {
		return fmt.Errorf("record node task start: %w", err)
	}
	result, runErr, leaseErr := w.executeWithPulses(ctx, claim.Task, claim.LeaseToken)
	if leaseErr != nil {
		return leaseErr
	}
	if runErr != nil {
		status := "reported_failed"
		var outcome *NodeTaskOutcomeError
		if errors.As(runErr, &outcome) {
			if outcome.Status != "needs_reconciliation" && outcome.Status != "cancelled" {
				return fmt.Errorf("executor returned unsupported task status %q: %w", outcome.Status, runErr)
			}
			status = outcome.Status
		}
		if status == "cancelled" {
			if err := w.client.report(ctx, claim.Task.ID, claim.LeaseToken, nodeTaskReport{Status: status}); err != nil {
				return fmt.Errorf("persist cancelled node task: %w", err)
			}
			return nil
		}
		if err := w.client.report(ctx, claim.Task.ID, claim.LeaseToken, nodeTaskReport{Status: status, Error: runErr.Error()}); err != nil {
			return errors.Join(fmt.Errorf("persist node task failure: %w", runErr), err)
		}
		return nil
	}
	if len(result) == 0 || !json.Valid(result) || bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		return w.client.reportFailure(ctx, claim.Task.ID, claim.LeaseToken, "node executor returned an empty or invalid durable result", false)
	}
	if err := w.client.report(ctx, claim.Task.ID, claim.LeaseToken, nodeTaskReport{Status: "reported_succeeded", Result: result}); err != nil {
		return fmt.Errorf("persist node task outcome: %w", err)
	}
	if w.outbox != nil {
		if err := w.outbox.Remove(claim.Task.ID); err != nil {
			return fmt.Errorf("cloud task result is durable but local outbox cleanup failed: %w", err)
		}
	}
	return nil
}

func (w *NodeWorker) reserveTaskSlot(capacity int) bool {
	for capacity > 0 {
		active := w.activeTasks.Load()
		if int(active) >= capacity {
			return false
		}
		if w.activeTasks.CompareAndSwap(active, active+1) {
			return true
		}
	}
	return false
}

func (w *NodeWorker) limitResources(resources storage.ExecutionNodeResources) storage.ExecutionNodeResources {
	if w != nil && resources.MaxConcurrentTasks > w.concurrency {
		resources.MaxConcurrentTasks = w.concurrency
	}
	return resources
}

func (w *NodeWorker) readResources() (storage.ExecutionNodeResources, error) {
	if w != nil && w.resourceReader != nil {
		return w.resourceReader()
	}
	return hostNodeResources()
}

func (w *NodeWorker) reportResources(ctx context.Context, resources storage.ExecutionNodeResources, force bool) error {
	w.resourceMu.Lock()
	defer w.resourceMu.Unlock()
	if !force && !w.resourceAt.IsZero() && time.Since(w.resourceAt) < 10*time.Second && w.resources.MaxConcurrentTasks == resources.MaxConcurrentTasks {
		return nil
	}
	if err := w.client.heartbeatWithResources(ctx, w.platform, w.capabilities, resources); err != nil {
		return err
	}
	capacityIncreased := resources.MaxConcurrentTasks > w.resources.MaxConcurrentTasks
	w.resources = resources
	w.resourceAt = time.Now()
	if capacityIncreased {
		// A sleeping poller may have been parked after the host ran out of
		// memory/CPU slots. Wake it as soon as a verified heartbeat reports
		// more capacity so it does not wait for the long connected-socket poll.
		w.broadcastWake()
	}
	return nil
}

func (w *NodeWorker) executeWithPulses(ctx context.Context, task storage.ExecutionTask, lease string) (json.RawMessage, error, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var phaseMu sync.Mutex
	progressPhase := ""
	phaseChanged := make(chan string, 16)
	setProgressPhase := func(phase string) {
		phaseMu.Lock()
		if localNodeProgressPhaseRank(phase) < localNodeProgressPhaseRank(progressPhase) {
			phaseMu.Unlock()
			return
		}
		changed := progressPhase != phase
		progressPhase = phase
		phaseMu.Unlock()
		if changed {
			phaseChanged <- phase
		}
	}
	getProgressPhase := func() string {
		phaseMu.Lock()
		defer phaseMu.Unlock()
		return progressPhase
	}
	setProgressPhase("preparing")
	type executionResult struct {
		result json.RawMessage
		err    error
	}
	finished := make(chan executionResult, 1)
	handoffRequestLogged := false
	go func() {
		var result json.RawMessage
		var err error
		if preparer, ok := w.executor.(leasedNodeTaskPreparer); ok {
			task, err = preparer.PrepareNodeTask(runCtx, task, w.client, lease)
		}
		if err == nil {
			setProgressPhase("agent_and_snapshot")
			if leaseAware, ok := w.executor.(leaseAwareNodeTaskExecutor); ok {
				result, err = leaseAware.ExecuteLeasedNodeTask(runCtx, task, lease)
			} else {
				result, err = w.executor.ExecuteNodeTask(runCtx, task)
			}
		}
		if err == nil && len(result) > 0 && json.Valid(result) && !bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
			if localTaskResultReachedHandoffBoundary(result) {
				setProgressPhase("safe_boundary_reached")
			}
			var outboxRecord nodeOutboxRecord
			if w.outbox != nil {
				outboxRecord, err = w.outbox.Save(task.ID, lease, result)
				if err == nil {
					result = outboxRecord.Result
				}
				if err != nil {
					err = &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: fmt.Errorf("persist local task result before upload: %w", err)}
				} else if w.outbox != nil {
					setProgressPhase("local_result_durable")
				}
			}
			if err == nil {
				setProgressPhase("uploading_artifacts")
				if w.outbox != nil {
					result, err = w.client.syncTranscriptArtifactWithRetryPreservingLocalFiles(runCtx, task.ID, lease, result)
				} else {
					result, err = w.client.syncTranscriptArtifactWithRetry(runCtx, task.ID, lease, result)
				}
				if err == nil && w.outbox != nil {
					_, err = w.outbox.SaveUploadedResult(outboxRecord, result)
					if err != nil {
						err = &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: fmt.Errorf("persist cloud-verified artifact result: %w", err)}
					}
				}
				if err == nil {
					setProgressPhase("artifacts_verified")
				}
			}
		}
		finished <- executionResult{result: result, err: err}
	}()
	ticker := time.NewTicker(w.pulseEvery)
	defer ticker.Stop()
	for {
		var pulsePhase string
		select {
		case done := <-finished:
			for {
				select {
				case phase := <-phaseChanged:
					if _, err := w.client.pulseWithPhase(ctx, task.ID, lease, phase); err != nil {
						return nil, nil, fmt.Errorf("node task lease lost while recording final progress: %w", err)
					}
				default:
					return done.result, done.err, nil
				}
			}
		case <-ctx.Done():
			cancel()
			done := <-finished
			if done.err != nil {
				return done.result, done.err, nil
			}
			return done.result, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: fmt.Errorf("local task stopped with runtime shutdown; result may be incomplete: %w", ctx.Err())}, nil
		case <-ticker.C:
			pulsePhase = getProgressPhase()
		case pulsePhase = <-phaseChanged:
		}
		current, err := w.client.pulseWithPhase(ctx, task.ID, lease, pulsePhase)
		if err != nil {
			cancel()
			<-finished
			return nil, nil, fmt.Errorf("node task lease lost; local execution was stopped: %w", err)
		}
		if current.CancelRequested {
			cancel()
			outcome := <-finished
			if outcome.err != nil {
				var explicit *NodeTaskOutcomeError
				if errors.As(outcome.err, &explicit) {
					return outcome.result, outcome.err, nil
				}
				return outcome.result, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: fmt.Errorf("task cancellation stopped local execution without a verified terminal state: %w", outcome.err)}, nil
			}
			return outcome.result, &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: errors.New("cancelled local task returned without a verified stop outcome")}, nil
		}
		if current.HandoffRequested {
			setProgressPhase("waiting_for_safe_boundary")
			pauser, ok := w.executor.(safePauseNodeTaskExecutor)
			if !ok {
				if !handoffRequestLogged {
					slog.Error("node advertised safe handoff but its task executor cannot receive pause requests", "task_id", task.ID)
					handoffRequestLogged = true
				}
			} else if err := pauser.RequestSafePause(task.ID); err != nil {
				if !handoffRequestLogged {
					slog.Error("node could not deliver safe handoff request to its Agent executor", "task_id", task.ID, "error", err)
					handoffRequestLogged = true
				}
			}
		}
	}
}

func localNodeProgressPhaseRank(phase string) int {
	switch phase {
	case "":
		return 0
	case "preparing":
		return 1
	case "agent_and_snapshot":
		return 2
	case "waiting_for_safe_boundary":
		return 3
	case "safe_boundary_reached":
		return 4
	case "local_result_durable":
		return 5
	case "uploading_artifacts":
		return 6
	case "artifacts_verified":
		return 7
	default:
		return -1
	}
}

func localTaskResultReachedHandoffBoundary(result json.RawMessage) bool {
	var state struct {
		AgentStatus     string `json:"agentStatus"`
		AgentStopReason string `json:"agentStopReason"`
	}
	return json.Unmarshal(result, &state) == nil && state.AgentStatus == "incomplete" && state.AgentStopReason == "handoff_requested"
}

type nodeOutboxReplayExecutor struct {
	result json.RawMessage
}

func (e nodeOutboxReplayExecutor) ExecuteNodeTask(context.Context, storage.ExecutionTask) (json.RawMessage, error) {
	return append(json.RawMessage(nil), e.result...), nil
}

func (w *NodeWorker) recoverOutbox(ctx context.Context) error {
	if w == nil || w.outbox == nil {
		return nil
	}
	records, err := w.outbox.List()
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		task, err := w.client.taskStatus(ctx, record.TaskID)
		if err != nil {
			slog.Warn("local node outbox could not be reconciled with cloud", "task_id", record.TaskID, "error", err)
			continue
		}
		if task.Status == "reported_succeeded" {
			if !record.hasUploadedResult() || !bytes.Equal(task.Result, record.UploadedResult) {
				slog.Error("cloud task success does not match durable local outbox result", "task_id", record.TaskID)
				continue
			}
			if err := w.outbox.Remove(record.TaskID); err != nil {
				return fmt.Errorf("remove cloud-confirmed local outbox entry: %w", err)
			}
			continue
		}
		if task.Status == "needs_reconciliation" {
			if record.hasUploadedResult() {
				slog.Warn("local node outbox result was already verified in cloud storage; retaining task for operator reconciliation", "task_id", record.TaskID)
				continue
			}
			evidence, err := w.client.syncTranscriptArtifactForRecovery(ctx, record.TaskID, record.LeaseToken, record.Result)
			if err != nil {
				slog.Warn("local node recovery evidence upload failed; retaining outbox", "task_id", record.TaskID, "error", err)
				continue
			}
			if _, err := w.outbox.SaveUploadedResult(record, evidence); err != nil {
				return fmt.Errorf("persist cloud-verified recovery evidence in local outbox: %w", err)
			}
			slog.Info("local node uploaded task recovery evidence without replaying execution", "task_id", record.TaskID)
			continue
		}
		if task.Status != "running" && task.Status != "accepted" && task.Status != "leased" {
			slog.Warn("local node outbox is retained for task requiring operator reconciliation", "task_id", record.TaskID, "cloud_status", task.Status)
			continue
		}
		if task.LeaseUntil == nil || !task.LeaseUntil.After(time.Now().UTC()) {
			slog.Warn("local node outbox lease expired; retaining durable result without replay", "task_id", record.TaskID, "cloud_status", task.Status)
			continue
		}
		// Revalidate and extend the lease before replaying any saved result.
		if _, err := w.client.pulse(ctx, record.TaskID, record.LeaseToken); err != nil {
			slog.Warn("local node outbox lease could not be revalidated; retaining result", "task_id", record.TaskID, "error", err)
			continue
		}
		previousExecutor := w.executor
		w.executor = nodeOutboxReplayExecutor{result: record.Result}
		result, runErr, leaseErr := w.executeWithPulses(ctx, task, record.LeaseToken)
		w.executor = previousExecutor
		if leaseErr != nil {
			slog.Warn("local node outbox replay lost its cloud lease", "task_id", record.TaskID, "error", leaseErr)
			continue
		}
		if runErr != nil {
			slog.Warn("local node outbox artifact replay needs reconciliation", "task_id", record.TaskID, "error", runErr)
			continue
		}
		if err := w.client.report(ctx, record.TaskID, record.LeaseToken, nodeTaskReport{Status: "reported_succeeded", Result: result}); err != nil {
			slog.Warn("local node outbox result report needs reconciliation", "task_id", record.TaskID, "error", err)
			continue
		}
		if err := w.outbox.Remove(record.TaskID); err != nil {
			return fmt.Errorf("remove recovered local node outbox entry: %w", err)
		}
	}
	return nil
}

func (c *nodeAPIClient) heartbeat(ctx context.Context, platform string, capabilities []string) error {
	resources, err := hostNodeResources()
	if err != nil {
		return err
	}
	return c.heartbeatWithResources(ctx, platform, capabilities, resources)
}

func (c *nodeAPIClient) heartbeatWithResources(ctx context.Context, platform string, capabilities []string, resources storage.ExecutionNodeResources) error {
	if err := resources.Validate(); err != nil {
		return err
	}
	var readBack struct {
		Platform     string                         `json:"platform"`
		Capabilities []string                       `json:"capabilities"`
		Resources    storage.ExecutionNodeResources `json:"resources"`
	}
	if err := c.callJSON(ctx, http.MethodPost, "/api/v1/nodes/heartbeat", "", map[string]any{"platform": platform, "capabilities": capabilities, "resources": resources}, &readBack); err != nil {
		return err
	}
	if readBack.Platform != platform || !sameStringSequence(readBack.Capabilities, capabilities) || readBack.Resources != resources {
		return errors.New("node heartbeat response did not read back the reported resources")
	}
	return nil
}

func sameStringSequence(left, right []string) bool {
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

func (c *nodeAPIClient) claim(ctx context.Context) (nodeClaim, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/nodes/tasks/claim", nil)
	if err != nil {
		return nodeClaim{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)
	response, err := c.http.Do(request)
	if err != nil {
		return nodeClaim{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nodeClaim{}, domain.ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nodeClaim{}, nodeHTTPError(response)
	}
	var claim nodeClaim
	if err := decodeNodeResponse(response, &claim); err != nil {
		return nodeClaim{}, err
	}
	if claim.LeaseUntil.IsZero() || claim.Task.Status != "leased" || claim.Task.LeaseUntil == nil || !claim.Task.LeaseUntil.Equal(claim.LeaseUntil) {
		return nodeClaim{}, errors.New("node claim did not return a durable live lease")
	}
	return claim, nil
}

func (c *nodeAPIClient) pulse(ctx context.Context, taskID, lease string) (storage.ExecutionTask, error) {
	return c.pulseWithPhase(ctx, taskID, lease, "")
}

func (c *nodeAPIClient) pulseWithPhase(ctx context.Context, taskID, lease, phase string) (storage.ExecutionTask, error) {
	var task storage.ExecutionTask
	err := c.callJSON(ctx, http.MethodPost, "/api/v1/nodes/tasks/"+url.PathEscape(taskID)+"/pulse", lease, map[string]any{"progressPhase": phase}, &task)
	if err == nil && phase != "" && task.ProgressPhase != phase {
		return storage.ExecutionTask{}, errors.New("node task phase pulse did not read back the requested durable phase")
	}
	return task, err
}

func (c *nodeAPIClient) taskStatus(ctx context.Context, taskID string) (storage.ExecutionTask, error) {
	var task storage.ExecutionTask
	err := c.callJSON(ctx, http.MethodGet, "/api/v1/nodes/tasks/"+url.PathEscape(taskID)+"/status", "", nil, &task)
	if err == nil && task.ID != taskID {
		return storage.ExecutionTask{}, errors.New("node task status response returned a different task identity")
	}
	return task, err
}

func (c *nodeAPIClient) downloadTaskArtifact(ctx context.Context, taskID, lease, artifactID, expectedSHA256 string, expectedByteSize int64) (resultPath string, resultErr error) {
	if expectedByteSize < 0 || len(expectedSHA256) != 64 {
		return "", domain.ErrInvalid
	}
	basePath := "/api/v1/nodes/tasks/" + url.PathEscape(taskID) + "/artifacts/" + url.PathEscape(artifactID)
	var ticket struct {
		TaskID     string    `json:"taskId"`
		ArtifactID string    `json:"artifactId"`
		SHA256     string    `json:"sha256"`
		ByteSize   int64     `json:"byteSize"`
		Direct     bool      `json:"direct"`
		URL        string    `json:"url"`
		ExpiresAt  time.Time `json:"expiresAt"`
	}
	ticketPath := "/api/v1/nodes/tasks/" + url.PathEscape(taskID) + "/artifact-downloads/" + url.PathEscape(artifactID) + "/ticket"
	if err := c.callJSON(ctx, http.MethodGet, ticketPath, lease, nil, &ticket); err != nil {
		return "", err
	}
	if ticket.TaskID != taskID || ticket.ArtifactID != artifactID || strings.ToLower(ticket.SHA256) != strings.ToLower(expectedSHA256) || ticket.ByteSize < 0 || (expectedByteSize > 0 && ticket.ByteSize != expectedByteSize) {
		return "", errors.New("task artifact download ticket did not match its leased manifest")
	}
	var request *http.Request
	var err error
	if ticket.Direct {
		target, parseErr := url.Parse(ticket.URL)
		if parseErr != nil || !target.IsAbs() || target.Host == "" || target.User != nil || target.Fragment != "" || (target.Scheme != "https" && (target.Scheme != "http" || !isLoopbackURLHost(target.Hostname()))) || ticket.ExpiresAt.IsZero() || !time.Now().Before(ticket.ExpiresAt) || time.Until(ticket.ExpiresAt) > 6*time.Minute {
			return "", errors.New("task artifact direct download URL or expiration is invalid")
		}
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return "", sanitizePresignedObjectTransportError(err, target, "download")
		}
	} else {
		if ticket.URL != "" || !ticket.ExpiresAt.IsZero() {
			return "", errors.New("proxied task artifact ticket unexpectedly contains a direct capability")
		}
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+basePath, nil)
		if err == nil {
			request.Header.Set("Authorization", "Bearer "+c.credential)
			request.Header.Set("X-O-Task-Lease", lease)
		}
	}
	if err != nil {
		return "", err
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ticket.Direct {
			target, parseErr := url.Parse(ticket.URL)
			if parseErr == nil {
				return "", sanitizePresignedObjectTransportError(err, target, "download")
			}
		}
		return "", err
	}
	responseBodyClosed := false
	defer func() {
		if !responseBodyClosed {
			if closeErr := response.Body.Close(); closeErr != nil {
				resultPath = ""
				resultErr = errors.Join(resultErr, fmt.Errorf("close task artifact response body: %w", closeErr))
			}
		}
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if ticket.Direct {
			return "", presignedObjectHTTPError(response, "download")
		}
		return "", nodeHTTPError(response)
	}
	digest := strings.ToLower(strings.TrimSpace(ticket.SHA256))
	wantSize := ticket.ByteSize
	if !ticket.Direct {
		digest = strings.ToLower(strings.TrimSpace(response.Header.Get("X-Artifact-SHA256")))
		wantSize, err = strconv.ParseInt(response.Header.Get("X-Artifact-Byte-Size"), 10, 64)
		if err != nil {
			return "", errors.New("source conversation artifact size metadata is invalid")
		}
	}
	if len(digest) != 64 || digest != strings.ToLower(strings.TrimSpace(expectedSHA256)) {
		return "", errors.New("source conversation artifact digest header does not match the task reference")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", errors.New("source conversation artifact has an invalid digest header")
	}
	if wantSize < 0 || (expectedByteSize > 0 && wantSize != expectedByteSize) || (response.ContentLength >= 0 && response.ContentLength != wantSize) {
		return "", errors.New("source conversation artifact size metadata is invalid")
	}
	file, err := os.CreateTemp("", "o-node-conversation-context-*.json")
	if err != nil {
		return "", err
	}
	path := file.Name()
	keep := false
	defer func() {
		if !keep {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				resultPath = ""
				resultErr = errors.Join(resultErr, fmt.Errorf("remove incomplete task artifact download: %w", removeErr))
			}
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", errors.Join(err, file.Close())
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), response.Body)
	responseCloseErr := response.Body.Close()
	responseBodyClosed = true
	if copyErr != nil || responseCloseErr != nil {
		closeErr := file.Close()
		return "", errors.Join(copyErr, responseCloseErr, closeErr)
	}
	if written != wantSize || hex.EncodeToString(hasher.Sum(nil)) != digest {
		closeErr := file.Close()
		return "", errors.Join(domain.ErrConflict, closeErr)
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		return "", errors.Join(err, closeErr)
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	keep = true
	return path, nil
}

func (c *nodeAPIClient) report(ctx context.Context, taskID, lease string, report nodeTaskReport) error {
	var readBack storage.ExecutionTask
	if err := c.callJSON(ctx, http.MethodPost, "/api/v1/nodes/tasks/"+url.PathEscape(taskID)+"/report", lease, report, &readBack); err != nil {
		return err
	}
	if readBack.ID != taskID || readBack.Status != report.Status {
		return errors.New("node task report did not read back the requested durable status")
	}
	if report.Status == "reported_succeeded" && !bytes.Equal(readBack.Result, report.Result) {
		return errors.New("node task result did not read back byte-for-byte")
	}
	if report.Status == "reported_succeeded" || report.Status == "reported_failed" || report.Status == "needs_reconciliation" || report.Status == "cancelled" {
		if readBack.LeaseUntil != nil {
			return errors.New("terminal node task report retained a live lease")
		}
	}
	return nil
}

func (c *nodeAPIClient) reportFailure(ctx context.Context, taskID, lease, reason string, reconcile bool) error {
	status := "reported_failed"
	if reconcile {
		status = "needs_reconciliation"
	}
	if err := c.report(ctx, taskID, lease, nodeTaskReport{Status: status, Error: reason}); err != nil {
		return errors.Join(fmt.Errorf("persist node execution state %s: %s", status, reason), err)
	}
	return nil
}

func (c *nodeAPIClient) callJSON(ctx context.Context, method, path, lease string, input any, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.credential)
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if lease != "" {
		request.Header.Set("X-O-Task-Lease", lease)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nodeHTTPError(response)
	}
	if output != nil {
		return decodeNodeResponse(response, output)
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, maxNodeResponseBytes+1))
	return err
}

func decodeNodeResponse(response *http.Response, target any) error {
	limited := io.LimitReader(response.Body, maxNodeResponseBytes+1)
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("node control response contains trailing JSON")
		}
		return err
	}
	return nil
}

type nodeControlHTTPError struct {
	StatusCode  int
	Message     string
	BodyReadErr error
}

func (e *nodeControlHTTPError) Error() string {
	if e.BodyReadErr != nil {
		return fmt.Sprintf("node control returned HTTP %d: %s (read response body: %v)", e.StatusCode, e.Message, e.BodyReadErr)
	}
	return fmt.Sprintf("node control returned HTTP %d: %s", e.StatusCode, e.Message)
}

func (e *nodeControlHTTPError) Unwrap() error { return e.BodyReadErr }

func nodeHTTPError(response *http.Response) error {
	body, bodyReadErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	return &nodeControlHTTPError{StatusCode: response.StatusCode, Message: strings.TrimSpace(string(body)), BodyReadErr: bodyReadErr}
}

func retryableNodeArtifactSyncError(err error) bool {
	if err == nil {
		return false
	}
	var controlErr *nodeControlHTTPError
	if errors.As(err, &controlErr) {
		return controlErr.StatusCode == http.StatusRequestTimeout || controlErr.StatusCode == http.StatusTooEarly || controlErr.StatusCode == http.StatusTooManyRequests || controlErr.StatusCode >= 500
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func validNodeWorkerPlatform(platform string) bool {
	return platform == "windows" || platform == "linux" || platform == "darwin" || platform == "other"
}

func isLoopbackURLHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func normalizeCapabilities(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
