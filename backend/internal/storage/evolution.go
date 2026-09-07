package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) CreateAgentGeneration(ctx context.Context, definition domain.AgentDefinition, generation domain.AgentGeneration) error {
	spec, err := json.Marshal(definition.Spec)
	if err != nil {
		return err
	}
	evidence := []byte(generation.Evidence)
	if len(evidence) == 0 {
		evidence = []byte(`{}`)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_definitions(user_id,digest,api_version,name,description,parent_digest,spec_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, definition.UserID, definition.Digest, definition.APIVersion, definition.Name, definition.Description, definition.ParentDigest, spec, definition.CreatedAt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_generations(id,user_id,generation_number,scope,scope_key,status,definition_digest,evidence_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, generation.ID, generation.UserID, generation.Number, generation.Scope, generation.ScopeKey, generation.Status, generation.DefinitionDigest, evidence, generation.CreatedAt, generation.UpdatedAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return domain.ErrConflict
		}
		return err
	}
	return tx.Commit()
}

const generationColumns = `g.id,g.user_id,g.generation_number,g.scope,g.scope_key,g.status,g.definition_digest,g.evidence_json,g.created_at,g.updated_at,
d.digest,d.user_id,d.api_version,d.name,d.description,d.parent_digest,d.spec_json,d.created_at`

func scanGeneration(scanner rowScanner) (domain.AgentGeneration, error) {
	var generation domain.AgentGeneration
	var evidence, spec []byte
	err := scanner.Scan(
		&generation.ID, &generation.UserID, &generation.Number, &generation.Scope, &generation.ScopeKey,
		&generation.Status, &generation.DefinitionDigest, &evidence, &generation.CreatedAt, &generation.UpdatedAt,
		&generation.Definition.Digest, &generation.Definition.UserID, &generation.Definition.APIVersion,
		&generation.Definition.Name, &generation.Definition.Description, &generation.Definition.ParentDigest,
		&spec, &generation.Definition.CreatedAt,
	)
	if err != nil {
		return generation, err
	}
	if err := json.Unmarshal(spec, &generation.Definition.Spec); err != nil {
		return generation, err
	}
	generation.Evidence = append(json.RawMessage(nil), evidence...)
	return generation, nil
}

func (s *Store) AgentGeneration(ctx context.Context, userID, id string) (domain.AgentGeneration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+generationColumns+` FROM agent_generations g JOIN agent_definitions d ON d.user_id=g.user_id AND d.digest=g.definition_digest WHERE g.id=? AND g.user_id=?`, id, userID)
	generation, err := scanGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return generation, domain.ErrNotFound
	}
	return generation, err
}

func (s *Store) AgentGenerationByDigest(ctx context.Context, userID, digest string) (domain.AgentGeneration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+generationColumns+` FROM agent_generations g JOIN agent_definitions d ON d.user_id=g.user_id AND d.digest=g.definition_digest WHERE g.definition_digest=? AND g.user_id=? ORDER BY g.generation_number DESC LIMIT 1`, digest, userID)
	generation, err := scanGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return generation, domain.ErrNotFound
	}
	return generation, err
}

func (s *Store) StableAgentGeneration(ctx context.Context, userID, scope, scopeKey string) (domain.AgentGeneration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+generationColumns+` FROM agent_generations g JOIN agent_definitions d ON d.user_id=g.user_id AND d.digest=g.definition_digest WHERE g.user_id=? AND g.scope=? AND g.scope_key=? AND g.status='stable' ORDER BY g.generation_number DESC LIMIT 1`, userID, scope, scopeKey)
	generation, err := scanGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return generation, domain.ErrNotFound
	}
	return generation, err
}

func (s *Store) ListAgentGenerations(ctx context.Context, userID string) ([]domain.AgentGeneration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+generationColumns+` FROM agent_generations g JOIN agent_definitions d ON d.user_id=g.user_id AND d.digest=g.definition_digest WHERE g.user_id=? ORDER BY g.generation_number DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.AgentGeneration{}
	for rows.Next() {
		generation, err := scanGeneration(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, generation)
	}
	return result, rows.Err()
}

func (s *Store) NextAgentGenerationNumber(ctx context.Context, userID string) (int, error) {
	var number int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation_number),0)+1 FROM agent_generations WHERE user_id=?`, userID).Scan(&number)
	return number, err
}

