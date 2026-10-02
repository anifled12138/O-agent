package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
	"github.com/coder/websocket"
)

func TestNodeAPIClientConnectWakeAndReadEvents(t *testing.T) {
	const credential = "node-wakeup-test-credential-with-at-least-32-chars"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes/connect" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+credential {
			http.Error(w, "missing node credential", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{nodeWakeSubprotocol}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for _, reason := range []string{"connected", "task_available"} {
			payload, _ := json.Marshal(nodeWakeMessage{Type: "wake", Reason: reason})
			if err := conn.Write(r.Context(), websocket.MessageText, payload); err != nil {
				return
			}
		}
		_ = conn.Close(websocket.StatusNormalClosure, "test complete")
	}))
	defer server.Close()

	worker, err := NewNodeWorker(server.URL, credential, "linux", nil, noopNodeTaskExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := worker.client.connectWake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var wakes atomic.Int32
	err = worker.client.readWakeMessages(ctx, conn, func() { wakes.Add(1) })
	if err == nil {
		t.Fatal("readWakeMessages returned nil after the server closed the channel")
	}
	if got := wakes.Load(); got != 2 {
		t.Fatalf("wake callbacks = %d, want 2", got)
	}
}

func TestNodeWakeBroadcastReleasesEveryWaitingPoller(t *testing.T) {
	worker := &NodeWorker{wakeEvent: make(chan struct{}), pollEvery: time.Hour}
	const waiters = 4
	done := make(chan struct{}, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			worker.waitForWakeOrPoll(context.Background())
			done <- struct{}{}
		}()
	}
	// Give all goroutines a chance to observe the current wake generation.
	time.Sleep(20 * time.Millisecond)
	worker.broadcastWake()
	for i := 0; i < waiters; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("only %d pollers woke", i)
		}
	}
}

func TestVerifiedCapacityIncreaseWakesConnectedPollers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var heartbeat struct {
			Platform     string                         `json:"platform"`
			Capabilities []string                       `json:"capabilities"`
			Resources    storage.ExecutionNodeResources `json:"resources"`
		}
		if err := json.NewDecoder(r.Body).Decode(&heartbeat); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(heartbeat)
	}))
	defer server.Close()
	worker, err := NewNodeWorker(server.URL, "capacity-wakeup-test-credential-at-least-32-chars", "linux", nil, noopNodeTaskExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	worker.resources = storage.ExecutionNodeResources{MaxConcurrentTasks: 0}
	worker.wakeOnline.Store(true)
	const waiters = 3
	done := make(chan struct{}, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			worker.waitForWakeOrPoll(context.Background())
			done <- struct{}{}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	capacity := storage.ExecutionNodeResources{MemoryTotalBytes: 4 << 30, MemoryAvailableBytes: 3 << 30, LogicalCPUs: 4, MaxConcurrentTasks: 1}
	if err := worker.reportResources(context.Background(), capacity, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < waiters; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("only %d pollers woke after the verified capacity increase", i)
		}
	}
}

func TestNodeWorkerStopsAfterWakeCredentialIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes/connect":
			http.Error(w, "node credential was revoked", http.StatusUnauthorized)
		case "/api/v1/nodes/heartbeat":
			writeNodeHeartbeatReadBack(t, w, r)
		case "/api/v1/nodes/tasks/claim":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	worker, err := NewNodeWorker(server.URL, "revoked-node-credential-at-least-32-characters", "linux", nil, noopNodeTaskExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	worker.resourceReader = testNodeResourceSnapshot
	worker.pollEvery = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = worker.Run(ctx)
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("NodeWorker.Run after node credential rejection = %v, want unauthorized", err)
	}
}

type noopNodeTaskExecutor struct{}

func (noopNodeTaskExecutor) ExecuteNodeTask(context.Context, storage.ExecutionTask) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
