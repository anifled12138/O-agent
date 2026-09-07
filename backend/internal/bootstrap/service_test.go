package bootstrap

import (
	"context"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/storage"
)

type fakeModel struct{ response string }

func (f fakeModel) Complete(context.Context, string, string, []provider.ChatMessage) (string, error) {
	return f.response, nil
}

func TestGenerateCandidatesPersistsOnlyCandidateDefinitions(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	userID := "usr_bootstrap"
	if err := store.CreateUser(context.Background(), domain.User{ID: userID, Email: "bootstrap@example.com", DisplayName: "Bootstrap", CreatedAt: time.Now().UTC()}, "hash"); err != nil {
		t.Fatal(err)
	}
	evolutionService := evolution.New(store)
	challenge, err := evolutionService.CreateChallenge(context.Background(), userID, evolution.ChallengeInput{Title: "Complex tasks stall", Objective: "Improve decomposition", SuccessCriteria: "returns DONE"})
	if err != nil {
		t.Fatal(err)
	}
	service := New(evolutionService, fakeModel{response: "```json\n{\"candidates\":[{\"name\":\"Planner\",\"description\":\"separate planning from action\",\"strategy\":\"plan-react.v1\",\"systemPrompt\":\"Use observations as truth.\",\"plannerPrompt\":\"Make a brief.\",\"maxSteps\":15}]}\n```"})
	candidates, err := service.GenerateCandidates(context.Background(), userID, challenge.ID, GenerateInput{ProviderID: "provider-not-used-by-fake", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Status != "candidate" || candidates[0].Definition.Spec.Strategy != evolution.StrategyPlanReact {
		t.Fatalf("unexpected candidates: %#v", candidates)
	}
	stable, err := evolutionService.Stable(context.Background(), userID)
	if err != nil || stable.ID == candidates[0].ID {
		t.Fatalf("self-bootstrap bypassed stable gate: %#v, %v", stable, err)
	}
}

func TestExtractJSONObjectHandlesProseAndFence(t *testing.T) {
	got := string(extractJSONObject("prefix ```json\n{\"candidates\":[]}\n``` suffix"))
	if got != "{\"candidates\":[]}" {
		t.Fatalf("unexpected extraction: %q", got)
	}
}
