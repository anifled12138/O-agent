package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/storage"
	"github.com/coder/websocket"
)

func TestNodeWebSocketWakeFollowsDurableTaskAndAllowsClaim(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const nodeID = "node_websocket_test"
	const credential = "node-websocket-test-credential-with-at-least-32-chars"
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: nodeID, UserID: owner, Name: "WSS test node", Platform: "linux"}, hashToken(credential)); err != nil {
		t.Fatal(err)
	}
	resources := storage.ExecutionNodeResources{MemoryTotalBytes: 4 << 30, MemoryAvailableBytes: 3 << 30, LogicalCPUs: 4, MaxConcurrentTasks: 1}
	if _, err := store.HeartbeatExecutionNodeWithResources(ctx, hashToken(credential), "linux", []string{"agent-runtime"}, resources, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&Server{workspaceID: owner, store: store}).Handler())
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/nodes/connect"

	unauthorized, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{nodeWebSocketSubprotocol}})
	if unauthorized != nil {
		_ = unauthorized.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated node socket was not rejected: conn=%v response=%v err=%v", unauthorized, response, err)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}

	header := make(http.Header)
	header.Set("Authorization", "Bearer "+credential)
	conn, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{nodeWebSocketSubprotocol}})
	if err != nil {
		t.Fatalf("authenticated node websocket dial failed (response=%v): %v", response, err)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != nodeWebSocketSubprotocol {
		t.Fatalf("negotiated subprotocol = %q", conn.Subprotocol())
	}
	readWake := func(wantReason string) {
		t.Helper()
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		messageType, payload, err := conn.Read(readCtx)
		if err != nil {
			t.Fatalf("read %q wake event: %v", wantReason, err)
		}
		if messageType != websocket.MessageText {
			t.Fatalf("wake event message type = %v", messageType)
		}
		var event nodeWakeEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "wake" || event.Reason != wantReason {
			t.Fatalf("wake event = %+v, want reason %q", event, wantReason)
		}
	}
	readWake("connected")

	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/tasks", strings.NewReader(`{"targetNodeId":"node_websocket_test","payload":{"kind":"probe"},"waitForNode":false}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "node-websocket-wake-task")
	responseHTTP, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if responseHTTP.StatusCode != http.StatusAccepted {
		_ = responseHTTP.Body.Close()
		t.Fatalf("task submission status = %d", responseHTTP.StatusCode)
	}
	var submitted struct {
		Task storage.ExecutionTask `json:"task"`
	}
	if err := json.NewDecoder(responseHTTP.Body).Decode(&submitted); err != nil {
		_ = responseHTTP.Body.Close()
		t.Fatal(err)
	}
	_ = responseHTTP.Body.Close()
	if submitted.Task.Status != "queued" || submitted.Task.NodeID != nodeID {
		t.Fatalf("submitted task was not queued for the node: %+v", submitted.Task)
	}
	readWake("task_available")

	claimRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/nodes/tasks/claim", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	claimRequest.Header.Set("Authorization", "Bearer "+credential)
	claimRequest.Header.Set("Content-Type", "application/json")
	claimResponse, err := http.DefaultClient.Do(claimRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer claimResponse.Body.Close()
	if claimResponse.StatusCode != http.StatusOK {
		t.Fatalf("task claim status = %d", claimResponse.StatusCode)
	}
	var claim struct {
		Task storage.ExecutionTask `json:"task"`
	}
	if err := json.NewDecoder(claimResponse.Body).Decode(&claim); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, submitted.Task.ID)
	if err != nil || claim.Task.ID != submitted.Task.ID || readBack.Status != "leased" || readBack.NodeID != nodeID {
		t.Fatalf("wake did not lead to durable claim: claim=%+v stored=%+v err=%v", claim.Task, readBack, err)
	}
}

