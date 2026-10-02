package httpapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestRemoteAuthenticationSetupLoginAndProtectedReadback(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ownerID, err := store.EnsureLocalWorkspaceOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: ownerID, store: store, frontendOrigin: "https://o.example"}
	server.SetRemoteAuthentication("01234567890123456789012345678901")
	handler := server.Handler()

	protected := httptest.NewRecorder()
	handler.ServeHTTP(protected, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	if protected.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated protected request = %d, want 401", protected.Code)
	}
	storageCapacityUnauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(storageCapacityUnauthenticated, httptest.NewRequest(http.MethodGet, "/api/v1/system/artifact-storage", nil))
	if storageCapacityUnauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated artifact capacity request = %d, want 401", storageCapacityUnauthenticated.Code)
	}
	turnReconciliationUnauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(turnReconciliationUnauthenticated, httptest.NewRequest(http.MethodGet, "/api/v2/agent/turns/turn_private/reconciliations", nil))
	if turnReconciliationUnauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated turn reconciliation history request = %d, want 401", turnReconciliationUnauthenticated.Code)
	}
	handoffPreviewUnauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(handoffPreviewUnauthenticated, httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task_private/continuation-preview", nil))
	if handoffPreviewUnauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated task handoff preview = %d, want 401", handoffPreviewUnauthenticated.Code)
	}
	handoffUnauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(handoffUnauthenticated, httptest.NewRequest(http.MethodPost, "/api/v1/tasks/task_private/continue-in-cloud", bytes.NewReader([]byte(`{"acknowledgeExternalEffects":true}`))))
	if handoffUnauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated task continuation = %d, want 401", handoffUnauthenticated.Code)
	}

	setupBody, _ := json.Marshal(map[string]string{"email": "owner@example.com", "displayName": "Owner", "password": "long-secure-password"})
	wrongTokenRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", bytes.NewReader(setupBody))
	wrongTokenRequest.Header.Set("X-O-Bootstrap-Token", "not-the-bootstrap-token")
	wrongTokenResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongTokenResponse, wrongTokenRequest)
	if wrongTokenResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid bootstrap token returned %d, want 401", wrongTokenResponse.Code)
	}
	setupRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", bytes.NewReader(setupBody))
	setupRequest.Header.Set("X-O-Bootstrap-Token", "01234567890123456789012345678901")
	setupResponse := httptest.NewRecorder()
	handler.ServeHTTP(setupResponse, setupRequest)
	if setupResponse.Code != http.StatusOK {
		t.Fatalf("setup returned %d: %s", setupResponse.Code, setupResponse.Body.String())
	}
	setupCookie := setupResponse.Result().Cookies()[0]

	protected = httptest.NewRecorder()
	protectedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	protectedRequest.AddCookie(setupCookie)
	handler.ServeHTTP(protected, protectedRequest)
	if protected.Code != http.StatusOK {
		t.Fatalf("authenticated project read = %d: %s", protected.Code, protected.Body.String())
	}
	var projects []domain.Project
	if err := json.Unmarshal(protected.Body.Bytes(), &projects); err != nil || len(projects) != 0 {
		t.Fatalf("protected read did not return authoritative project list: %+v err=%v", projects, err)
	}
	crossOrigin := httptest.NewRecorder()
	crossOriginRequest := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	crossOriginRequest.Header.Set("Origin", "https://attacker.example")
	crossOriginRequest.AddCookie(setupCookie)
	handler.ServeHTTP(crossOrigin, crossOriginRequest)
	if crossOrigin.Code != http.StatusForbidden {
		t.Fatalf("cross-origin cookie request = %d, want 403", crossOrigin.Code)
	}

	nodeBody, _ := json.Marshal(map[string]any{"name": "Laptop", "platform": "windows", "capabilities": []string{"browser", "shell"}})
	pairRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes", bytes.NewReader(nodeBody))
	pairRequest.AddCookie(setupCookie)
	pairResponse := httptest.NewRecorder()
	handler.ServeHTTP(pairResponse, pairRequest)
	if pairResponse.Code != http.StatusCreated {
		t.Fatalf("node pair returned %d: %s", pairResponse.Code, pairResponse.Body.String())
	}
	var paired struct {
		Node       executionNodeResponse `json:"node"`
		Credential string                `json:"credential"`
	}
	if err := json.Unmarshal(pairResponse.Body.Bytes(), &paired); err != nil || paired.Node.ID == "" || paired.Credential == "" {
		t.Fatalf("pair response lacks node identity or one-time credential: %+v err=%v", paired, err)
	}
	nodeListRequest := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	nodeListRequest.AddCookie(setupCookie)
	nodeListResponse := httptest.NewRecorder()
	handler.ServeHTTP(nodeListResponse, nodeListRequest)
	if nodeListResponse.Code != http.StatusOK || bytes.Contains(nodeListResponse.Body.Bytes(), []byte(paired.Credential)) {
		t.Fatalf("node list did not safely read back without re-exposing credential: %d %s", nodeListResponse.Code, nodeListResponse.Body.String())
	}
	heartbeatResources := storage.ExecutionNodeResources{MemoryTotalBytes: 16 << 30, MemoryAvailableBytes: 12 << 30, LogicalCPUs: 8, MaxConcurrentTasks: 2}
	heartbeatBody, _ := json.Marshal(map[string]any{"platform": "windows", "capabilities": []string{"browser", "shell"}, "resources": heartbeatResources})
	heartbeatRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatRequest.Header.Set("Authorization", "Bearer "+paired.Credential)
	heartbeatResponse := httptest.NewRecorder()
	handler.ServeHTTP(heartbeatResponse, heartbeatRequest)
	if heartbeatResponse.Code != http.StatusOK {
		t.Fatalf("node heartbeat returned %d: %s", heartbeatResponse.Code, heartbeatResponse.Body.String())
	}
	var heartbeat executionNodeResponse
	if err := json.Unmarshal(heartbeatResponse.Body.Bytes(), &heartbeat); err != nil || heartbeat.ID != paired.Node.ID || heartbeat.Connectivity != "connected" || heartbeat.Resources != heartbeatResources {
		t.Fatalf("heartbeat response does not reflect connected node: %+v err=%v", heartbeat, err)
	}
	taskBody, _ := json.Marshal(map[string]any{"targetNodeId": paired.Node.ID, "payload": map[string]any{"kind": "agent_turn", "prompt": "inspect project"}})
	taskRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(taskBody))
	taskRequest.Header.Set("Idempotency-Key", "http-task-1")
	taskRequest.AddCookie(setupCookie)
	taskResponse := httptest.NewRecorder()
	handler.ServeHTTP(taskResponse, taskRequest)
	if taskResponse.Code != http.StatusAccepted {
		t.Fatalf("task submission returned %d: %s", taskResponse.Code, taskResponse.Body.String())
	}
	var submitted struct {
		Task    storage.ExecutionTask `json:"task"`
		Created bool                  `json:"created"`
	}
	if err := json.Unmarshal(taskResponse.Body.Bytes(), &submitted); err != nil || !submitted.Created || submitted.Task.Status != "queued" {
		t.Fatalf("task submission read-back mismatch: %+v err=%v", submitted, err)
	}
	taskRetry := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(taskBody))
	taskRetry.Header.Set("Idempotency-Key", "http-task-1")
	taskRetry.AddCookie(setupCookie)
	taskRetryResponse := httptest.NewRecorder()
	handler.ServeHTTP(taskRetryResponse, taskRetry)
	var retried struct {
		Task    storage.ExecutionTask `json:"task"`
		Created bool                  `json:"created"`
	}
	if taskRetryResponse.Code != http.StatusOK || json.Unmarshal(taskRetryResponse.Body.Bytes(), &retried) != nil || retried.Created || retried.Task.ID != submitted.Task.ID {
		t.Fatalf("idempotent HTTP retry created a duplicate: %d %+v %s", taskRetryResponse.Code, retried, taskRetryResponse.Body.String())
	}
	conflictBody, _ := json.Marshal(map[string]any{"targetNodeId": paired.Node.ID, "payload": map[string]any{"kind": "agent_turn", "prompt": "different request"}})
	conflictRequest := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", bytes.NewReader(conflictBody))
	conflictRequest.Header.Set("Idempotency-Key", "http-task-1")
	conflictRequest.AddCookie(setupCookie)
	conflictResponse := httptest.NewRecorder()
	handler.ServeHTTP(conflictResponse, conflictRequest)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("same idempotency key with different body returned %d", conflictResponse.Code)
	}
	claimRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/tasks/claim", nil)
	claimRequest.Header.Set("Authorization", "Bearer "+paired.Credential)
	claimResponse := httptest.NewRecorder()
	handler.ServeHTTP(claimResponse, claimRequest)
	var claimed struct {
		Task       storage.ExecutionTask `json:"task"`
		LeaseToken string                `json:"leaseToken"`
	}
	if claimResponse.Code != http.StatusOK || json.Unmarshal(claimResponse.Body.Bytes(), &claimed) != nil || claimed.Task.ID != submitted.Task.ID || claimed.LeaseToken == "" {
		t.Fatalf("node task claim failed: %d %+v %s", claimResponse.Code, claimed, claimResponse.Body.String())
	}
	for _, state := range []string{"accepted", "running"} {
		reportBody, _ := json.Marshal(map[string]string{"status": state})
		reportRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/tasks/"+claimed.Task.ID+"/report", bytes.NewReader(reportBody))
		reportRequest.Header.Set("Authorization", "Bearer "+paired.Credential)
		reportRequest.Header.Set("X-O-Task-Lease", claimed.LeaseToken)
		reportResponse := httptest.NewRecorder()
		handler.ServeHTTP(reportResponse, reportRequest)
		if reportResponse.Code != http.StatusOK {
			t.Fatalf("task %s report returned %d: %s", state, reportResponse.Code, reportResponse.Body.String())
		}
	}
	resultBody, _ := json.Marshal(map[string]any{"status": "reported_succeeded", "result": map[string]string{"summary": "node reported done"}})
	resultRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/tasks/"+claimed.Task.ID+"/report", bytes.NewReader(resultBody))
	resultRequest.Header.Set("Authorization", "Bearer "+paired.Credential)
	resultRequest.Header.Set("X-O-Task-Lease", claimed.LeaseToken)
	resultResponse := httptest.NewRecorder()
	handler.ServeHTTP(resultResponse, resultRequest)
	if resultResponse.Code != http.StatusOK {
		t.Fatalf("terminal task report returned %d: %s", resultResponse.Code, resultResponse.Body.String())
	}
	eventRequest := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+claimed.Task.ID+"/events", nil)
	eventRequest.AddCookie(setupCookie)
	eventResponse := httptest.NewRecorder()
	handler.ServeHTTP(eventResponse, eventRequest)
	var taskEvents []storage.ExecutionTaskEvent
	if eventResponse.Code != http.StatusOK || json.Unmarshal(eventResponse.Body.Bytes(), &taskEvents) != nil || len(taskEvents) != 5 {
		t.Fatalf("task event journal unavailable: %d %s", eventResponse.Code, eventResponse.Body.String())
	}
	taskReadRequest := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+claimed.Task.ID, nil)
	taskReadRequest.AddCookie(setupCookie)
	taskReadResponse := httptest.NewRecorder()
	handler.ServeHTTP(taskReadResponse, taskReadRequest)
	var taskRead storage.ExecutionTask
	if taskReadResponse.Code != http.StatusOK || json.Unmarshal(taskReadResponse.Body.Bytes(), &taskRead) != nil || taskRead.Status != "reported_succeeded" {
		t.Fatalf("user-visible task state did not distinguish a node report from verified completion: %d %+v", taskReadResponse.Code, taskRead)
	}
	nodeOnlyRequest := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	nodeOnlyRequest.Header.Set("Authorization", "Bearer "+paired.Credential)
	nodeOnlyResponse := httptest.NewRecorder()
	handler.ServeHTTP(nodeOnlyResponse, nodeOnlyRequest)
	if nodeOnlyResponse.Code != http.StatusUnauthorized {
		t.Fatalf("node credential authorized user API: %d", nodeOnlyResponse.Code)
	}
	nodeOnlyStorage := httptest.NewRecorder()
	nodeOnlyStorageRequest := httptest.NewRequest(http.MethodGet, "/api/v1/system/artifact-storage", nil)
	nodeOnlyStorageRequest.Header.Set("Authorization", "Bearer "+paired.Credential)
	handler.ServeHTTP(nodeOnlyStorage, nodeOnlyStorageRequest)
	if nodeOnlyStorage.Code != http.StatusUnauthorized {
		t.Fatalf("node credential exposed user artifact storage capacity: %d", nodeOnlyStorage.Code)
	}
	revokeRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/nodes/"+paired.Node.ID, nil)
	revokeRequest.AddCookie(setupCookie)
	revokeResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokeResponse, revokeRequest)
	if revokeResponse.Code != http.StatusOK {
		t.Fatalf("node revoke returned %d: %s", revokeResponse.Code, revokeResponse.Body.String())
	}
	heartbeatRequest = httptest.NewRequest(http.MethodPost, "/api/v1/nodes/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatRequest.Header.Set("Authorization", "Bearer "+paired.Credential)
	heartbeatResponse = httptest.NewRecorder()
	handler.ServeHTTP(heartbeatResponse, heartbeatRequest)
	if heartbeatResponse.Code != http.StatusUnauthorized {
		t.Fatalf("revoked node credential returned %d, want 401", heartbeatResponse.Code)
	}

	logout := httptest.NewRecorder()
	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	logoutRequest.AddCookie(setupCookie)
	handler.ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout returned %d: %s", logout.Code, logout.Body.String())
	}
	protected = httptest.NewRecorder()
	protectedRequest = httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	protectedRequest.AddCookie(setupCookie)
	handler.ServeHTTP(protected, protectedRequest)
	if protected.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session remained authorized: %d", protected.Code)
	}

	loginBody, _ := json.Marshal(map[string]string{"email": "owner@example.com", "password": "long-secure-password"})
	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(loginBody))
	loginRequest.Header.Set("X-Forwarded-Proto", "https")
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("login returned %d: %s", login.Code, login.Body.String())
	}
	if len(login.Result().Cookies()) == 0 || login.Result().Cookies()[0].Value == "" {
		t.Fatal("login did not issue a session cookie")
	}
	if !login.Result().Cookies()[0].Secure || !login.Result().Cookies()[0].HttpOnly {
		t.Fatal("HTTPS login cookie must be Secure and HttpOnly")
	}
	if _, _, err := store.AuthUserByID(context.Background(), ownerID); err != nil {
		t.Fatalf("account state lost after login: %v", err)
	}
}

