package evalharness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/storage"
)

type TrialRunner interface {
	RunEvaluation(context.Context, string, string, domain.AgentGeneration, string) (string, domain.RunMetrics, error)
}

type StartInput struct {
	ChallengeID           string            `json:"challengeId"`
	BaselineGenerationID  string            `json:"baselineGenerationId"`
	CandidateGenerationID string            `json:"candidateGenerationId"`
	ProviderID            string            `json:"providerId"`
	Cases                 []domain.EvalCase `json:"cases"`
	Repetitions           int               `json:"repetitions"`
}

type Service struct {
	store     *storage.Store
	evolution *evolution.Service
	runner    TrialRunner
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.Mutex
	running   map[string]bool
}

func New(store *storage.Store, evolutionService *evolution.Service, runner TrialRunner) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{store: store, evolution: evolutionService, runner: runner, ctx: ctx, cancel: cancel, running: map[string]bool{}}
}

func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Service) Start(ctx context.Context, userID string, input StartInput) (domain.EvalExperiment, error) {
	challenge, err := s.evolution.Challenge(ctx, userID, input.ChallengeID)
	if err != nil {
		return domain.EvalExperiment{}, err
	}
	if input.BaselineGenerationID == "" {
		input.BaselineGenerationID = challenge.BaselineGenerationID
	}
	baseline, err := s.evolution.Generation(ctx, userID, input.BaselineGenerationID)
	if err != nil {
		return domain.EvalExperiment{}, err
	}
	candidate, err := s.evolution.Generation(ctx, userID, input.CandidateGenerationID)
	if err != nil {
		return domain.EvalExperiment{}, err
	}
	if candidate.Status != "candidate" && candidate.Status != "canary" {
		return domain.EvalExperiment{}, domain.ErrConflict
	}
	if baseline.ID == candidate.ID || input.ProviderID == "" {
		return domain.EvalExperiment{}, domain.ErrInvalid
	}
	if _, _, _, err := s.store.ProviderSecret(ctx, userID, input.ProviderID); err != nil {
		return domain.EvalExperiment{}, err
	}
	cases, err := normalizeCases(input.Cases)
	if err != nil {
		return domain.EvalExperiment{}, err
	}
	if input.Repetitions == 0 {
		input.Repetitions = 1
	}
	if input.Repetitions < 1 || input.Repetitions > 5 {
		return domain.EvalExperiment{}, domain.ErrInvalid
	}
	now := time.Now().UTC()
	experiment := domain.EvalExperiment{ID: newID("exp"), UserID: userID, ChallengeID: challenge.ID, BaselineGenerationID: baseline.ID, CandidateGenerationID: candidate.ID, ProviderID: input.ProviderID, Status: "queued", Cases: cases, Repetitions: input.Repetitions, CreatedAt: now, UpdatedAt: now}
	if err := s.store.StartEvalExperiment(ctx, experiment); err != nil {
		return domain.EvalExperiment{}, err
	}
	persisted, err := s.store.EvalExperiment(ctx, userID, experiment.ID)
	if err != nil {
		return domain.EvalExperiment{}, fmt.Errorf("read back started evaluation: %w", err)
	}
	persistedChallenge, err := s.evolution.Challenge(ctx, userID, challenge.ID)
	if err != nil {
		return domain.EvalExperiment{}, fmt.Errorf("read back evaluation challenge: %w", err)
	}
	if persisted.Status != "queued" || persistedChallenge.Status != "evaluating" {
		return domain.EvalExperiment{}, fmt.Errorf("evaluation start did not reach its persisted state")
	}
	s.startWorker(userID, experiment.ID)
	return persisted, nil
}

// Recover resumes queued experiments and interrupted running experiments from
// their durable per-trial records. A trial already committed to SQLite is
// never repeated after restart.
func (s *Service) Recover(ctx context.Context, userID string) error {
	experiments, err := s.store.ListEvalExperiments(ctx, userID)
	if err != nil {
		return err
	}
	for _, experiment := range experiments {
		if experiment.Status != "queued" && experiment.Status != "running" {
			continue
		}
		if experiment.Status == "running" {
			if err := s.store.UpdateEvalExperiment(ctx, userID, experiment.ID, "queued", nil, "Host restarted; continuing from committed trials"); err != nil {
				return fmt.Errorf("requeue interrupted evaluation %s: %w", experiment.ID, err)
			}
		}
		s.startWorker(userID, experiment.ID)
	}
	return nil
}

