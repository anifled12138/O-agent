package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"axiom.local/agent/internal/gitcredential"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func TestGitCredentialAPIChangesBrokerStateWithoutReturningSecret(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, err := secure.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := gitcredential.New(ctx, store, vault)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{gitCredentials: service}
	const secret = "api-secret-token"
	body, err := json.Marshal(gitcredential.Credential{ID: "github", Host: "github.com", Repository: "acme/o-agent", Username: "git", Password: secret})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/system/git-credentials", bytes.NewReader(body))
	response := httptest.NewRecorder()
	server.gitCredentialsSave(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatal("credential save response disclosed the password")
	}
	username, password, ok := service.Lookup("https://github.com/acme/o-agent.git")
	if !ok || username != "git" || password != secret {
		t.Fatalf("saved API mutation did not reach the execution broker: username=%q password=%q ok=%t", username, password, ok)
	}

	listResponse := httptest.NewRecorder()
	server.gitCredentialsList(listResponse, httptest.NewRequest(http.MethodGet, "/api/v1/system/git-credentials", nil))
	if listResponse.Code != http.StatusOK || strings.Contains(listResponse.Body.String(), secret) {
		t.Fatalf("credential list disclosed a secret or failed: status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/system/git-credentials/github", nil)
	deleteRequest.SetPathValue("id", "github")
	deleteResponse := httptest.NewRecorder()
	server.gitCredentialsDelete(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if _, _, ok := service.Lookup("https://github.com/acme/o-agent.git"); ok {
		t.Fatal("deleted credential remains available to the execution broker")
	}
}
