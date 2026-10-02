package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
	"github.com/coder/websocket"
)

func TestNodeArtifactDirectDownloadDoesNotForwardLeaseAndVerifiesContents(t *testing.T) {
	content := []byte("cloud object data delivered directly to the leased node")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	var corruptBody atomic.Bool
	var rejectBody atomic.Bool
	dataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("X-O-Task-Lease") != "" {
			t.Errorf("direct object request forwarded control credentials: method=%s headers=%v", r.Method, r.Header)
		}
		if rejectBody.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream failed at " + r.URL.String()))
			return
		}
		body := content
		if corruptBody.Load() {
			body = append([]byte(nil), content...)
			body[0] ^= 0xff
		}
		_, _ = w.Write(body)
	}))
	defer dataServer.Close()
	controlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes/tasks/task-direct/artifact-downloads/artifact-direct/ticket" || r.Header.Get("Authorization") != "Bearer node-credential" || r.Header.Get("X-O-Task-Lease") != "task-lease" {
			t.Errorf("download ticket request was not bound to node credential and lease: path=%s headers=%v", r.URL.Path, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"taskId": "task-direct", "artifactId": "artifact-direct", "sha256": digestHex,
			"byteSize": len(content), "direct": true, "url": dataServer.URL + "?X-Amz-Signature=opaque-download-signature",
			"expiresAt": time.Now().UTC().Add(2 * time.Minute),
		})
	}))
	defer controlServer.Close()
	client := &nodeAPIClient{baseURL: controlServer.URL, credential: "node-credential", http: controlServer.Client()}
	path, err := client.downloadTaskArtifact(context.Background(), "task-direct", "task-lease", "artifact-direct", digestHex, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	readBack, err := os.ReadFile(path)
	if err != nil || string(readBack) != string(content) {
		t.Fatalf("direct node download was not durably materialized after hash verification: content=%q err=%v", readBack, err)
	}
	corruptBody.Store(true)
	if corruptPath, err := client.downloadTaskArtifact(context.Background(), "task-direct", "task-lease", "artifact-direct", digestHex, int64(len(content))); err == nil || corruptPath != "" {
		t.Fatalf("node accepted content with the advertised byte size but the wrong SHA-256: path=%q err=%v", corruptPath, err)
	}
	rejectBody.Store(true)
	if rejectedPath, err := client.downloadTaskArtifact(context.Background(), "task-direct", "task-lease", "artifact-direct", digestHex, int64(len(content))); err == nil || rejectedPath != "" || strings.Contains(err.Error(), "opaque-download-signature") || strings.Contains(err.Error(), "X-Amz-Signature") {
		t.Fatalf("direct-download provider failure leaked the signed URL or was reported as success: path=%q err=%v", rejectedPath, err)
	}
}

type nodeWorkerExecutorFunc func(context.Context, storage.ExecutionTask) (json.RawMessage, error)

func (fn nodeWorkerExecutorFunc) ExecuteNodeTask(ctx context.Context, task storage.ExecutionTask) (json.RawMessage, error) {
	return fn(ctx, task)
}