func (s *Service) startWorker(userID, experimentID string) {
	s.mu.Lock()
	if s.running[experimentID] {
		s.mu.Unlock()
		return
	}
	s.running[experimentID] = true
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, experimentID)
			s.mu.Unlock()
		}()
		s.run(s.ctx, userID, experimentID)
	}()
}

func (s *Service) run(ctx context.Context, userID, experimentID string) {
	experiment, err := s.store.EvalExperiment(ctx, userID, experimentID)
	if err != nil {
		slog.Error("load evaluation experiment failed", "experiment_id", experimentID, "error", err)
		return
	}
	if err := s.store.UpdateEvalExperiment(ctx, userID, experimentID, "running", nil, ""); err != nil {
		slog.Error("mark evaluation running failed", "experiment_id", experimentID, "error", err)
		return
	}
	baseline, err := s.evolution.Generation(ctx, userID, experiment.BaselineGenerationID)
	if err != nil {
		s.fail(ctx, userID, experiment, err)
		return
	}
	candidate, err := s.evolution.Generation(ctx, userID, experiment.CandidateGenerationID)
	if err != nil {
		s.fail(ctx, userID, experiment, err)
		return
	}
	priorTrials, err := s.store.EvalTrials(ctx, experiment.ID)
	if err != nil {
		s.fail(ctx, userID, experiment, err)
		return
	}
	committed := make(map[string]bool, len(priorTrials))
	for _, trial := range priorTrials {
		committed[trialKey(trial.CaseID, trial.Side, trial.Repetition)] = true
	}

	for repetition := 1; repetition <= experiment.Repetitions; repetition++ {
		for caseIndex, evalCase := range experiment.Cases {
			sides := []struct {
				name       string
				generation domain.AgentGeneration
			}{{"baseline", baseline}, {"candidate", candidate}}
			if (repetition+caseIndex)%2 == 0 {
				sides[0], sides[1] = sides[1], sides[0]
			}
			for _, side := range sides {
				key := trialKey(evalCase.ID, side.name, repetition)
				if committed[key] {
					continue
				}
				select {
				case <-ctx.Done():
					s.pause(ctx, userID, experiment)
					return
				default:
				}
				response, metrics, runErr := s.runner.RunEvaluation(ctx, userID, experiment.ProviderID, side.generation, evalCase.Prompt)
				if ctx.Err() != nil {
					s.pause(ctx, userID, experiment)
					return
				}
				trial := classifyTrial(experiment.ID, evalCase, side.name, repetition, response, metrics, runErr)
				if err := s.store.AddEvalTrial(ctx, trial); err != nil {
					s.fail(ctx, userID, experiment, err)
					return
				}
				committed[key] = true
			}
		}
	}
	trials, err := s.store.EvalTrials(ctx, experiment.ID)
	if err != nil {
		s.fail(ctx, userID, experiment, err)
		return
	}
	report := compare(trials, evaluationPlan{Cases: experiment.Cases, Repetitions: experiment.Repetitions})
	challengeStatus := "inconclusive"
	switch report.Recommendation {
	case "promote":
		challengeStatus = "candidate_selected"
	case "reject":
		challengeStatus = "candidate_rejected"
	}
	if err := s.store.FinishEvalExperiment(ctx, userID, experiment.ID, "completed", challengeStatus, "", &report); err != nil {
		s.fail(ctx, userID, experiment, fmt.Errorf("persist evaluation report: %w", err))
		return
	}
}

func (s *Service) fail(ctx context.Context, userID string, experiment domain.EvalExperiment, err error) {
	message := "evaluation interrupted"
	if err != nil {
		message = err.Error()
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if writeErr := s.store.FinishEvalExperiment(writeCtx, userID, experiment.ID, "failed", "scoped", message, nil); writeErr != nil {
		slog.Error("persist evaluation failure state failed", "experiment_id", experiment.ID, "cause_class", evaluationErrorClass(err), "error", writeErr)
	}
}

func (s *Service) pause(ctx context.Context, userID string, experiment domain.EvalExperiment) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.UpdateEvalExperiment(writeCtx, userID, experiment.ID, "queued", nil, "Host is stopping; committed trials are preserved for restart"); err != nil {
		slog.Error("persist evaluation pause failed", "experiment_id", experiment.ID, "error", err)
	}
}

func (s *Service) List(ctx context.Context, userID string) ([]domain.EvalExperiment, error) {
	return s.store.ListEvalExperiments(ctx, userID)
}

