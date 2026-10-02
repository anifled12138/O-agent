package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func TestCanonicalBaseURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "root gains v1", input: "https://api.example.com", want: "https://api.example.com/v1", ok: true},
		{name: "existing v1 remains", input: "https://api.example.com/v1/", want: "https://api.example.com/v1", ok: true},
		{name: "custom path remains", input: "http://localhost:9000/openai/v1", want: "http://localhost:9000/openai/v1", ok: true},
		{name: "credentials rejected", input: "https://user:pass@api.example.com", ok: false},
		{name: "query rejected", input: "https://api.example.com?token=value", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := canonicalBaseURL(test.input)
			if ok != test.ok || got != test.want {
				t.Fatalf("canonicalBaseURL(%q) = %q, %v; want %q, %v", test.input, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestBodyPreviewDoesNotEchoHTML(t *testing.T) {
	if got := bodyPreview([]byte("<html><body>upstream login page</body></html>")); got != "HTML response" {
		t.Fatalf("bodyPreview returned %q", got)
	}
}

func TestModelExamplesPreferSameFamily(t *testing.T) {
	got := modelExamples([]string{"claude-sonnet", "gpt-5.6-sol", "gpt-5.5", "gemini-pro"}, "gpt-5.6", 2)
	if got[0] != "gpt-5.5" || got[1] != "gpt-5.6-sol" {
		t.Fatalf("modelExamples returned %v", got)
	}
}

func TestCompletionPreflightAuditsSerializedRequestBeforeSend(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	user := domain.User{ID: "provider_audit_user", Email: "provider-audit@axiom.local", DisplayName: "Provider audit", CreatedAt: now}
	if err := store.CreateUser(ctx, user, "test"); err != nil {
		t.Fatal(err)
	}
	vault, err := secure.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, vault)

	var sentBody []byte
	var callbackRanBeforeSend bool
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		callbackRanBeforeSend = true
		sentBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"test-model","choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer server.Close()
	service.client = server.Client()
	configured, err := service.Create(ctx, user.ID, Input{Name: "audit", Kind: KindOpenAICompatible, BaseURL: server.URL, Model: "test-model", APIKey: "test-key", ContextWindow: 8192})
	if err != nil {
		t.Fatal(err)
	}
	messages := []ChatMessage{{Role: "user", Content: "hello"}}
	var audit RequestAudit
	observerRan := false
	callCtx := WithRequestAudit(ctx, func(got RequestAudit) error {
		if callbackRanBeforeSend {
			t.Fatal("request was sent before its serialized payload was audited")
		}
		audit, observerRan = got, true
		return nil
	})
	if _, err := service.CompleteWithTools(callCtx, user.ID, configured.ID, messages, nil); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(sentBody)
	if !observerRan || audit.PayloadSHA256 != hex.EncodeToString(digest[:]) || audit.RequestBytes != len(sentBody) {
		t.Fatalf("request audit did not match the wire payload: audit=%+v body=%d", audit, len(sentBody))
	}
	auditFailure := errors.New("trace persistence failed")
	callbackRanBeforeSend = false
	_, err = service.CompleteWithTools(WithRequestAudit(ctx, func(RequestAudit) error { return auditFailure }), user.ID, configured.ID, messages, nil)
	if !errors.Is(err, auditFailure) || requestCount != 1 {
		t.Fatalf("request was sent after the audit callback failed: requests=%d err=%v", requestCount, err)
	}

	requests := 0
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer blocked.Close()
	service.client = blocked.Client()
	tiny, err := service.Create(ctx, user.ID, Input{Name: "tiny", Kind: KindOpenAICompatible, BaseURL: blocked.URL, Model: "test-model", APIKey: "test-key", ContextWindow: 8})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CompleteWithTools(ctx, user.ID, tiny.ID, []ChatMessage{{Role: "user", Content: strings.Repeat("large input ", 20)}}, nil)
	var providerErr *ProviderError
	if err == nil || !errors.As(err, &providerErr) || providerErr.Class != ErrorContextOverflow || requests != 0 {
		t.Fatalf("oversized input was not rejected before HTTP: err=%v requests=%d", err, requests)
	}
}
