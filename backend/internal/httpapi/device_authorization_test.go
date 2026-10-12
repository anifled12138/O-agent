package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func TestDeviceAuthorizationEnrollmentIdentityCatalogAndRevocation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secure.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{workspaceID: owner, store: store, frontendOrigin: "https://o.example"}
	s.SetRemoteAuthentication(strings.Repeat("b", 32))
	s.SetDeviceAuthorizationVault(vault)
	if err = store.CreateAuthSession(ctx, hashToken("synthetic-session"), owner, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: authCookieName, Value: "synthetic-session"}
	call := func(path string, input any, cookie *http.Cookie, token, origin string) *httptest.ResponseRecorder {
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", path, bytes.NewReader(body))
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	start := func() (string, string) {
		r := call("/api/v1/auth/device/start", map[string]string{"clientId": strings.Repeat("a", 64), "name": "My PC", "platform": "windows"}, nil, "", "")
		if r.Code != 201 {
			t.Fatalf("start %d %s", r.Code, r.Body)
		}
		var out struct{ DeviceCode, VerificationURL string }
		if err = json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(out.VerificationURL)
		if err != nil {
			t.Fatal(err)
		}
		return out.DeviceCode, u.Query().Get("device_login")
	}
	device, user := start()
	if device == user || len(device) < 32 {
		t.Fatal("codes must be strong and separated")
	}
	pending := call("/api/v1/auth/device/poll", map[string]string{"deviceCode": device}, nil, "", "")
	if pending.Code != 202 {
		t.Fatal(pending.Code)
	}
	wrong := call("/api/v1/auth/device/poll", map[string]string{"deviceCode": user}, nil, "", "")
	if wrong.Code == 200 {
		t.Fatal("public approval code exchanged for credential")
	}
	r := call("/api/v1/auth/device/approve", map[string]string{"code": user}, nil, "", s.frontendOrigin)
	if r.Code == 200 {
		t.Fatal("anonymous approval")
	}
	r = call("/api/v1/auth/device/approve", map[string]string{"code": user}, cookie, "", "https://evil.example")
	if r.Code == 200 {
		t.Fatal("cross origin approval")
	}
	r = call("/api/v1/auth/device/approve", map[string]string{"code": user}, cookie, "", s.frontendOrigin)
	if r.Code != 200 {
		t.Fatalf("approve %d %s", r.Code, r.Body)
	}
	poll := func(code string) (string, string) {
		r := call("/api/v1/auth/device/poll", map[string]string{"deviceCode": code}, nil, "", "")
		if r.Code != 200 {
			t.Fatalf("poll %d %s", r.Code, r.Body)
		}
		var out struct {
			NodeID, Credential string
			User               struct{ ID string }
		}
		if err = json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.User.ID != owner {
			t.Fatal("owner mismatch")
		}
		return out.NodeID, out.Credential
	}
	cancelledDevice, cancelledUser := start()
	cancelled := call("/api/v1/auth/device/cancel", map[string]string{"deviceCode": cancelledDevice}, nil, "", "")
	if cancelled.Code != 200 {
		t.Fatal(cancelled.Code, cancelled.Body)
	}
	rejected := call("/api/v1/auth/device/approve", map[string]string{"code": cancelledUser}, cookie, "", s.frontendOrigin)
	if rejected.Code == 200 {
		t.Fatal("cancelled login enrolled a node")
	}
	nodeID, credential := poll(device)
	bound, err := store.ExecutionNodeForToken(ctx, hashToken(credential))
	if err != nil || bound.ID != nodeID || bound.LastSeen != nil {
		t.Fatalf("durable enrollment did not read back %v", err)
	}
	// Lost replies and fresh browser authorization retain one computer identity.
	device2, user2 := start()
	r = call("/api/v1/auth/device/approve", map[string]string{"code": user2}, cookie, "", s.frontendOrigin)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	id2, credential2 := poll(device2)
	if id2 != nodeID || credential2 != credential {
		t.Fatal("repeated sign-in changed active node identity")
	}
	catalog := storage.NodeCatalog{Models: []storage.NodeModel{{ID: "prv_local", Name: "Local model", Model: "model"}}, PermissionProfiles: []domain.PermissionProfile{domain.PermissionProfileReadOnly}}
	r = call("/api/v1/nodes/heartbeat", map[string]any{"platform": "windows", "capabilities": []string{"agent-runtime"}, "catalog": catalog}, nil, credential, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	saved, err := store.ExecutionNodeCatalog(ctx, nodeID)
	if err != nil || len(saved.Models) != 1 {
		t.Fatal("catalog not consumed", err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	vault, err = secure.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.SetDeviceAuthorizationVault(vault)
	id3, cred3 := poll(device)
	if id3 != nodeID || cred3 != credential {
		t.Fatal("enrollment did not survive restart")
	}
	r = call("/api/v1/nodes/self/revoke", struct{}{}, nil, credential, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	if _, err = store.ExecutionNodeForToken(ctx, hashToken(credential)); err == nil {
		t.Fatal("revoked token still usable")
	}
	// A lost first acknowledgement can be retried against the durable receipt.
	r = call("/api/v1/nodes/self/revoke", struct{}{}, nil, credential, "")
	if r.Code != 200 {
		t.Fatal("revocation receipt was not acknowledged", r.Code, r.Body)
	}
	r = call("/api/v1/nodes/heartbeat", map[string]string{"platform": "windows"}, nil, credential, "")
	if r.Code == 200 {
		t.Fatal("receipt authenticated a revoked credential for execution")
	}
	r = call("/api/v1/nodes/self/revoke", struct{}{}, nil, "unknown-token", "")
	if r.Code == 200 {
		t.Fatal("unknown credential was acknowledged as revoked")
	}

	r = call("/api/v1/auth/device/poll", map[string]string{"deviceCode": device}, nil, "", "")
	if r.Code == 200 {
		t.Fatal("revoked binding could be exchanged")
	}
}
