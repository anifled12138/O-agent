package deviceaccount

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

type failingSettings struct {
	Settings
	fail atomic.Bool
}

func (f *failingSettings) SetRuntimeSetting(c context.Context, k, v string) error {
	if f.fail.Load() {
		return errors.New("injected storage failure")
	}
	return f.Settings.SetRuntimeSetting(c, k, v)
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(6 * time.Second)
	for time.Now().Before(until) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("state never converged")
}
func TestOptionalLoginEncryptedRestartLogoutAndRollback(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, err := secure.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	settings := &failingSettings{Settings: store}
	var authorized, offline atomic.Bool
	var starts, active, revoked atomic.Int32
	credential := strings.Repeat("private-device-token-", 3)
	clientID := ""
	var base string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if offline.Load() {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/api/v1/auth/device/start":
			var input struct{ ClientID string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if clientID != "" && clientID != input.ClientID {
				t.Error("client identity changed on restart")
			}
			clientID = input.ClientID
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"deviceCode": strings.Repeat("d", 43), "verificationUrl": base + "/?device_login=" + strings.Repeat("u", 43), "expiresAt": time.Now().Add(time.Minute)})
		case "/api/v1/auth/device/poll":
			if !authorized.Load() {
				w.WriteHeader(202)
				json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "authorized", "nodeId": "node-test", "credential": credential, "user": User{ID: "owner", Email: "owner@example.test", DisplayName: "Owner"}})
		case "/api/v1/nodes/self/revoke":
			if r.Header.Get("Authorization") != "Bearer "+credential {
				t.Error("revocation did not use node identity")
			}
			revoked.Add(1)
			json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer cloud.Close()
	base = cloud.URL
	runner := func(ctx context.Context, b Binding, observed func()) error {
		starts.Add(1)
		if active.Add(1) != 1 {
			t.Error("duplicate worker")
		}
		defer active.Add(-1)
		if b.ServerURL != base || b.Credential != credential {
			t.Error("wrong worker binding")
		}
		observed()
		<-ctx.Done()
		return ctx.Err()
	}
	s, err := New(ctx, settings, vault, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != "signed_out" || active.Load() != 0 {
		t.Fatal("optional local use started remote runtime")
	}
	state, err := s.Begin(ctx, base, "PC", "windows")
	if err != nil || state.State != "awaiting_login" {
		t.Fatal(state, err)
	}
	s.Close()
	s, err = New(ctx, settings, vault, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	authorized.Store(true)
	settings.fail.Store(true)
	s.notify()
	eventually(t, func() bool { return strings.Contains(s.Status().Error, "无法保存") })
	if active.Load() != 0 {
		t.Fatal("worker consumed credentials before durable persistence")
	}
	settings.fail.Store(false)
	s.notify()
	eventually(t, func() bool { return s.Status().State == "connected" })
	raw, err := store.RuntimeSetting(ctx, settingKey)
	if err != nil || strings.Contains(raw, credential) || strings.Contains(raw, "owner@example") {
		t.Fatal("local identity not encrypted")
	}
	settings.fail.Store(true)
	if _, err = s.Logout(ctx); err == nil {
		t.Fatal("logout ignored persistence error")
	}
	if s.Status().State != "connected" || active.Load() != 1 {
		t.Fatal("failed logout disconnected durable binding")
	}
	settings.fail.Store(false)
	s.Close()
	if active.Load() != 0 {
		t.Fatal("close left worker active")
	}
	s, err = New(ctx, settings, vault, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.Status().State == "connected" })
	if starts.Load() != 2 {
		t.Fatal("restart failed to resume same worker")
	}
	offline.Store(true)
	state, err = s.Logout(ctx)
	if err != nil || state.User != nil || state.State != "signed_out" || active.Load() != 0 {
		t.Fatal("logout did not stop remote runtime", state, err)
	}
	s.Close()
	s, err = New(ctx, settings, vault, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Status().User != nil || s.Status().PendingRevocations != 1 {
		t.Fatal("offline logout did not persist revocation")
	}
	offline.Store(false)
	s.notify()
	eventually(t, func() bool { return s.Status().PendingRevocations == 0 })
	if revoked.Load() != 1 || active.Load() != 0 {
		t.Fatal("revocation/restart started unauthorized runtime")
	}
}
func TestNormalizeServer(t *testing.T) {
	for _, raw := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/?token=x", "file:///a"} {
		if _, err := NormalizeServer(raw); err == nil {
			t.Fatal(raw)
		}
	}
	if value, err := NormalizeServer(" https://example.com/ "); err != nil || value != "https://example.com" {
		t.Fatal(value, err)
	}
}

func TestRejectedDeviceLoginExpiresWithoutStartingWorker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, err := secure.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var polls, starts atomic.Int32
	var base string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/device/start" {
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"deviceCode": strings.Repeat("d", 43), "verificationUrl": base + "/?device_login=" + strings.Repeat("u", 43), "expiresAt": time.Now().Add(time.Minute)})
		} else {
			polls.Add(1)
			w.WriteHeader(http.StatusGone)
		}
	}))
	defer cloud.Close()
	base = cloud.URL
	s, err := New(ctx, store, vault, func(context.Context, Binding, func()) error { starts.Add(1); return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Begin(ctx, base, "PC", "windows"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.Status().State == "login_expired" })
	s.poll()
	if polls.Load() != 1 || starts.Load() != 0 || s.Status().User != nil {
		t.Fatal("invalid authorization was retried or started a worker")
	}
}

func TestUnverifiedRevocationResponseRemainsPending(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, err := secure.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "gateway denied request"})
	}))
	defer cloud.Close()
	runner := func(context.Context, Binding, func()) error { t.Error("revocation queue started worker"); return nil }
	s, err := New(ctx, store, vault, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.data.Revocations = []Binding{{ServerURL: cloud.URL, NodeID: "pc", Credential: strings.Repeat("private", 8), User: User{ID: "owner"}}}
	if err = s.persist(ctx, s.data); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	s.revokePending()
	if s.Status().PendingRevocations != 1 {
		t.Fatal("unverified gateway response cleared revocation")
	}
	s.Close()
	s, err = New(ctx, store, vault, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Status().State != "signed_out" || s.Status().PendingRevocations != 1 {
		t.Fatal("pending revocation was lost")
	}
}
