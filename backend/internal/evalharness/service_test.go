package evalharness

import (
	"context"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/storage"
)

type fakeRunner struct{}

func (fakeRunner) RunEvaluation(_ context.Context, _ string, _ string, generation domain.AgentGeneration, _ string) (string, domain.RunMetrics, error) {
	if generation.Status == "candidate" {
		return "DONE", domain.RunMetrics{ModelCalls: 2, TotalTokens: 60}, nil
	}
	return "not solved", domain.RunMetrics{ModelCalls: 1, TotalTokens: 40}, nil
}

func TestHarnessRunsPairedTrialsAndRecommendsFrontierGain(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	userID := "usr_eval"
	now := time.Now().UTC()
	if err := store.CreateUser(ctx, domain.User{ID: userID, Email: "eval@example.com", DisplayName: "Eval", CreatedAt: now}, "hash"); err != nil {
		t.Fatal(err)
	}
	provider := domain.Provider{ID: "prv_eval", UserID: userID, Name: "Eval", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "fake", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, provider, []byte("cipher"), []byte("nonce")); err != nil {
		t.Fatal(err)
	}
	evolutionService := evolution.New(store)
	seed, err := evolutionService.EnsureSeed(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := evolutionService.CreateChallenge(ctx, userID, evolution.ChallengeInput{Title: "Frontier", Objective: "solve", SuccessCriteria: "response contains DONE", BaselineGenerationID: seed.ID})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := evolutionService.CreateCandidate(ctx, userID, evolution.CandidateInput{Name: "Candidate", Description: "test child", ParentGenerationID: seed.ID, Spec: domain.AgentSpec{Strategy: evolution.StrategyPlanReact, MaxSteps: 12}})
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, evolutionService, fakeRunner{})
	defer service.Close()
	experiment, err := service.Start(ctx, userID, StartInput{ChallengeID: challenge.ID, CandidateGenerationID: candidate.ID, ProviderID: provider.ID, Repetitions: 2, Cases: []domain.EvalCase{{ID: "frontier", Name: "Frontier", Prompt: "solve", Evaluator: "contains", Expected: "DONE"}}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		experiment, err = service.Get(ctx, userID, experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		if experiment.Status == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if experiment.Status != "completed" || experiment.Report == nil {
		t.Fatalf("experiment did not complete: %#v", experiment)
	}
	if len(experiment.Trials) != 4 || experiment.Report.FrontierWins != 2 || experiment.Report.Regressions != 0 || experiment.Report.Recommendation != "promote" {
		t.Fatalf("unexpected paired report: %#v", experiment)
	}
}

func TestNormalizeCasesRejectsUnsafeEvaluatorConfiguration(t *testing.T) {
	if _, err := normalizeCases([]domain.EvalCase{{Prompt: "x", Evaluator: "regex", Expected: "["}}); err == nil {
		t.Fatal("invalid regular expression was accepted")
	}
	if _, err := normalizeCases([]domain.EvalCase{{Prompt: "x", Evaluator: "model-judge"}}); err == nil {
		t.Fatal("unsupported evaluator was accepted")
	}
}