func (s *Store) BindConversationGeneration(ctx context.Context, userID, conversationID, generationID, digest string) error {
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_agent_bindings(conversation_id,user_id,generation_id,definition_digest,bound_at)
SELECT c.id,c.user_id,g.id,g.definition_digest,? FROM conversations c JOIN agent_generations g ON g.id=? AND g.user_id=c.user_id
WHERE c.id=? AND c.user_id=? AND g.definition_digest=?`, time.Now().UTC(), generationID, conversationID, userID, digest)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows > 0 {
		return nil
	}
	var existingGeneration, existingDigest string
	err = s.db.QueryRowContext(ctx, `SELECT generation_id,definition_digest FROM conversation_agent_bindings WHERE conversation_id=? AND user_id=?`, conversationID, userID).Scan(&existingGeneration, &existingDigest)
	if err == nil && existingGeneration == generationID && existingDigest == digest {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err == nil {
		return domain.ErrConflict
	}
	return err
}

func (s *Store) ConversationGeneration(ctx context.Context, userID, conversationID string) (domain.AgentGeneration, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+generationColumns+` FROM conversation_agent_bindings b JOIN agent_generations g ON g.id=b.generation_id AND g.user_id=b.user_id JOIN agent_definitions d ON d.user_id=g.user_id AND d.digest=g.definition_digest WHERE b.conversation_id=? AND b.user_id=?`, conversationID, userID)
	generation, err := scanGeneration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return generation, domain.ErrNotFound
	}
	return generation, err
}

func (s *Store) PromoteAgentGeneration(ctx context.Context, userID, generationID string, evidence json.RawMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var scope, scopeKey, status string
	err = tx.QueryRowContext(ctx, `SELECT scope,scope_key,status FROM agent_generations WHERE id=? AND user_id=?`, generationID, userID).Scan(&scope, &scopeKey, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "candidate" && status != "canary" {
		return domain.ErrConflict
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE agent_generations SET status='superseded',updated_at=? WHERE user_id=? AND scope=? AND scope_key=? AND status='stable' AND id<>?`, now, userID, scope, scopeKey, generationID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_generations SET status='stable',evidence_json=?,updated_at=? WHERE id=? AND user_id=?`, []byte(evidence), now, generationID, userID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return domain.ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) CreateFrontierChallenge(ctx context.Context, challenge domain.FrontierChallenge) error {
	hypotheses := []byte(challenge.GapHypotheses)
	if len(hypotheses) == 0 {
		hypotheses = []byte(`[]`)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO frontier_challenges(id,user_id,title,objective,failure_evidence,success_criteria,gap_hypotheses_json,baseline_generation_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, challenge.ID, challenge.UserID, challenge.Title, challenge.Objective, challenge.FailureEvidence, challenge.SuccessCriteria, hypotheses, challenge.BaselineGenerationID, challenge.Status, challenge.CreatedAt, challenge.UpdatedAt)
	return err
}

func scanChallenge(scanner rowScanner) (domain.FrontierChallenge, error) {
	var challenge domain.FrontierChallenge
	var hypotheses []byte
	err := scanner.Scan(&challenge.ID, &challenge.UserID, &challenge.Title, &challenge.Objective, &challenge.FailureEvidence, &challenge.SuccessCriteria, &hypotheses, &challenge.BaselineGenerationID, &challenge.Status, &challenge.CreatedAt, &challenge.UpdatedAt)
	challenge.GapHypotheses = append(json.RawMessage(nil), hypotheses...)
	return challenge, err
}

func (s *Store) FrontierChallenge(ctx context.Context, userID, id string) (domain.FrontierChallenge, error) {
	challenge, err := scanChallenge(s.db.QueryRowContext(ctx, `SELECT id,user_id,title,objective,failure_evidence,success_criteria,gap_hypotheses_json,baseline_generation_id,status,created_at,updated_at FROM frontier_challenges WHERE id=? AND user_id=?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return challenge, domain.ErrNotFound
	}
	return challenge, err
}

func (s *Store) ListFrontierChallenges(ctx context.Context, userID string) ([]domain.FrontierChallenge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,title,objective,failure_evidence,success_criteria,gap_hypotheses_json,baseline_generation_id,status,created_at,updated_at FROM frontier_challenges WHERE user_id=? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.FrontierChallenge{}
	for rows.Next() {
		challenge, err := scanChallenge(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, challenge)
	}
	return result, rows.Err()
}

