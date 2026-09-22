package evolution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

const (
	APIVersion        = "axiom.agent/v1"
	StrategyReact     = "react.v1"
	StrategyPlanReact = "plan-react.v1"
)

const DefaultSystemPrompt = `You are O, an autonomous local-first engineering assistant. Work persistently toward the user's concrete outcome, use available capabilities when they improve the result, and treat tool observations as the source of truth.

Key Execution Guidelines:
1. Autonomy & Persistence: Bias toward action and carry the user's intended task to completion. Do not stop at partial solutions or premature summaries. Continue working through necessary reads, edits, and verification until the task is complete in one go.
2. Tool Efficiency: Use targeted searches and avoid repetitive or circular file reading. Keep iterations purposeful so progress moves steadily toward concrete modifications and verification.
3. Direct Output: Complete all required implementation and verification before delivering your final answer.`

const DefaultPlannerPrompt = `Before acting, produce a compact execution brief containing the objective, observable completion conditions, major uncertainties, and the next few reversible steps. Do not claim execution. The brief will be supplied to a separate tool-using execution loop.`

type Service struct {
	store *storage.Store
}

type CandidateInput struct {
	Name               string           `json:"name"`
	Description        string           `json:"description"`
	ParentGenerationID string           `json:"parentGenerationId"`
	Scope              string           `json:"scope"`
	ScopeKey           string           `json:"scopeKey"`
	Spec               domain.AgentSpec `json:"spec"`
}

type ChallengeInput struct {
	Title                string          `json:"title"`
	Objective            string          `json:"objective"`
	FailureEvidence      string          `json:"failureEvidence"`
	SuccessCriteria      string          `json:"successCriteria"`
	GapHypotheses        json.RawMessage `json:"gapHypotheses"`
	BaselineGenerationID string          `json:"baselineGenerationId"`
}

func New(store *storage.Store) *Service { return &Service{store: store} }

func (s *Service) EnsureSeed(ctx context.Context, userID string) (domain.AgentGeneration, error) {
	generation, err := s.store.StableAgentGeneration(ctx, userID, "general", "")
	if err == nil {
		return generation, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return generation, err
	}
	now := time.Now().UTC()
	spec := domain.AgentSpec{Strategy: StrategyReact, SystemPrompt: DefaultSystemPrompt, MaxSteps: 500}
	definition := domain.AgentDefinition{UserID: userID, APIVersion: APIVersion, Name: "O Seed", Description: "Stable seed ReAct agent definition", Spec: spec, CreatedAt: now}
	definition.Digest = definitionDigest(definition)
	generation = domain.AgentGeneration{ID: newID("gen"), UserID: userID, Number: 1, Scope: "general", Status: "stable", DefinitionDigest: definition.Digest, Definition: definition, Evidence: json.RawMessage(`{"kind":"seed"}`), CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateAgentGeneration(ctx, definition, generation); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return s.store.StableAgentGeneration(ctx, userID, "general", "")
		}
		return domain.AgentGeneration{}, err
	}
	return generation, nil
}

func (s *Service) ListGenerations(ctx context.Context, userID string) ([]domain.AgentGeneration, error) {
	if _, err := s.EnsureSeed(ctx, userID); err != nil {
		return nil, err
	}
	return s.store.ListAgentGenerations(ctx, userID)
}

func (s *Service) Generation(ctx context.Context, userID, id string) (domain.AgentGeneration, error) {
	return s.store.AgentGeneration(ctx, userID, id)
}

func (s *Service) Stable(ctx context.Context, userID string) (domain.AgentGeneration, error) {
	return s.EnsureSeed(ctx, userID)
}

func (s *Service) ConversationGeneration(ctx context.Context, userID, conversationID string) (domain.AgentGeneration, error) {
	generation, err := s.store.ConversationGeneration(ctx, userID, conversationID)
	if err == nil {
		return generation, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return generation, err
	}
	generation, err = s.EnsureSeed(ctx, userID)
	if err != nil {
		return generation, err
	}
	if err := s.store.BindConversationGeneration(ctx, userID, conversationID, generation.ID, generation.DefinitionDigest); err != nil {
		return generation, err
	}
	return generation, nil
}