func writeNodeHeartbeatReadBack(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var body struct {
		Platform     string                         `json:"platform"`
		Capabilities []string                       `json:"capabilities"`
		Resources    storage.ExecutionNodeResources `json:"resources"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode heartbeat: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeTestJSON(w, http.StatusOK, map[string]any{"platform": body.Platform, "capabilities": body.Capabilities, "resources": body.Resources})
}

func testNodeResourceSnapshot() (storage.ExecutionNodeResources, error) {
	return storage.ExecutionNodeResources{MemoryTotalBytes: 8 << 30, MemoryAvailableBytes: 6 << 30, LogicalCPUs: 4, MaxConcurrentTasks: 2}, nil
}

func TestNodeWorkerRunsLeasedTaskAndReadsBackEveryDurableTransition(t *testing.T) {
	var mu sync.Mutex
	var reports []nodeTaskReport
	var heartbeatCount, pulseCount int
	var pulseLease string
	const credential = "paired-node-secret-0123456789abc"
	const lease = "task-lease-secret"
	task := storage.ExecutionTask{ID: "task_local_1", NodeID: "node_local_1", Status: "leased", Payload: json.RawMessage(`{"kind":"agent_prompt","content":"hello"}`), LeaseUntil: ptrTime(time.Now().UTC().Add(time.Minute))}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+credential {
			t.Errorf("authorization header = %q", got)
		}
		switch {
		case r.URL.Path == "/api/v1/nodes/heartbeat" && r.Method == http.MethodPost:
			var body struct {
				Platform     string                         `json:"platform"`
				Capabilities []string                       `json:"capabilities"`
				Resources    storage.ExecutionNodeResources `json:"resources"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Platform != "linux" || !reflect.DeepEqual(body.Capabilities, []string{"agent-runtime", "sandbox"}) || body.Resources.MaxConcurrentTasks < 0 {
				t.Errorf("heartbeat body=%+v err=%v", body, err)
			}
			mu.Lock()
			heartbeatCount++
			mu.Unlock()
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "node_local_1", "platform": body.Platform, "capabilities": body.Capabilities, "resources": body.Resources})
		case r.URL.Path == "/api/v1/nodes/tasks/claim" && r.Method == http.MethodPost:
			writeTestJSON(w, http.StatusOK, nodeClaim{Task: task, LeaseToken: lease, LeaseUntil: *task.LeaseUntil})
		case r.URL.Path == "/api/v1/nodes/tasks/task_local_1/pulse" && r.Method == http.MethodPost:
			if r.Header.Get("X-O-Task-Lease") != lease {
				t.Errorf("pulse lease=%q", r.Header.Get("X-O-Task-Lease"))
			}
			var pulse struct {
				ProgressPhase string `json:"progressPhase"`
			}
			if err := json.NewDecoder(r.Body).Decode(&pulse); err != nil {
				t.Errorf("decode pulse: %v", err)
			}
			mu.Lock()
			pulseCount++
			pulseLease = r.Header.Get("X-O-Task-Lease")
			mu.Unlock()
			next := task
			next.Status = "running"
			next.CancelRequested = false
			next.LeaseUntil = ptrTime(time.Now().UTC().Add(time.Minute))
			next.ProgressPhase = pulse.ProgressPhase
			writeTestJSON(w, http.StatusOK, next)
		case r.URL.Path == "/api/v1/nodes/tasks/task_local_1/report" && r.Method == http.MethodPost:
			if r.Header.Get("X-O-Task-Lease") != lease {
				t.Errorf("report lease=%q", r.Header.Get("X-O-Task-Lease"))
			}
			var report nodeTaskReport
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Errorf("decode report: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			reports = append(reports, report)
			mu.Unlock()
			next := task
			next.Status = report.Status
			next.Result = report.Result
			if report.Status == "reported_succeeded" || report.Status == "reported_failed" || report.Status == "needs_reconciliation" || report.Status == "cancelled" {
				next.LeaseUntil = nil
			}
			writeTestJSON(w, http.StatusOK, next)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executorEntered := make(chan struct{})
	allowFinish := make(chan struct{})
	worker, err := NewNodeWorker(server.URL, credential, "linux", []string{"sandbox", "agent-runtime", "sandbox"}, nodeWorkerExecutorFunc(func(ctx context.Context, claimed storage.ExecutionTask) (json.RawMessage, error) {
		if claimed.ID != task.ID || string(claimed.Payload) != string(task.Payload) {
			return nil, domain.ErrConflict
		}
		close(executorEntered)
		select {
		case <-allowFinish:
			return json.RawMessage(`{"assistantMessage":"done"}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	worker.resourceReader = testNodeResourceSnapshot
	worker.pulseEvery = time.Millisecond
	worker.pollEvery = time.Millisecond
	if err := worker.client.heartbeat(context.Background(), worker.platform, worker.capabilities); err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- worker.RunOne(context.Background()) }()
	select {
	case <-executorEntered:
	case <-time.After(time.Second):
		t.Fatal("node executor did not run")
	}
	// The executor callback is made deterministic by releasing it from a timer
	// after the first lease pulse has reached the control server.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		pulses := pulseCount
		mu.Unlock()
		if pulses > 0 {
			close(allowFinish)
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("node task worker did not finish after executor release")
	}
	mu.Lock()
	defer mu.Unlock()
	wantStatuses := []string{"accepted", "running", "reported_succeeded"}
	gotStatuses := make([]string, 0, len(reports))
	for _, report := range reports {
		gotStatuses = append(gotStatuses, report.Status)
	}
	if !reflect.DeepEqual(gotStatuses, wantStatuses) || string(reports[len(reports)-1].Result) != `{"assistantMessage":"done"}` {
		t.Fatalf("reports=%+v, want statuses %v and durable result", reports, wantStatuses)
	}
	if heartbeatCount != 2 || pulseCount == 0 || pulseLease != lease {
		t.Fatalf("heartbeat=%d pulse=%d pulseLease=%q", heartbeatCount, pulseCount, pulseLease)
	}
}

func TestNodeWorkerPersistsUncertainOutcomesAndRejectsInvalidResults(t *testing.T) {
	for _, test := range []struct {
		name     string
		result   json.RawMessage
		err      error
		wantCode string
	}{
		{name: "uncertain", err: &NodeTaskOutcomeError{Status: "needs_reconciliation", Err: errors.New("remote write may have happened")}, wantCode: "needs_reconciliation"},
		{name: "ordinary failure", err: errors.New("local command failed"), wantCode: "reported_failed"},
		{name: "empty success", wantCode: "reported_failed"},
		{name: "invalid json", result: json.RawMessage("not-json"), wantCode: "reported_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reports []nodeTaskReport
			const lease = "lease"
			claimed := storage.ExecutionTask{ID: "task", NodeID: "node", Status: "leased", LeaseUntil: ptrTime(time.Now().UTC().Add(time.Minute)), Payload: json.RawMessage(`{}`)}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/nodes/tasks/claim":
					writeTestJSON(w, http.StatusOK, nodeClaim{Task: claimed, LeaseToken: lease, LeaseUntil: *claimed.LeaseUntil})
				case strings.HasSuffix(r.URL.Path, "/pulse"):
					var pulse struct {
						ProgressPhase string `json:"progressPhase"`
					}
					_ = json.NewDecoder(r.Body).Decode(&pulse)
					claimed.Status = "running"
					claimed.ProgressPhase = pulse.ProgressPhase
					leaseUntil := time.Now().UTC().Add(time.Minute)
					claimed.LeaseUntil = &leaseUntil
					writeTestJSON(w, http.StatusOK, claimed)
				case strings.HasSuffix(r.URL.Path, "/report"):
					var report nodeTaskReport
					_ = json.NewDecoder(r.Body).Decode(&report)
					reports = append(reports, report)
					claimed.Status = report.Status
					if report.Status == "reported_succeeded" || report.Status == "reported_failed" || report.Status == "needs_reconciliation" || report.Status == "cancelled" {
						claimed.LeaseUntil = nil
					}
					writeTestJSON(w, http.StatusOK, claimed)
				case r.URL.Path == "/api/v1/nodes/heartbeat":
					writeNodeHeartbeatReadBack(t, w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			worker, err := NewNodeWorker(server.URL, "paired-node-secret-0123456789abc", "windows", nil, nodeWorkerExecutorFunc(func(context.Context, storage.ExecutionTask) (json.RawMessage, error) {
				return test.result, test.err
			}))
			if err != nil {
				t.Fatal(err)
			}
			worker.resourceReader = testNodeResourceSnapshot
			if err := worker.RunOne(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(reports) == 0 || reports[len(reports)-1].Status != test.wantCode {
				t.Fatalf("reports=%+v want terminal state %q", reports, test.wantCode)
			}
			if test.wantCode == "needs_reconciliation" && reports[len(reports)-1].Error != "remote write may have happened" {
				t.Fatalf("uncertain result reason was not persisted: %+v", reports[len(reports)-1])
			}
		})
	}
}

func TestNewNodeWorkerRequiresTLSOutsideLoopbackAndDisablesRedirects(t *testing.T) {
	const credential = "0123456789abcdef0123456789abcdef"
	noop := nodeWorkerExecutorFunc(func(context.Context, storage.ExecutionTask) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})
	for _, insecure := range []string{"http://example.com", "http://192.0.2.10:9171"} {
		if _, err := NewNodeWorker(insecure, credential, "linux", nil, noop); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("NewNodeWorker(%q) err=%v, want invalid", insecure, err)
		}
	}
	if _, err := NewNodeWorker("https://user:pass@example.com", credential, "linux", nil, noop); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("control URL credentials were accepted: %v", err)
	}
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("node credential followed an HTTP redirect") }))
	defer redirectTarget.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	worker, err := NewNodeWorker(redirector.URL, credential, "linux", nil, noop)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.client.heartbeat(context.Background(), "linux", nil); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("node client did not stop at redirect response: %v", err)
	}
}

func TestNodeArtifactRetryPolicyRetriesOnlyTransientFailures(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
		if !retryableNodeArtifactSyncError(&nodeControlHTTPError{StatusCode: status}) {
			t.Errorf("HTTP %d should retry artifact synchronization", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict} {
		if retryableNodeArtifactSyncError(&nodeControlHTTPError{StatusCode: status}) {
			t.Errorf("HTTP %d should not retry artifact synchronization", status)
		}
	}
	if !retryableNodeArtifactSyncError(&url.Error{Op: "PUT", URL: "https://control.example/chunk", Err: errors.New("temporary network failure")}) {
		t.Fatal("transport errors should retry artifact synchronization")
	}
	if retryableNodeArtifactSyncError(domain.ErrConflict) {
		t.Fatal("local content conflicts should not retry artifact synchronization")
	}
}

func TestUnverifiedLocalArtifactUploadRequiresReconciliation(t *testing.T) {
	invalidEnvelope, err := json.Marshal(localTranscriptEnvelope{Transcript: json.RawMessage(`{"conversation":{"messages":[]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&nodeAPIClient{}).syncTranscriptArtifactWithRetry(context.Background(), "task", "lease", invalidEnvelope)
	var outcome *NodeTaskOutcomeError
	if !errors.As(err, &outcome) || outcome.Status != "needs_reconciliation" {
		t.Fatalf("unverified local artifact upload error = %v, want needs_reconciliation", err)
	}
}

func TestNodeWorkerConcurrencyUsesAHostAdaptiveCeiling(t *testing.T) {
	worker, err := NewNodeWorker("https://o.example.test", "0123456789abcdef0123456789abcdef", "linux", nil, nodeWorkerExecutorFunc(func(context.Context, storage.ExecutionTask) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if worker.concurrency != domain.MaxLocalNodeWorkerConcurrency {
		t.Fatalf("default local worker ceiling = %d, want %d", worker.concurrency, domain.MaxLocalNodeWorkerConcurrency)
	}
	if err := worker.SetConcurrency(2); err != nil || worker.concurrency != 2 {
		t.Fatalf("SetConcurrency(2) concurrency=%d err=%v", worker.concurrency, err)
	}
	if err := worker.SetConcurrency(64); err != nil || worker.concurrency != 64 {
		t.Fatalf("SetConcurrency(64) concurrency=%d err=%v", worker.concurrency, err)
	}
	for _, limit := range []int{0, 65} {
		if err := worker.SetConcurrency(limit); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("SetConcurrency(%d) err=%v, want invalid", limit, err)
		}
	}
}

func TestNodeWorkerPollerCountUsesConfiguredAndCPUCeilings(t *testing.T) {
	for _, test := range []struct {
		configured int
		cpus       int
		want       int
	}{
		{configured: 64, cpus: 4, want: 4},
		{configured: 2, cpus: 8, want: 2},
		{configured: 64, cpus: 0, want: 64},
		{configured: 0, cpus: 4, want: 1},
	} {
		if got := nodeWorkerPollerCount(test.configured, test.cpus); got != test.want {
			t.Errorf("nodeWorkerPollerCount(%d, %d) = %d, want %d", test.configured, test.cpus, got, test.want)
		}
	}
}

func TestNodeWorkerRunsConfiguredTasksConcurrently(t *testing.T) {
	const credential = "0123456789abcdef0123456789abcdef"
	var claimCount atomic.Int32
	var active, maximum atomic.Int32
	var terminalReports atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/nodes/connect":
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{nodeWakeSubprotocol}})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			payload, _ := json.Marshal(nodeWakeMessage{Type: "wake", Reason: "connected"})
			if err := conn.Write(r.Context(), websocket.MessageText, payload); err != nil {
				return
			}
			_, _, _ = conn.Read(r.Context())
		case r.URL.Path == "/api/v1/nodes/heartbeat":
			writeNodeHeartbeatReadBack(t, w, r)
		case r.URL.Path == "/api/v1/nodes/tasks/claim":
			index := claimCount.Add(1)
			if index > 2 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			taskID := fmt.Sprintf("task_%d", index)
			lease := fmt.Sprintf("lease_%d", index)
			writeTestJSON(w, http.StatusOK, nodeClaim{Task: storage.ExecutionTask{ID: taskID, NodeID: "node", Status: "leased", Payload: json.RawMessage(`{}`), LeaseUntil: ptrTime(time.Now().UTC().Add(time.Minute))}, LeaseToken: lease, LeaseUntil: time.Now().UTC().Add(time.Minute)})
		case strings.HasSuffix(r.URL.Path, "/pulse"):
			id := strings.Split(r.URL.Path, "/")[5]
			var pulse struct {
				ProgressPhase string `json:"progressPhase"`
			}
			_ = json.NewDecoder(r.Body).Decode(&pulse)
			writeTestJSON(w, http.StatusOK, storage.ExecutionTask{ID: id, NodeID: "node", Status: "running", ProgressPhase: pulse.ProgressPhase, LeaseUntil: ptrTime(time.Now().UTC().Add(time.Minute))})
		case strings.HasSuffix(r.URL.Path, "/report"):
			id := strings.Split(r.URL.Path, "/")[5]
			var report nodeTaskReport
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Errorf("decode report: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if report.Status == "reported_succeeded" {
				terminalReports.Add(1)
			}
			writeTestJSON(w, http.StatusOK, storage.ExecutionTask{ID: id, NodeID: "node", Status: report.Status, Result: report.Result})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	worker, err := NewNodeWorker(server.URL, credential, "linux", nil, nodeWorkerExecutorFunc(func(ctx context.Context, _ storage.ExecutionTask) (json.RawMessage, error) {
		now := active.Add(1)
		for previous := maximum.Load(); now > previous && !maximum.CompareAndSwap(previous, now); previous = maximum.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			active.Add(-1)
			return nil, ctx.Err()
		}
		active.Add(-1)
		return json.RawMessage(`{"done":true}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	worker.resourceReader = testNodeResourceSnapshot
	worker.pollEvery = time.Millisecond
	worker.pulseEvery = time.Millisecond
	if err := worker.SetConcurrency(2); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- worker.Run(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("the second task did not start before the first task finished")
		}
	}
	releaseOnce.Do(func() { close(release) })
	deadline := time.Now().Add(2 * time.Second)
	for terminalReports.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("NodeWorker.Run error=%v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("node worker did not stop after cancellation")
	}
	if got := terminalReports.Load(); got != 2 {
		t.Fatalf("terminal task reports=%d, want 2", got)
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrent executions=%d, want 2", got)
	}
}

func TestNodeWorkerKeepsExecutingWhenWebSocketWakeIsUnavailable(t *testing.T) {
	const credential = "0123456789abcdef0123456789abcdef"
	var wakeAttempts atomic.Int32
	var claimCount atomic.Int32
	var terminalReports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/nodes/connect":
			wakeAttempts.Add(1)
			http.Error(w, "websocket upgrade is unavailable", http.StatusServiceUnavailable)
		case r.URL.Path == "/api/v1/nodes/heartbeat":
			writeNodeHeartbeatReadBack(t, w, r)
		case r.URL.Path == "/api/v1/nodes/tasks/claim":
			if claimCount.Add(1) != 1 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			leaseUntil := time.Now().UTC().Add(time.Minute)
			writeTestJSON(w, http.StatusOK, nodeClaim{Task: storage.ExecutionTask{ID: "task_poll_fallback", NodeID: "node", Status: "leased", Payload: json.RawMessage(`{"kind":"probe"}`), LeaseUntil: &leaseUntil}, LeaseToken: "fallback-lease", LeaseUntil: leaseUntil})
		case strings.HasSuffix(r.URL.Path, "/pulse"):
			var pulse struct {
				ProgressPhase string `json:"progressPhase"`
			}
			_ = json.NewDecoder(r.Body).Decode(&pulse)
			leaseUntil := time.Now().UTC().Add(time.Minute)
			writeTestJSON(w, http.StatusOK, storage.ExecutionTask{ID: "task_poll_fallback", NodeID: "node", Status: "running", ProgressPhase: pulse.ProgressPhase, LeaseUntil: &leaseUntil})
		case strings.HasSuffix(r.URL.Path, "/report"):
			var report nodeTaskReport
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Errorf("decode fallback task report: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			task := storage.ExecutionTask{ID: "task_poll_fallback", NodeID: "node", Status: report.Status, Result: report.Result}
			if report.Status == "accepted" || report.Status == "running" {
				leaseUntil := time.Now().UTC().Add(time.Minute)
				task.LeaseUntil = &leaseUntil
			} else if report.Status == "reported_succeeded" {
				terminalReports.Add(1)
			} else {
				t.Errorf("unexpected fallback task report status = %q", report.Status)
				w.WriteHeader(http.StatusConflict)
				return
			}
			writeTestJSON(w, http.StatusOK, task)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	worker, err := NewNodeWorker(server.URL, credential, "linux", []string{"agent-runtime"}, nodeWorkerExecutorFunc(func(context.Context, storage.ExecutionTask) (json.RawMessage, error) {
		return json.RawMessage(`{"ran":true}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	worker.resourceReader = testNodeResourceSnapshot
	worker.pollEvery = 5 * time.Millisecond
	worker.heartbeatEvery = time.Hour
	worker.pulseEvery = time.Hour
	if err := worker.SetConcurrency(1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- worker.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for terminalReports.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if terminalReports.Load() != 1 || claimCount.Load() == 0 || wakeAttempts.Load() == 0 {
		cancel()
		t.Fatalf("HTTP polling fallback did not complete the task: wakeAttempts=%d claims=%d reports=%d", wakeAttempts.Load(), claimCount.Load(), terminalReports.Load())
	}
	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("NodeWorker.Run error=%v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("node worker did not stop after cancellation")
	}
}

func writeTestJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(fmt.Sprintf("encode test response: %v", err))
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