func (s *Service) Get(ctx context.Context, userID, id string) (domain.EvalExperiment, error) {
	experiment, err := s.store.EvalExperiment(ctx, userID, id)
	if err != nil {
		return experiment, err
	}
	experiment.Trials, err = s.store.EvalTrials(ctx, id)
	return experiment, err
}

func normalizeCases(cases []domain.EvalCase) ([]domain.EvalCase, error) {
	if len(cases) == 0 || len(cases) > 50 {
		return nil, domain.ErrInvalid
	}
	result := make([]domain.EvalCase, len(cases))
	seen := map[string]bool{}
	for index, item := range cases {
		item.Name = strings.TrimSpace(item.Name)
		item.Prompt = strings.TrimSpace(item.Prompt)
		item.Expected = strings.TrimSpace(item.Expected)
		item.Evaluator = strings.TrimSpace(item.Evaluator)
		if item.ID == "" {
			item.ID = fmt.Sprintf("case-%03d", index+1)
		}
		if item.Name == "" {
			item.Name = item.ID
		}
		if item.Prompt == "" || seen[item.ID] {
			return nil, domain.ErrInvalid
		}
		seen[item.ID] = true
		switch item.Evaluator {
		case "nonempty":
		case "contains", "equals", "regex":
			if item.Expected == "" {
				return nil, domain.ErrInvalid
			}
			if item.Evaluator == "regex" {
				if _, err := regexp.Compile(item.Expected); err != nil {
					return nil, fmt.Errorf("%w: invalid evaluator regex", domain.ErrInvalid)
				}
			}
		default:
			return nil, domain.ErrInvalid
		}
		result[index] = item
	}
	return result, nil
}

func evaluate(evalCase domain.EvalCase, response string) bool {
	switch evalCase.Evaluator {
	case "nonempty":
		return strings.TrimSpace(response) != ""
	case "contains":
		return strings.Contains(strings.ToLower(response), strings.ToLower(evalCase.Expected))
	case "equals":
		return strings.EqualFold(strings.TrimSpace(response), strings.TrimSpace(evalCase.Expected))
	case "regex":
		matched, _ := regexp.MatchString(evalCase.Expected, response)
		return matched
	default:
		return false
	}
}

type evaluationPlan struct {
	Cases       []domain.EvalCase
	Repetitions int
}

func compare(trials []domain.EvalTrial, plans ...evaluationPlan) domain.EvalReport {
	report := domain.EvalReport{CompletedAt: time.Now().UTC()}
	type pair struct {
		baseline  *domain.EvalTrial
		candidate *domain.EvalTrial
	}
	pairs := map[string]*pair{}
	report.EvidenceComplete = true
	for index := range trials {
		trial := trials[index]
		key := fmt.Sprintf("%s:%d", trial.CaseID, trial.Repetition)
		if pairs[key] == nil {
			pairs[key] = &pair{}
		}
		if trial.Side == "baseline" {
			pairs[key].baseline = &trial
			accumulate(&report.Baseline, trial)
		} else if trial.Side == "candidate" {
			pairs[key].candidate = &trial
			accumulate(&report.Candidate, trial)
		}
		if !trialEvidenceComplete(trial) {
			report.IncompleteTrials++
		}
		if trial.FailureClass == "infrastructure" || (trial.Status == "failed" && trial.FailureClass == "") || (trial.Error != "" && trial.FailureClass == "") {
			report.InfrastructureErrors++
		}
	}
	finalizeSummary(&report.Baseline)
	finalizeSummary(&report.Candidate)
	for _, item := range pairs {
		if item.baseline == nil || item.candidate == nil {
			continue
		}
		report.PairedCases++
		if !trialEvidenceComplete(*item.baseline) || !trialEvidenceComplete(*item.candidate) {
			report.EvidenceComplete = false
		}
		if !item.baseline.Success && item.candidate.Success {
			report.FrontierWins++
		}
		if item.baseline.Success && !item.candidate.Success {
			report.Regressions++
		}
	}
	if len(plans) > 0 {
		plan := plans[0]
		for _, evalCase := range plan.Cases {
			for repetition := 1; repetition <= plan.Repetitions; repetition++ {
				item := pairs[fmt.Sprintf("%s:%d", evalCase.ID, repetition)]
				if item == nil || item.baseline == nil || item.candidate == nil {
					report.EvidenceComplete = false
					if item == nil {
						report.IncompleteTrials += 2
					} else {
						if item.baseline == nil {
							report.IncompleteTrials++
						}
						if item.candidate == nil {
							report.IncompleteTrials++
						}
					}
				}
			}
		}
	}
	if report.InfrastructureErrors > 0 || report.IncompleteTrials > 0 {
		report.EvidenceComplete = false
	}
	if !report.EvidenceComplete {
		report.Recommendation = "inconclusive"
		report.RecommendationCause = "evaluation evidence is incomplete or contains infrastructure failures"
		return report
	}
	switch {
	case report.FrontierWins > 0 && report.Regressions == 0 && report.Candidate.Successes > report.Baseline.Successes:
		report.Recommendation = "promote"
		report.RecommendationCause = "candidate expands the measured frontier without a paired regression"
	case report.Candidate.Successes == report.Baseline.Successes && report.Candidate.Successes > 0 && report.Regressions == 0 && report.Candidate.TotalTokens > 0 && float64(report.Candidate.TotalTokens) <= float64(report.Baseline.TotalTokens)*0.85:
		report.Recommendation = "promote"
		report.RecommendationCause = "candidate preserves measured capability while reducing token use by at least 15%"
	case report.Regressions > report.FrontierWins || report.Candidate.Successes < report.Baseline.Successes:
		report.Recommendation = "reject"
		report.RecommendationCause = "candidate introduces more measured regressions than frontier wins"
	default:
		report.Recommendation = "inconclusive"
		report.RecommendationCause = "the paired evidence is insufficient for promotion"
	}
	return report
}