func (s *Store) UpdateFrontierChallengeStatus(ctx context.Context, userID, id, status string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE frontier_challenges SET status=?,updated_at=? WHERE id=? AND user_id=?`, status, time.Now().UTC(), id, userID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) CreateEvalExperiment(ctx context.Context, experiment domain.EvalExperiment) error {
	cases, err := json.Marshal(experiment.Cases)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO eval_experiments(id,user_id,challenge_id,baseline_generation_id,candidate_generation_id,provider_id,status,cases_json,repetitions,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, experiment.ID, experiment.UserID, experiment.ChallengeID, experiment.BaselineGenerationID, experiment.CandidateGenerationID, experiment.ProviderID, experiment.Status, cases, experiment.Repetitions, experiment.CreatedAt, experiment.UpdatedAt)
	return err
}

func scanExperiment(scanner rowScanner) (domain.EvalExperiment, error) {
	var experiment domain.EvalExperiment
	var cases, report []byte
	err := scanner.Scan(&experiment.ID, &experiment.UserID, &experiment.ChallengeID, &experiment.BaselineGenerationID, &experiment.CandidateGenerationID, &experiment.ProviderID, &experiment.Status, &cases, &experiment.Repetitions, &report, &experiment.LastError, &experiment.CreatedAt, &experiment.UpdatedAt)
	if err != nil {
		return experiment, err
	}
	if err := json.Unmarshal(cases, &experiment.Cases); err != nil {
		return experiment, err
	}
	if len(report) > 0 {
		var value domain.EvalReport
		if err := json.Unmarshal(report, &value); err != nil {
			return experiment, err
		}
		experiment.Report = &value
	}
	return experiment, nil
}

const experimentColumns = `id,user_id,challenge_id,baseline_generation_id,candidate_generation_id,provider_id,status,cases_json,repetitions,report_json,last_error,created_at,updated_at`

func (s *Store) EvalExperiment(ctx context.Context, userID, id string) (domain.EvalExperiment, error) {
	experiment, err := scanExperiment(s.db.QueryRowContext(ctx, `SELECT `+experimentColumns+` FROM eval_experiments WHERE id=? AND user_id=?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return experiment, domain.ErrNotFound
	}
	return experiment, err
}

func (s *Store) ListEvalExperiments(ctx context.Context, userID string) ([]domain.EvalExperiment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+experimentColumns+` FROM eval_experiments WHERE user_id=? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.EvalExperiment{}
	for rows.Next() {
		experiment, err := scanExperiment(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, experiment)
	}
	return result, rows.Err()
}

func (s *Store) UpdateEvalExperiment(ctx context.Context, userID, id, status string, report *domain.EvalReport, lastError string) error {
	var reportJSON []byte
	var err error
	if report != nil {
		reportJSON, err = json.Marshal(report)
		if err != nil {
			return err
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE eval_experiments SET status=?,report_json=?,last_error=?,updated_at=? WHERE id=? AND user_id=?`, status, nullableBytes(reportJSON), lastError, time.Now().UTC(), id, userID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func (s *Store) AddEvalTrial(ctx context.Context, trial domain.EvalTrial) error {
	metrics, err := json.Marshal(trial.Metrics)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO eval_trials(id,experiment_id,case_id,side,repetition,success,response,error,metrics_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, trial.ID, trial.ExperimentID, trial.CaseID, trial.Side, trial.Repetition, trial.Success, trial.Response, trial.Error, metrics, trial.CreatedAt)
	return err
}

func (s *Store) EvalTrials(ctx context.Context, experimentID string) ([]domain.EvalTrial, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,experiment_id,case_id,side,repetition,success,response,error,metrics_json,created_at FROM eval_trials WHERE experiment_id=? ORDER BY case_id,repetition,side`, experimentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.EvalTrial{}
	for rows.Next() {
		var trial domain.EvalTrial
		var success int
		var metrics []byte
		if err := rows.Scan(&trial.ID, &trial.ExperimentID, &trial.CaseID, &trial.Side, &trial.Repetition, &success, &trial.Response, &trial.Error, &metrics, &trial.CreatedAt); err != nil {
			return nil, err
		}
		trial.Success = success != 0
		if err := json.Unmarshal(metrics, &trial.Metrics); err != nil {
			return nil, err
		}
		result = append(result, trial)
	}
	return result, rows.Err()
}