func TestPBKDF2SHA256MatchesStandardVector(t *testing.T) {
	got := hex.EncodeToString(derivePasswordKey("password", []byte("salt"), 1))
	const want = "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got != want {
		t.Fatalf("PBKDF2-SHA256 output %s, want %s", got, want)
	}
}

func TestLoginRateLimitPersistsAcrossRestartAndSuccessClearsIt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hashPassword("long-secure-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetInitialCredentials(ctx, owner, "owner@example.com", "Owner", hash); err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store}
	server.SetRemoteAuthentication("01234567890123456789012345678901")
	handler := server.Handler()
	for attempt := 1; attempt <= 5; attempt++ {
		body := []byte(`{"email":"owner@example.com","password":"incorrect"}`)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		request.RemoteAddr = "203.0.113.8:4567"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if attempt < 5 && response.Code != http.StatusUnauthorized {
			t.Fatalf("failed attempt %d returned %d: %s", attempt, response.Code, response.Body.String())
		}
		if attempt == 5 && (response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "") {
			t.Fatalf("lockout was not reported after the fifth failure: %d %v %s", response.Code, response.Header(), response.Body.String())
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server = &Server{workspaceID: owner, store: store}
	server.SetRemoteAuthentication("01234567890123456789012345678901")
	handler = server.Handler()
	blocked := httptest.NewRecorder()
	blockedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader([]byte(`{"email":"owner@example.com","password":"long-secure-password"}`)))
	blockedRequest.RemoteAddr = "203.0.113.8:9876"
	handler.ServeHTTP(blocked, blockedRequest)
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("lockout did not survive server restart: %d %s", blocked.Code, blocked.Body.String())
	}
	bucket := hashToken("login-ip:203.0.113.8")
	if err := store.ClearAuthLoginFailures(ctx, bucket); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader([]byte(`{"email":"owner@example.com","password":"long-secure-password"}`)))
	loginRequest.RemoteAddr = "203.0.113.8:1111"
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("valid login after operator reset failed: %d %s", login.Code, login.Body.String())
	}
	allowed, _, err := store.AuthLoginAllowed(ctx, bucket, time.Now().UTC())
	if err != nil || !allowed {
		t.Fatalf("successful login did not clear failure state: allowed=%v err=%v", allowed, err)
	}
}