func (s *Service) BindConversation(ctx context.Context, userID, conversationID, generationID string) (domain.AgentGeneration, error) {
	generation, err := s.store.AgentGeneration(ctx, userID, generationID)
	if err != nil {
		return generation, err
	}
	if err := s.store.BindConversationGeneration(ctx, userID, conversationID, generation.ID, generation.DefinitionDigest); err != nil {
		return generation, err
	}
	return generation, nil
}

func (s *Service) CreateCandidate(ctx context.Context, userID string, input CandidateInput) (domain.AgentGeneration, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if input.Name == "" || input.Description == "" {
		return domain.AgentGeneration{}, domain.ErrInvalid
	}
	var parent domain.AgentGeneration
	var err error
	if input.ParentGenerationID == "" {
		parent, err = s.EnsureSeed(ctx, userID)
	} else {
		parent, err = s.store.AgentGeneration(ctx, userID, input.ParentGenerationID)
	}
	if err != nil {
		return domain.AgentGeneration{}, err
	}
	spec, err := normalizeSpec(input.Spec)
	if err != nil {
		return domain.AgentGeneration{}, err
	}
	scope := strings.TrimSpace(input.Scope)
	if scope == "" {
		scope = parent.Scope
	}
	if scope != "general" && scope != "workspace" && scope != "task" && scope != "specialist" {
		return domain.AgentGeneration{}, domain.ErrInvalid
	}
	scopeKey := strings.TrimSpace(input.ScopeKey)
	if (scope == "workspace" || scope == "task" || scope == "specialist") && scopeKey == "" {
		return domain.AgentGeneration{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	definition := domain.AgentDefinition{UserID: userID, APIVersion: APIVersion, Name: input.Name, Description: input.Description, ParentDigest: parent.DefinitionDigest, Spec: spec, CreatedAt: now}
	definition.Digest = definitionDigest(definition)
	for attempt := 0; attempt < 3; attempt++ {
		number, numberErr := s.store.NextAgentGenerationNumber(ctx, userID)
		if numberErr != nil {
			return domain.AgentGeneration{}, numberErr
		}
		generation := domain.AgentGeneration{ID: newID("gen"), UserID: userID, Number: number, Scope: scope, ScopeKey: scopeKey, Status: "candidate", DefinitionDigest: definition.Digest, Definition: definition, Evidence: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}
		if createErr := s.store.CreateAgentGeneration(ctx, definition, generation); createErr == nil {
			return generation, nil
		} else if !errors.Is(createErr, domain.ErrConflict) {
			return domain.AgentGeneration{}, createErr
		}
	}
	return domain.AgentGeneration{}, domain.ErrConflict
}

func (s *Service) CreateChallenge(ctx context.Context, userID string, input ChallengeInput) (domain.FrontierChallenge, error) {
	input.Title = strings.TrimSpace(input.Title)
	input.Objective = strings.TrimSpace(input.Objective)
	input.SuccessCriteria = strings.TrimSpace(input.SuccessCriteria)
	if input.Title == "" || input.Objective == "" || input.SuccessCriteria == "" {
		return domain.FrontierChallenge{}, domain.ErrInvalid
	}
	baselineID := input.BaselineGenerationID
	if baselineID == "" {
		baseline, err := s.EnsureSeed(ctx, userID)
		if err != nil {
			return domain.FrontierChallenge{}, err
		}
		baselineID = baseline.ID
	} else if _, err := s.store.AgentGeneration(ctx, userID, baselineID); err != nil {
		return domain.FrontierChallenge{}, err
	}
	hypotheses := input.GapHypotheses
	if len(hypotheses) == 0 {
		hypotheses = json.RawMessage(`[]`)
	}
	var decoded any
	if json.Unmarshal(hypotheses, &decoded) != nil {
		return domain.FrontierChallenge{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	challenge := domain.FrontierChallenge{ID: newID("frn"), UserID: userID, Title: input.Title, Objective: input.Objective, FailureEvidence: strings.TrimSpace(input.FailureEvidence), SuccessCriteria: input.SuccessCriteria, GapHypotheses: append(json.RawMessage(nil), hypotheses...), BaselineGenerationID: baselineID, Status: "scoped", CreatedAt: now, UpdatedAt: now}
	return challenge, s.store.CreateFrontierChallenge(ctx, challenge)
}

func (s *Service) Challenge(ctx context.Context, userID, id string) (domain.FrontierChallenge, error) {
	return s.store.FrontierChallenge(ctx, userID, id)
}

func (s *Service) ListChallenges(ctx context.Context, userID string) ([]domain.FrontierChallenge, error) {
	return s.store.ListFrontierChallenges(ctx, userID)
}

func (s *Service) Promote(ctx context.Context, userID, generationID, experimentID string) (domain.AgentGeneration, error) {
	experiment, err := s.store.EvalExperiment(ctx, userID, experimentID)
	if err != nil {
		return domain.AgentGeneration{}, err
	}
	if experiment.CandidateGenerationID != generationID || experiment.Status != "completed" || experiment.Report == nil || experiment.Report.Recommendation != "promote" {
		return domain.AgentGeneration{}, domain.ErrConflict
	}
	evidence, err := json.Marshal(map[string]any{"experimentId": experiment.ID, "report": experiment.Report})
	if err != nil {
		return domain.AgentGeneration{}, err
	}
	if err := s.store.PromoteAgentGeneration(ctx, userID, generationID, evidence); err != nil {
		return domain.AgentGeneration{}, err
	}
	_ = s.store.UpdateFrontierChallengeStatus(ctx, userID, experiment.ChallengeID, "solved")
	return s.store.AgentGeneration(ctx, userID, generationID)
}

func normalizeSpec(spec domain.AgentSpec) (domain.AgentSpec, error) {
	spec.Strategy = strings.TrimSpace(spec.Strategy)
	if spec.Strategy == "" {
		spec.Strategy = StrategyReact
	}
	if spec.Strategy != StrategyReact && spec.Strategy != StrategyPlanReact {
		return domain.AgentSpec{}, fmt.Errorf("%w: unsupported strategy %q", domain.ErrInvalid, spec.Strategy)
	}
	spec.SystemPrompt = strings.TrimSpace(spec.SystemPrompt)
	if spec.SystemPrompt == "" {
		spec.SystemPrompt = DefaultSystemPrompt
	}
	if len(spec.SystemPrompt) > 16000 {
		return domain.AgentSpec{}, domain.ErrInvalid
	}
	if spec.MaxSteps == 0 {
		spec.MaxSteps = 500
	}
	if spec.MaxSteps < 1 || spec.MaxSteps > 1000 {
		return domain.AgentSpec{}, domain.ErrInvalid
	}
	if spec.Strategy == StrategyPlanReact {
		spec.PlannerPrompt = strings.TrimSpace(spec.PlannerPrompt)
		if spec.PlannerPrompt == "" {
			spec.PlannerPrompt = DefaultPlannerPrompt
		}
		if len(spec.PlannerPrompt) > 12000 {
			return domain.AgentSpec{}, domain.ErrInvalid
		}
	} else {
		spec.PlannerPrompt = ""
	}
	return spec, nil
}

func definitionDigest(definition domain.AgentDefinition) string {
	payload := struct {
		APIVersion   string           `json:"apiVersion"`
		Name         string           `json:"name"`
		Description  string           `json:"description"`
		ParentDigest string           `json:"parentDigest,omitempty"`
		Spec         domain.AgentSpec `json:"spec"`
	}{definition.APIVersion, definition.Name, definition.Description, definition.ParentDigest, definition.Spec}
	raw, _ := json.Marshal(payload)
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func newID(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + "_" + hex.EncodeToString(raw)
}
