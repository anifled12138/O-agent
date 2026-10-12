package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestAccountOnlineRequiresCatalogAndHeartbeatReadback(t *testing.T) {
	ctx := context.Background()
	catalog := storage.NodeCatalog{Models: []storage.NodeModel{{ID: "provider", Name: "Model", Model: "test"}}, PermissionProfiles: []domain.PermissionProfile{domain.PermissionProfileReadOnly}}
	wrong := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		body["id"] = "node-account"
		body["connectivity"] = "connected"
		body["lastSeen"] = time.Now().UTC()
		if wrong {
			delete(body, "catalog")
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	worker, err := NewNodeWorker(server.URL, strings.Repeat("x", 32), "windows", []string{"agent-runtime"}, nodeWorkerExecutorFunc(func(context.Context, storage.ExecutionTask) (json.RawMessage, error) {
		t.Error("unexpected task execution")
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	observed := 0
	worker.SetAccountReporting("node-account", func(context.Context) (storage.NodeCatalog, error) { return catalog, nil }, func() { observed++ })
	resources := storage.ExecutionNodeResources{MaxConcurrentTasks: 1}
	if err = worker.reportResources(ctx, resources, true); err == nil || observed != 0 {
		t.Fatal("missing catalog was reported online")
	}
	wrong = false
	if err = worker.reportResources(ctx, resources, true); err != nil || observed != 1 {
		t.Fatal("verified heartbeat did not notify account", err)
	}
}