func TestPersistentBrowserSessionSurvivesServerRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	passwordHash, err := hashPassword("long-secure-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetInitialCredentials(ctx, owner, "owner@example.com", "Owner", passwordHash); err != nil {
		t.Fatal(err)
	}
	server := &Server{workspaceID: owner, store: store}
	server.SetRemoteAuthentication("01234567890123456789012345678901")
	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader([]byte(`{"email":"owner@example.com","password":"long-secure-password"}`)))
	loginRequest.Header.Set("X-Forwarded-Proto", "https")
	server.Handler().ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login failed to issue a persistent session: %d %s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	if cookie.MaxAge != int(authSessionLifetime.Seconds()) || cookie.Expires.IsZero() || !cookie.Secure || !cookie.HttpOnly {
		t.Fatalf("browser session cookie has unexpected persistence/security attributes: %+v", cookie)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close reopened auth storage: %v", err)
		}
	}()
	server = &Server{workspaceID: owner, store: store}
	server.SetRemoteAuthentication("01234567890123456789012345678901")
	session := httptest.NewRecorder()
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	sessionRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(session, sessionRequest)
	if session.Code != http.StatusOK || !bytes.Contains(session.Body.Bytes(), []byte(`"authenticated":true`)) {
		t.Fatalf("persistent session did not survive storage/server restart: %d %s", session.Code, session.Body.String())
	}
}
