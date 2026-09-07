package evolution

import (
	"context"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func testStore(t *testing.T) (*storage.Store, string) {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	userID := "usr_test"
	err = store.CreateUser(context.Background(), domain.User{ID: userID, Email: "test@example.com", DisplayName: "Test", CreatedAt: time.Now().UTC()}, "hash")
	if err != nil {
		t.Fatal(err)
	}
	return store, userID
}

func TestSeedIsStableAndIdempotent(t *testing.T) {
	store, userID := testStore(t)
	service := New(store)
	first, err := service.EnsureSeed(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.EnsureSeed(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Status != "stable" || first.Definition.Spec.Strategy != StrategyReact {
		t.Fatalf("unexpected seed: %#v / %#v", first, second)
	}
	if first.DefinitionDigest == "" || first.DefinitionDigest != first.Definition.Digest {
		t.Fatalf("seed digest was not pinned: %#v", first)
	}
}

func TestCreateCandidateProducesImmutableChildGeneration(t *testing.T) {
	store, userID := testStore(t)
	service := New(store)
	seed, err := service.EnsureSeed(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.CreateCandidate(context.Background(), userID, CandidateInput{
		Name: "Planning child", Description: "Adds a bounded planning stage", ParentGenerationID: seed.ID,
		Spec: domain.AgentSpec{Strategy: StrategyPlanReact, SystemPrompt: DefaultSystemPrompt, MaxSteps: 16},
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Number != 2 || candidate.Status != "candidate" || candidate.Definition.ParentDigest != seed.DefinitionDigest {
		t.Fatalf("unexpected candidate lineage: %#v", candidate)
	}
	if candidate.Definition.Spec.PlannerPrompt == "" || candidate.DefinitionDigest == seed.DefinitionDigest {
		t.Fatalf("candidate definition was not normalized and versioned: %#v", candidate.Definition)
	}
	loaded, err := service.Generation(context.Background(), userID, candidate.ID)
	if err != nil || loaded.DefinitionDigest != candidate.DefinitionDigest {
		t.Fatalf("candidate did not round-trip: %#v, %v", loaded, err)
	}
}
