package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/deviceaccount"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/execution"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginruntime"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func TestDeviceAccountEndToEndWithoutCloudModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type host struct {
		store       *storage.Store
		vault       *secure.Vault
		providers   *provider.Service
		agent       *agent.Service
		artifacts   *artifactstore.Service
		owner, data string
	}
	newHost := func() host {
		t.Helper()
		h := host{data: t.TempDir()}
		var err error
		h.store, err = storage.Open(h.data)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := h.store.Close(); err != nil {
				t.Error(err)
			}
		})
		h.owner, err = h.store.EnsureLocalWorkspaceOwner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		h.vault, err = secure.Open(h.data)
		if err != nil {
			t.Fatal(err)
		}
		h.providers = provider.New(h.store, h.vault)
		evol := evolution.New(h.store)
		if _, err = evol.EnsureSeed(ctx, h.owner); err != nil {
			t.Fatal(err)
		}
		workspace := t.TempDir()
		repo, err := pluginforge.OpenRepository(h.data)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := repo.Close(); err != nil {
				t.Error(err)
			}
		})
		supervisor, err := pluginruntime.NewWithData(workspace, filepath.Join(h.data, "plugins"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { supervisor.Close() })
		forge := pluginforge.NewService(repo, supervisor, h.data, workspace)
		h.agent, err = agent.New(ctx, h.store, h.providers, forge, evol, workspace, h.data, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		h.artifacts, err = artifactstore.New(h.data, h.store)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	cloud, local := newHost(), newHost()
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if in.Model != "chosen-on-pc" {
			t.Error("wrong local model", in.Model)
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "Answered by the PC."}}}}); err != nil {
			t.Error(err)
		}
	}))
	defer model.Close()
	chosen, err := local.providers.Create(ctx, local.owner, provider.Input{Name: "PC model", Kind: provider.KindOpenAICompatible, BaseURL: model.URL + "/v1", Model: "chosen-on-pc", APIKey: "synthetic-model-key"})
	if err != nil {
		t.Fatal(err)
	}
	api := &Server{workspaceID: cloud.owner, store: cloud.store, providers: cloud.providers, agent: cloud.agent, artifacts: cloud.artifacts}
	api.SetRemoteAuthentication(strings.Repeat("b", 32))
	api.SetDeviceAuthorizationVault(cloud.vault)
	control := httptest.NewServer(api.Handler())
	defer control.Close()
	api.frontendOrigin = control.URL
	if err = cloud.store.CreateAuthSession(ctx, hashToken("synthetic-browser-session"), cloud.owner, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: authCookieName, Value: "synthetic-browser-session"}
	call := func(path string, input, out any) {
		t.Helper()
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest("POST", control.URL+"/api/v1"+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", control.URL)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "e2e-request")
		req.AddCookie(cookie)
		res, err := control.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("%s returned %d: %s", path, res.StatusCode, b)
		}
		if out != nil {
			if err = json.NewDecoder(res.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	executor, err := execution.NewLocalAgentExecutor(local.agent, local.providers, local.owner, "")
	if err != nil {
		t.Fatal(err)
	}
	account, err := deviceaccount.New(ctx, local.store, local.vault, func(c context.Context, b deviceaccount.Binding, observed func()) error {
		worker, err := execution.NewNodeWorker(b.ServerURL, b.Credential, runtime.GOOS, []string{"agent-runtime", "conversation-transcript", "sandboxed-tools", "safe-handoff"}, executor)
		if err != nil {
			return err
		}
		if err = worker.SetConcurrency(1); err != nil {
			return err
		}
		if err = worker.SetOutboxDirectory(filepath.Join(local.data, "outbox", b.OutboxKey())); err != nil {
			return err
		}
		worker.SetAccountReporting(b.NodeID, func(context.Context) (storage.NodeCatalog, error) {
			return storage.NodeCatalog{Models: []storage.NodeModel{{ID: chosen.ID, Name: chosen.Name, Model: chosen.Model}}, PermissionProfiles: []domain.PermissionProfile{domain.PermissionProfileReadOnly}}, nil
		}, observed)
		return worker.Run(c)
	}, execution.IsNodeCredentialRejected)
	if err != nil {
		t.Fatal(err)
	}
	defer account.Close()
	state, err := account.Begin(ctx, control.URL, "PC", runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(state.VerificationURL)
	if err != nil {
		t.Fatal(err)
	}
	call("/auth/device/approve", map[string]string{"code": u.Query().Get("device_login")}, nil)
	wait := func(f func() bool) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			if f() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("flow did not converge", account.Status())
	}
	wait(func() bool { return account.Status().State == "connected" })
	var conversation domain.Conversation
	call("/conversations", map[string]any{"title": "From phone", "targetNodeId": account.Status().NodeID, "localProviderId": chosen.ID, "permissionProfile": domain.PermissionProfileReadOnly}, &conversation)
	configured, err := cloud.providers.List(ctx, cloud.owner)
	if err != nil || len(configured) != 0 {
		t.Fatal("remote-only flow required an invented cloud model", err)
	}
	var submitted struct{ Task storage.ExecutionTask }
	call("/conversations/"+conversation.ID+"/tasks/nodes", map[string]string{"targetNodeId": account.Status().NodeID, "localProviderId": chosen.ID, "content": "Answer this on the PC"}, &submitted)
	var completed storage.ExecutionTask
	wait(func() bool {
		var err error
		completed, err = cloud.store.ExecutionTask(ctx, cloud.owner, submitted.Task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return completed.Status == "reported_succeeded" || completed.Status == "reported_failed" || completed.Status == "needs_reconciliation"
	})
	if completed.Status != "reported_succeeded" {
		t.Fatal("PC did not report a verified durable result", completed.Status, completed.Error)
	}
	var result struct {
		ConversationID     string                        `json:"conversationId"`
		AssistantText      string                        `json:"assistantText"`
		TranscriptArtifact storage.ExecutionTaskArtifact `json:"transcriptArtifact"`
	}
	if err = json.Unmarshal(completed.Result, &result); err != nil {
		t.Fatal(err)
	}
	detail, err := local.store.Conversation(ctx, local.owner, result.ConversationID)
	if err != nil || detail.ProviderID != chosen.ID || detail.PermissionProfile != domain.PermissionProfileReadOnly || calls.Load() != 1 {
		t.Fatal("execution settings not consumed by PC", err)
	}
	if result.AssistantText != "Answered by the PC." || result.TranscriptArtifact.ID == "" {
		t.Fatal("cloud did not receive result and archive", result)
	}
	_, file, err := cloud.artifacts.Open(ctx, cloud.owner, result.TranscriptArtifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	var transcript struct{ Conversation domain.ConversationDetail }
	decodeErr := json.NewDecoder(file).Decode(&transcript)
	closeErr := file.Close()
	if decodeErr != nil || closeErr != nil || len(transcript.Conversation.Messages) != 2 || transcript.Conversation.Messages[1].Content != result.AssistantText {
		t.Fatal("cloud transcript was not durable", decodeErr, closeErr)
	}
	state, err = account.Logout(ctx)
	if err != nil || state.State != "signed_out" || state.User != nil {
		t.Fatal("logout left remote identity active", state, err)
	}
	wait(func() bool { return account.Status().PendingRevocations == 0 })
	node, err := cloud.store.ExecutionNode(ctx, cloud.owner, completed.NodeID)
	if err != nil || node.RevokedAt == nil {
		t.Fatal("remote logout not consumed by cloud", err)
	}
}
