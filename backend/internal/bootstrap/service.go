package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/provider"
)

type Model interface {
	Complete(context.Context, string, string, []provider.ChatMessage) (string, error)
}

type Service struct {
	evolution *evolution.Service
	model     Model
}

type GenerateInput struct {
	ProviderID string `json:"providerId"`
	Count      int    `json:"count"`
}

type proposalEnvelope struct {
	Candidates []proposal `json:"candidates"`
}

type proposal struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Strategy      string `json:"strategy"`
	SystemPrompt  string `json:"systemPrompt"`
	PlannerPrompt string `json:"plannerPrompt"`
	MaxSteps      int    `json:"maxSteps"`
}

func New(evolutionService *evolution.Service, model Model) *Service {
	return &Service{evolution: evolutionService, model: model}
}

// GenerateCandidates asks a model to propose immutable Agent Definitions. It does
// not install, promote, or run them; that authority remains behind the eval gate.
func (s *Service) GenerateCandidates(ctx context.Context, userID, challengeID string, input GenerateInput) ([]domain.AgentGeneration, error) {
	if strings.TrimSpace(input.ProviderID) == "" {
		return nil, domain.ErrInvalid
	}
	if input.Count == 0 {
		input.Count = 2
	}
	if input.Count < 1 || input.Count > 4 {
		return nil, domain.ErrInvalid
	}
	challenge, err := s.evolution.Challenge(ctx, userID, challengeID)
	if err != nil {
		return nil, err
	}
	baseline, err := s.evolution.Generation(ctx, userID, challenge.BaselineGenerationID)
	if err != nil {
		return nil, err
	}
	prompt := proposalPrompt(challenge, baseline, input.Count)
	raw, err := s.model.Complete(ctx, userID, input.ProviderID, []provider.ChatMessage{
		{Role: "system", Content: "You are an Agent architecture optimizer. Return only the requested JSON object. Propose bounded, testable changes; never claim a candidate has passed evaluation."},
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return nil, fmt.Errorf("generate agent candidates: %w", err)
	}
	var envelope proposalEnvelope
	if err := json.Unmarshal(extractJSONObject(raw), &envelope); err != nil {
		return nil, fmt.Errorf("generate agent candidates: invalid model JSON: %w", err)
	}
	if len(envelope.Candidates) == 0 || len(envelope.Candidates) > input.Count {
		return nil, fmt.Errorf("%w: model returned %d candidates", domain.ErrInvalid, len(envelope.Candidates))
	}
	created := make([]domain.AgentGeneration, 0, len(envelope.Candidates))
	for _, item := range envelope.Candidates {
		generation, err := s.evolution.CreateCandidate(ctx, userID, evolution.CandidateInput{
			Name:               item.Name,
			Description:        item.Description,
			ParentGenerationID: baseline.ID,
			Scope:              baseline.Scope,
			ScopeKey:           baseline.ScopeKey,
			Spec: domain.AgentSpec{
				Strategy:      item.Strategy,
				SystemPrompt:  item.SystemPrompt,
				PlannerPrompt: item.PlannerPrompt,
				MaxSteps:      item.MaxSteps,
			},
		})
		if err != nil {
			return created, fmt.Errorf("persist agent candidate %q: %w", item.Name, err)
		}
		created = append(created, generation)
	}
	return created, nil
}

func proposalPrompt(challenge domain.FrontierChallenge, baseline domain.AgentGeneration, count int) string {
	baselineJSON, _ := json.MarshalIndent(baseline.Definition, "", "  ")
	return fmt.Sprintf(`Frontier challenge:
Title: %s
Objective: %s
Observed failure: %s
Success criteria: %s
Gap hypotheses: %s

Baseline immutable Agent Definition:
%s

Propose at most %d diverse, minimal candidates. Supported strategies are exactly "react.v1" and "plan-react.v1". Keep maxSteps between 1 and 64. A candidate may change prompts, loop strategy, and step budget, but must remain provider-neutral and must not weaken tool-result truthfulness or user authority.

Return this exact JSON shape:
{"candidates":[{"name":"...","description":"what changed and why it may address the measured gap","strategy":"react.v1|plan-react.v1","systemPrompt":"...","plannerPrompt":"... or empty for react.v1","maxSteps":12}]}`,
		challenge.Title, challenge.Objective, challenge.FailureEvidence, challenge.SuccessCriteria, string(challenge.GapHypotheses), string(baselineJSON), count)
}

func extractJSONObject(raw string) []byte {
	value := strings.TrimSpace(raw)
	if strings.HasPrefix(value, "```") {
		value = strings.TrimPrefix(value, "```json")
		value = strings.TrimPrefix(value, "```JSON")
		value = strings.TrimPrefix(value, "```")
		value = strings.TrimSuffix(strings.TrimSpace(value), "```")
	}
	start := strings.IndexByte(value, '{')
	end := strings.LastIndexByte(value, '}')
	if start >= 0 && end >= start {
		value = value[start : end+1]
	}
	return []byte(value)
}