func TestNodeWebSocketReconnectWakesTaskQueuedWhileOffline(t *testing.T) {
	ctx := context.Background()
	storeDir := t.TempDir()
	store, err := storage.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const nodeID = "node_websocket_offline"
	const credential = "node-websocket-offline-credential-with-at-least-32-chars"
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: nodeID, UserID: owner, Name: "Offline WSS test node", Platform: "linux"}, hashToken(credential)); err != nil {
		t.Fatal(err)
	}
	resources := storage.ExecutionNodeResources{MemoryTotalBytes: 4 << 30, MemoryAvailableBytes: 3 << 30, LogicalCPUs: 4, MaxConcurrentTasks: 1}
	if _, err := store.HeartbeatExecutionNodeWithResources(ctx, hashToken(credential), "linux", []string{"agent-runtime"}, resources, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&Server{workspaceID: owner, store: store}).Handler())
	defer func() { server.Close() }()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/tasks", strings.NewReader(`{"targetNodeId":"node_websocket_offline","payload":{"kind":"probe"},"waitForNode":false}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "node-websocket-offline-task")
	submittedResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if submittedResponse.StatusCode != http.StatusAccepted {
		_ = submittedResponse.Body.Close()
		t.Fatalf("offline task submission status = %d", submittedResponse.StatusCode)
	}
	var submitted struct {
		Task storage.ExecutionTask `json:"task"`
	}
	decodeErr := json.NewDecoder(submittedResponse.Body).Decode(&submitted)
	closeErr := submittedResponse.Body.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if submitted.Task.Status != "queued" || submitted.Task.NodeID != nodeID {
		t.Fatalf("offline task was not durably queued for its node: %+v", submitted.Task)
	}
	server.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(storeDir)
	if err != nil {
		t.Fatalf("reopen task database after control-plane restart: %v", err)
	}
	server = httptest.NewServer((&Server{workspaceID: owner, store: store}).Handler())

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/nodes/connect"
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+credential)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{nodeWebSocketSubprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	messageType, payload, err := conn.Read(readCtx)
	if err != nil {
		t.Fatal(err)
	}
	var event nodeWakeEvent
	if messageType != websocket.MessageText || json.Unmarshal(payload, &event) != nil || event.Type != "wake" || event.Reason != "connected" {
		t.Fatalf("reconnecting node did not receive its initial wake: type=%v event=%+v payload=%s", messageType, event, payload)
	}

	claimRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/nodes/tasks/claim", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	claimRequest.Header.Set("Authorization", "Bearer "+credential)
	claimRequest.Header.Set("Content-Type", "application/json")
	claimResponse, err := http.DefaultClient.Do(claimRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer claimResponse.Body.Close()
	if claimResponse.StatusCode != http.StatusOK {
		t.Fatalf("reconnected node claim status = %d", claimResponse.StatusCode)
	}
	var claim struct {
		Task storage.ExecutionTask `json:"task"`
	}
	if err := json.NewDecoder(claimResponse.Body).Decode(&claim); err != nil {
		t.Fatal(err)
	}
	readBack, err := store.ExecutionTask(ctx, owner, submitted.Task.ID)
	if err != nil || claim.Task.ID != submitted.Task.ID || readBack.Status != "leased" {
		t.Fatalf("reconnected node failed to claim its durable queued task: claim=%+v stored=%+v err=%v", claim.Task, readBack, err)
	}
}

func TestNodeRevocationClosesItsActiveWebSocket(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const nodeID = "node_websocket_revocation"
	const credential = "node-websocket-revocation-credential-at-least-32-chars"
	const sessionToken = "owner-session-for-websocket-revocation-test"
	if _, err := store.RegisterExecutionNode(ctx, storage.ExecutionNode{ID: nodeID, UserID: owner, Name: "Revocation test node", Platform: "linux"}, hashToken(credential)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAuthSession(ctx, hashToken(sessionToken), owner, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	serverHandler := &Server{workspaceID: owner, store: store}
	serverHandler.SetRemoteAuthentication("test-bootstrap-token-long-enough-32-chars")
	server := httptest.NewServer(serverHandler.Handler())
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/nodes/connect"
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+credential)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header, Subprotocols: []string{nodeWebSocketSubprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read initial node wake before revocation: %v", err)
	}

	request, err := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/nodes/"+nodeID, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: authCookieName, Value: sessionToken})
	revokedResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if revokedResponse.StatusCode != http.StatusOK {
		_ = revokedResponse.Body.Close()
		t.Fatalf("node revocation status = %d", revokedResponse.StatusCode)
	}
	_ = revokedResponse.Body.Close()
	readBack, err := store.ExecutionNode(ctx, owner, nodeID)
	if err != nil || readBack.RevokedAt == nil {
		t.Fatalf("node revocation did not persist before closing its socket: node=%+v err=%v", readBack, err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _, err = conn.Read(readCtx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("revoked node websocket close status = %v, err=%v", websocket.CloseStatus(err), err)
	}
}