func trialEvidenceComplete(trial domain.EvalTrial) bool {
	if trial.Error != "" {
		return false
	}
	return trial.Status == "" || trial.Status == "completed"
}

func classifyTrial(experimentID string, evalCase domain.EvalCase, side string, repetition int, response string, metrics domain.RunMetrics, runErr error) domain.EvalTrial {
	trial := domain.EvalTrial{ID: newID("trial"), ExperimentID: experimentID, CaseID: evalCase.ID, Side: side, Repetition: repetition, Response: response, Metrics: metrics, CreatedAt: time.Now().UTC(), Status: "completed"}
	switch {
	case runErr != nil:
		trial.Status = "failed"
		trial.FailureClass = "infrastructure"
		if errors.Is(runErr, context.DeadlineExceeded) {
			trial.Status = "incomplete"
			trial.FailureClass = "time_limit"
		}
		trial.Error = "Trial ended with " + evaluationErrorClass(runErr)
	case metrics.ReachedStepLimit:
		trial.Status, trial.FailureClass, trial.Error = "incomplete", "step_limit", "Agent step limit reached"
	case metrics.ReachedTokenLimit:
		trial.Status, trial.FailureClass, trial.Error = "incomplete", "token_limit", "Agent token budget reached"
	case metrics.ReachedModelCallLimit:
		trial.Status, trial.FailureClass, trial.Error = "incomplete", "model_call_limit", "Agent model-call budget reached"
	default:
		trial.Success = evaluate(evalCase, response)
		if !trial.Success {
			trial.FailureClass = "task"
		}
	}
	return trial
}

func trialKey(caseID, side string, repetition int) string {
	return fmt.Sprintf("%s:%s:%d", caseID, side, repetition)
}

func evaluationErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "time_limit"
	default:
		return "evaluation_error"
	}
}

func accumulate(summary *domain.EvalSideSummary, trial domain.EvalTrial) {
	summary.Trials++
	if trial.Success {
		summary.Successes++
	}
	summary.TotalTokens += trial.Metrics.TotalTokens
	summary.AverageDurationMS += float64(trial.Metrics.DurationMillis)
	if trial.Metrics.ReachedStepLimit || trial.Metrics.ReachedTokenLimit || trial.Metrics.ReachedModelCallLimit {
		summary.ReachedLimits++
	}
	if trial.Error != "" {
		summary.NonPermissionError++
	}
}

func finalizeSummary(summary *domain.EvalSideSummary) {
	if summary.Trials == 0 {
		return
	}
	summary.SuccessRate = float64(summary.Successes) / float64(summary.Trials)
	summary.AverageTokens = float64(summary.TotalTokens) / float64(summary.Trials)
	summary.AverageDurationMS /= float64(summary.Trials)
}

func newID(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + "_" + hex.EncodeToString(raw)
}
