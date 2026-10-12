package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginruntime"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func TestLocalAgentTaskConsumesSelectedProviderAndPermission(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	workspace := t.TempDir()
	s, err := storage.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owner, err := s.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, err := secure.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	providers := provider.New(s, v)
	calls := 0
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Model != "chosen-model" {
			t.Error("remote task consumed the wrong local provider", input.Model)
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": "chosen-model", "choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "Selected model answered."}}}, "usage": map[string]int{"total_tokens": 5}})
	}))
	defer modelServer.Close()
	for _, name := range []string{"default-model", "chosen-model"} {
		if _, err = providers.Create(ctx, owner, provider.Input{Name: name, Kind: provider.KindOpenAICompatible, BaseURL: modelServer.URL + "/v1", Model: name, APIKey: "synthetic-test-key"}); err != nil {
			t.Fatal(err)
		}
	}
	models, err := providers.List(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	chosen := ""
	for _, m := range models {
		if m.Model == "chosen-model" {
			chosen = m.ID
		}
	}
	evol := evolution.New(s)
	if _, err = evol.EnsureSeed(ctx, owner); err != nil {
		t.Fatal(err)
	}
	repo, err := pluginforge.OpenRepository(data)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	supervisor, err := pluginruntime.NewWithData(workspace, filepath.Join(data, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	forge := pluginforge.NewService(repo, supervisor, data, workspace)
	runtime, err := agent.New(ctx, s, providers, forge, evol, workspace, data, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewLocalAgentExecutor(runtime, providers, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	source := localAgentTranscript{Conversation: domain.ConversationDetail{Conversation: domain.Conversation{ID: "cloud-conversation", PermissionProfile: domain.PermissionProfileWorkspaceAutonomy}}}
	body, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "source.json")
	if err = os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	input := localAgentPrompt{Kind: "agent_prompt", SourceConversationID: "cloud-conversation", Content: "Answer using the selected model", LocalProviderID: chosen, PermissionProfile: domain.PermissionProfileReadOnly, SourceContextArtifactID: "artifact", SourceContextFilePath: file, SourceContextSHA256: hex.EncodeToString(digest[:]), SourceContextByteSize: int64(len(body))}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.ExecuteNodeTask(ctx, storage.ExecutionTask{ID: "task-selection", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		ConversationID     string `json:"conversationId"`
		TranscriptFilePath string `json:"transcriptFilePath"`
	}
	if err = json.Unmarshal(result, &receipt); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Conversation(ctx, owner, receipt.ConversationID)
	if err != nil {
		t.Fatal(string(result), err)
	}
	if calls == 0 || detail.ProviderID != chosen || detail.PermissionProfile != domain.PermissionProfileReadOnly || len(detail.Messages) != 2 || detail.Messages[1].Content != "Selected model answered." {
		t.Fatal("task selection not consumed by runtime", detail, calls)
	}
	if receipt.TranscriptFilePath != "" {
		if err = os.Remove(receipt.TranscriptFilePath); err != nil {
			t.Error(err)
		}
	}
}
