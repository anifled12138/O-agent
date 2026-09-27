package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "axiom.db")
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) RuntimeSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	return value, err
}

func (s *Store) SetRuntimeSetting(ctx context.Context, key, value string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO runtime_settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
		return err
	}
	var persisted string
	if err = tx.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key=?`, key).Scan(&persisted); err != nil {
		return err
	}
	if persisted != value {
		return fmt.Errorf("runtime setting read-back mismatch for %q", key)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	readBack, err := s.RuntimeSetting(ctx, key)
	if err != nil {
		return err
	}
	if readBack != value {
		return fmt.Errorf("runtime setting read-back mismatch for %q after commit", key)
	}
	return nil
}

func (s *Store) DeleteRuntimeSetting(ctx context.Context, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM runtime_settings WHERE key=?`, key); err != nil {
		return err
	}
	var persisted string
	err = tx.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key=?`, key).Scan(&persisted)
	if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return err
		}
		return fmt.Errorf("runtime setting %q still exists after deletion", key)
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_, err = s.RuntimeSetting(ctx, key)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("runtime setting %q still exists after commit", key)
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
 id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL,
 password_hash TEXT NOT NULL, created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS runtime_settings (
 key TEXT PRIMARY KEY, value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS providers (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), name TEXT NOT NULL,
 kind TEXT NOT NULL, base_url TEXT NOT NULL, model TEXT NOT NULL, context_window INTEGER NOT NULL DEFAULT 0,
 api_key_cipher BLOB NOT NULL, api_key_nonce BLOB NOT NULL,
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
 UNIQUE(user_id, name)
);
CREATE TABLE IF NOT EXISTS projects (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), name TEXT NOT NULL,
 instructions TEXT NOT NULL DEFAULT '', instructions_enabled INTEGER NOT NULL DEFAULT 0,
 workdir TEXT NOT NULL DEFAULT '', remote_repo_url TEXT NOT NULL DEFAULT '',
 remote_branch TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_projects_user ON projects(user_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS conversations (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), title TEXT NOT NULL,
 provider_id TEXT NOT NULL REFERENCES providers(id), project_id TEXT NOT NULL DEFAULT '',
 permission_profile TEXT NOT NULL DEFAULT 'workspace_autonomous',
 parent_conversation_id TEXT NOT NULL DEFAULT '', branch_from_message_id TEXT NOT NULL DEFAULT '',
 execution_paused INTEGER NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversations_user ON conversations(user_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS messages (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id),
 role TEXT NOT NULL, content TEXT NOT NULL, created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, created_at);
CREATE TABLE IF NOT EXISTS agent_trace_events (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id), turn_id TEXT NOT NULL,
 sequence INTEGER NOT NULL, kind TEXT NOT NULL, details_json BLOB NOT NULL, created_at DATETIME NOT NULL,
 UNIQUE(turn_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_agent_trace_conversation ON agent_trace_events(conversation_id, created_at);
CREATE TABLE IF NOT EXISTS agent_turns (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id),
 user_id TEXT NOT NULL REFERENCES users(id), input_message_id TEXT NOT NULL REFERENCES messages(id),
 result_message_id TEXT REFERENCES messages(id), provider_id TEXT NOT NULL REFERENCES providers(id),
 generation_id TEXT NOT NULL, definition_digest TEXT NOT NULL,
 permission_profile TEXT NOT NULL DEFAULT 'workspace_autonomous',
 retry_of_turn_id TEXT NOT NULL DEFAULT '', input_content_snapshot TEXT NOT NULL DEFAULT '', inbox_id TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL, stop_reason TEXT NOT NULL DEFAULT '', recovery_class TEXT NOT NULL DEFAULT '',
 cancel_requested INTEGER NOT NULL DEFAULT 0, last_sequence INTEGER NOT NULL DEFAULT 0,
 started_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, completed_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_agent_turns_conversation ON agent_turns(conversation_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_turns_user_started ON agent_turns(user_id, started_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_turns_one_active ON agent_turns(conversation_id)
 WHERE status IN ('running','cancelling','awaiting_approval');
CREATE TABLE IF NOT EXISTS agent_approvals (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id), turn_id TEXT NOT NULL REFERENCES agent_turns(id),
 user_id TEXT NOT NULL REFERENCES users(id), tool_call_id TEXT NOT NULL, tool_name TEXT NOT NULL, source TEXT NOT NULL,
 effect TEXT NOT NULL, permission_profile TEXT NOT NULL, plugin_id TEXT NOT NULL DEFAULT '', release_id TEXT NOT NULL DEFAULT '', resource TEXT NOT NULL DEFAULT '', impact TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL, arguments_preview TEXT NOT NULL,
 arguments_sha256 TEXT NOT NULL, status TEXT NOT NULL, decision TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL, expires_at DATETIME NOT NULL, resolved_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_agent_approvals_pending ON agent_approvals(user_id,conversation_id,status,created_at);
CREATE INDEX IF NOT EXISTS idx_agent_approvals_user_created ON agent_approvals(user_id,created_at,tool_name);
CREATE TABLE IF NOT EXISTS message_revisions (
 id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES messages(id),
 prior_content TEXT NOT NULL, revised_content TEXT NOT NULL,
 revised_by_turn_id TEXT NOT NULL REFERENCES agent_turns(id), revised_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_message_revisions_message ON message_revisions(message_id,revised_at);
CREATE TABLE IF NOT EXISTS agent_inbox (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id),
 user_id TEXT NOT NULL REFERENCES users(id), content TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'queued', turn_id TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_inbox_queue ON agent_inbox(user_id,conversation_id,status,created_at);
CREATE TABLE IF NOT EXISTS agent_turn_reconciliations (
 id TEXT PRIMARY KEY, turn_id TEXT NOT NULL UNIQUE REFERENCES agent_turns(id),
 user_id TEXT NOT NULL REFERENCES users(id), conversation_id TEXT NOT NULL REFERENCES conversations(id),
 decision TEXT NOT NULL, note TEXT NOT NULL, created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_turn_reconciliations_user ON agent_turn_reconciliations(user_id,conversation_id,created_at);
CREATE TABLE IF NOT EXISTS conversation_lifecycle_events (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id),
 kind TEXT NOT NULL, details_json BLOB NOT NULL DEFAULT '{}', created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversation_lifecycle_events ON conversation_lifecycle_events(conversation_id,created_at);
CREATE TABLE IF NOT EXISTS agent_steps (
 id TEXT PRIMARY KEY, turn_id TEXT NOT NULL REFERENCES agent_turns(id), ordinal INTEGER NOT NULL,
 status TEXT NOT NULL, attempt_count INTEGER NOT NULL DEFAULT 0,
 started_at DATETIME NOT NULL, completed_at DATETIME,
 UNIQUE(turn_id, ordinal)
);
CREATE TABLE IF NOT EXISTS agent_model_attempts (
 id TEXT PRIMARY KEY, turn_id TEXT NOT NULL REFERENCES agent_turns(id),
 step_id TEXT NOT NULL REFERENCES agent_steps(id), ordinal INTEGER NOT NULL,
 status TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', usage_json BLOB NOT NULL DEFAULT '{}',
 error_class TEXT NOT NULL DEFAULT '', started_at DATETIME NOT NULL, completed_at DATETIME,
 UNIQUE(step_id, ordinal)
);
CREATE TABLE IF NOT EXISTS agent_turn_checkpoints (
 turn_id TEXT PRIMARY KEY REFERENCES agent_turns(id) ON DELETE CASCADE,
 checkpoint_version INTEGER NOT NULL,
 state_cipher BLOB NOT NULL,
 state_nonce BLOB NOT NULL,
 resume_allowed INTEGER NOT NULL DEFAULT 0,
 event_sequence INTEGER NOT NULL,
 updated_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_definitions (
 user_id TEXT NOT NULL REFERENCES users(id), digest TEXT NOT NULL,
 api_version TEXT NOT NULL, name TEXT NOT NULL, description TEXT NOT NULL,
 parent_digest TEXT NOT NULL DEFAULT '', spec_json BLOB NOT NULL, created_at DATETIME NOT NULL,
 PRIMARY KEY(user_id, digest)
);
CREATE TABLE IF NOT EXISTS agent_generations (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), generation_number INTEGER NOT NULL,
 scope TEXT NOT NULL, scope_key TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
 definition_digest TEXT NOT NULL, evidence_json BLOB NOT NULL DEFAULT '{}',
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
 UNIQUE(user_id, generation_number),
 FOREIGN KEY(user_id, definition_digest) REFERENCES agent_definitions(user_id, digest)
);
CREATE INDEX IF NOT EXISTS idx_agent_generations_user ON agent_generations(user_id, status, generation_number DESC);
CREATE TABLE IF NOT EXISTS conversation_agent_bindings (
 conversation_id TEXT PRIMARY KEY REFERENCES conversations(id),
 user_id TEXT NOT NULL REFERENCES users(id), generation_id TEXT NOT NULL REFERENCES agent_generations(id),
 definition_digest TEXT NOT NULL, bound_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS frontier_challenges (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), title TEXT NOT NULL,
 objective TEXT NOT NULL, failure_evidence TEXT NOT NULL DEFAULT '', success_criteria TEXT NOT NULL,
 gap_hypotheses_json BLOB NOT NULL DEFAULT '[]', baseline_generation_id TEXT NOT NULL REFERENCES agent_generations(id),
 status TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_frontier_challenges_user ON frontier_challenges(user_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS eval_experiments (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), challenge_id TEXT NOT NULL REFERENCES frontier_challenges(id),
 baseline_generation_id TEXT NOT NULL REFERENCES agent_generations(id),
 candidate_generation_id TEXT NOT NULL REFERENCES agent_generations(id), provider_id TEXT NOT NULL REFERENCES providers(id),
 status TEXT NOT NULL, cases_json BLOB NOT NULL, repetitions INTEGER NOT NULL,
 report_json BLOB, last_error TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_eval_experiments_user ON eval_experiments(user_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS eval_trials (
 id TEXT PRIMARY KEY, experiment_id TEXT NOT NULL REFERENCES eval_experiments(id), case_id TEXT NOT NULL,
 side TEXT NOT NULL, repetition INTEGER NOT NULL, success INTEGER NOT NULL,
 status TEXT NOT NULL DEFAULT 'completed', failure_class TEXT NOT NULL DEFAULT '',
 response TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', metrics_json BLOB NOT NULL,
 created_at DATETIME NOT NULL, UNIQUE(experiment_id, case_id, side, repetition)
);
CREATE INDEX IF NOT EXISTS idx_eval_trials_experiment ON eval_trials(experiment_id, created_at);
`
	_, err := s.db.ExecContext(ctx, schema)
	if err != nil {
		return err
	}
	for _, migration := range []string{
		`ALTER TABLE conversations ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE conversations ADD COLUMN permission_profile TEXT NOT NULL DEFAULT 'workspace_autonomous'`,
		`ALTER TABLE conversations ADD COLUMN parent_conversation_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE conversations ADD COLUMN branch_from_message_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE conversations ADD COLUMN execution_paused INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE agent_turns ADD COLUMN permission_profile TEXT NOT NULL DEFAULT 'workspace_autonomous'`,
		`ALTER TABLE agent_turns ADD COLUMN retry_of_turn_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_turns ADD COLUMN input_content_snapshot TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_turns ADD COLUMN inbox_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN instructions_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE projects ADD COLUMN remote_repo_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN remote_branch TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE providers ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE agent_approvals ADD COLUMN permission_profile TEXT NOT NULL DEFAULT 'workspace_autonomous'`,
		`ALTER TABLE agent_approvals ADD COLUMN impact TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_approvals ADD COLUMN plugin_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_approvals ADD COLUMN release_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE eval_trials ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'`,
		`ALTER TABLE eval_trials ADD COLUMN failure_class TEXT NOT NULL DEFAULT ''`,
	} {
		if _, migrationErr := s.db.ExecContext(ctx, migration); migrationErr != nil && !strings.Contains(strings.ToLower(migrationErr.Error()), "duplicate column name") {
			return migrationErr
		}
	}
	if _, err := s.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_agent_turns_one_active;
CREATE UNIQUE INDEX idx_agent_turns_one_active ON agent_turns(conversation_id) WHERE status IN ('running','cancelling','awaiting_approval');`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_turns SET recovery_class=CASE
 WHEN EXISTS (SELECT 1 FROM agent_trace_events e WHERE e.turn_id=agent_turns.id AND e.kind='tool.started') THEN
  CASE WHEN status='completed' THEN 'not_replayable' ELSE 'unknown_external_effect' END
 ELSE 'safe_to_retry' END
WHERE recovery_class='' AND status IN ('completed','failed','cancelled','interrupted','incomplete','needs_reconciliation')`); err != nil {
		return err
	}
	if err := s.reclassifyLegacyReadOnlyTurns(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) CreateUser(ctx context.Context, u domain.User, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id,email,display_name,password_hash,created_at) VALUES(?,?,?,?,?)`, u.ID, strings.ToLower(u.Email), u.DisplayName, passwordHash, u.CreatedAt)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return domain.ErrConflict
	}
	return err
}

// EnsureLocalWorkspaceOwner resolves the durable owner used by the single-user
// local runtime. Existing installations keep the first account they created so
// providers, conversations, plugins, and evaluation history remain visible.
func (s *Store) EnsureLocalWorkspaceOwner(ctx context.Context) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var ownerID string
	err = tx.QueryRowContext(ctx, `SELECT value FROM runtime_settings WHERE key='local_workspace_owner'`).Scan(&ownerID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id FROM users ORDER BY created_at ASC LIMIT 1`).Scan(&ownerID)
		if errors.Is(err, sql.ErrNoRows) {
			ownerID = "local-workspace"
			_, err = tx.ExecContext(ctx, `INSERT INTO users(id,email,display_name,password_hash,created_at) VALUES(?,?,?,?,?)`, ownerID, "local@axiom.invalid", "Local workspace", "", time.Now().UTC())
		}
		if err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO runtime_settings(key,value) VALUES('local_workspace_owner',?)`, ownerID); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return ownerID, nil
}

func (s *Store) UpsertProvider(ctx context.Context, p domain.Provider, cipher, nonce []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO providers(id,user_id,name,kind,base_url,model,context_window,api_key_cipher,api_key_nonce,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, p.ID, p.UserID, p.Name, p.Kind, p.BaseURL, p.Model, p.ContextWindow, cipher, nonce, p.CreatedAt, p.UpdatedAt)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return domain.ErrConflict
	}
	return err
}

func (s *Store) UpdateProvider(ctx context.Context, p domain.Provider, cipher, nonce []byte) error {
	result, err := s.db.ExecContext(ctx, `UPDATE providers SET name=?,kind=?,base_url=?,model=?,context_window=?,api_key_cipher=?,api_key_nonce=?,updated_at=? WHERE id=? AND user_id=?`, p.Name, p.Kind, p.BaseURL, p.Model, p.ContextWindow, cipher, nonce, p.UpdatedAt, p.ID, p.UserID)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) ListProviders(ctx context.Context, userID string) ([]domain.Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,name,kind,base_url,model,context_window,length(api_key_cipher)>0,created_at,updated_at FROM providers WHERE user_id=? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Provider{}
	for rows.Next() {
		var p domain.Provider
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Kind, &p.BaseURL, &p.Model, &p.ContextWindow, &p.HasAPIKey, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) ProviderSecret(ctx context.Context, userID, id string) (domain.Provider, []byte, []byte, error) {
	var p domain.Provider
	var cipher, nonce []byte
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,name,kind,base_url,model,context_window,api_key_cipher,api_key_nonce,created_at,updated_at FROM providers WHERE id=? AND user_id=?`, id, userID).Scan(&p.ID, &p.UserID, &p.Name, &p.Kind, &p.BaseURL, &p.Model, &p.ContextWindow, &cipher, &nonce, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil, nil, domain.ErrNotFound
	}
	p.HasAPIKey = len(cipher) > 0
	return p, cipher, nonce, err
}

func (s *Store) CreateConversation(ctx context.Context, c domain.Conversation) error {
	profile := c.PermissionProfile
	if profile == "" {
		profile = domain.DefaultPermissionProfile()
	} else if !profile.Valid() {
		return domain.ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversations(id,user_id,title,provider_id,project_id,permission_profile,parent_conversation_id,branch_from_message_id,execution_paused,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, c.ID, c.UserID, c.Title, c.ProviderID, c.ProjectID, profile, c.ParentConversationID, c.BranchFromMessageID, c.ExecutionPaused, c.CreatedAt, c.UpdatedAt)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		return domain.ErrInvalid
	}
	return err
}

func (s *Store) CreateConversationWithGeneration(ctx context.Context, c domain.Conversation, generation domain.AgentGeneration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	profile := c.PermissionProfile
	if profile == "" {
		profile = domain.DefaultPermissionProfile()
	} else if !profile.Valid() {
		return domain.ErrInvalid
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversations(id,user_id,title,provider_id,project_id,permission_profile,parent_conversation_id,branch_from_message_id,execution_paused,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, c.ID, c.UserID, c.Title, c.ProviderID, c.ProjectID, profile, c.ParentConversationID, c.BranchFromMessageID, c.ExecutionPaused, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			return domain.ErrInvalid
		}
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO conversation_agent_bindings(conversation_id,user_id,generation_id,definition_digest,bound_at)
SELECT ?,?,?,?,? FROM agent_generations WHERE id=? AND user_id=? AND definition_digest=?`, c.ID, c.UserID, generation.ID, generation.DefinitionDigest, time.Now().UTC(), generation.ID, c.UserID, generation.DefinitionDigest)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return domain.ErrInvalid
	}
	return tx.Commit()
}

func (s *Store) ListConversations(ctx context.Context, userID string) ([]domain.Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.user_id,c.title,c.provider_id,COALESCE(b.generation_id,''),COALESCE(b.definition_digest,''),COALESCE(c.project_id,''),c.permission_profile,COALESCE(c.parent_conversation_id,''),COALESCE(c.branch_from_message_id,''),c.execution_paused,c.created_at,c.updated_at FROM conversations c LEFT JOIN conversation_agent_bindings b ON b.conversation_id=c.id WHERE c.user_id=? ORDER BY c.updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Conversation{}
	for rows.Next() {
		var c domain.Conversation
		if err := rows.Scan(&c.ID, &c.UserID, &c.Title, &c.ProviderID, &c.AgentGenerationID, &c.AgentDefinitionDigest, &c.ProjectID, &c.PermissionProfile, &c.ParentConversationID, &c.BranchFromMessageID, &c.ExecutionPaused, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) UpdateConversationPermissionProfile(ctx context.Context, userID, id string, profile domain.PermissionProfile) error {
	if !profile.Valid() {
		return domain.ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `UPDATE conversations SET permission_profile=?,updated_at=? WHERE id=? AND user_id=?`, profile, time.Now().UTC(), id, userID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) UpdateConversationTitle(ctx context.Context, userID, id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return domain.ErrInvalid
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE conversations SET title=?, updated_at=? WHERE id=? AND (user_id=? OR ?='')`, title, now, id, userID, userID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) UpdateConversationProject(ctx context.Context, userID, id, projectID string) error {
	if projectID != "" {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=? AND user_id=?`, projectID, userID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		} else if err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE conversations SET project_id=?, updated_at=? WHERE id=? AND (user_id=? OR ?='')`, projectID, now, id, userID, userID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) CreateProject(ctx context.Context, p domain.Project) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return domain.ErrInvalid
	}
	instrEnabled := 0
	if p.InstructionsEnabled {
		instrEnabled = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO projects(id,user_id,name,instructions,instructions_enabled,workdir,remote_repo_url,remote_branch,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.UserID, p.Name, p.Instructions, instrEnabled, p.Workdir, p.RemoteRepoURL, p.RemoteBranch, p.CreatedAt, p.UpdatedAt)
	return err
}

func (s *Store) ListProjects(ctx context.Context, userID string) ([]domain.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,name,instructions,instructions_enabled,workdir,remote_repo_url,remote_branch,created_at,updated_at FROM projects WHERE user_id=? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Project{}
	for rows.Next() {
		var p domain.Project
		var instrEnabled int
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Instructions, &instrEnabled, &p.Workdir, &p.RemoteRepoURL, &p.RemoteBranch, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.InstructionsEnabled = instrEnabled != 0
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) Project(ctx context.Context, userID, id string) (domain.Project, error) {
	var p domain.Project
	var instrEnabled int
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,name,instructions,instructions_enabled,workdir,remote_repo_url,remote_branch,created_at,updated_at FROM projects WHERE id=? AND user_id=?`, id, userID).
		Scan(&p.ID, &p.UserID, &p.Name, &p.Instructions, &instrEnabled, &p.Workdir, &p.RemoteRepoURL, &p.RemoteBranch, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, domain.ErrNotFound
	}
	p.InstructionsEnabled = instrEnabled != 0
	return p, err
}

func (s *Store) UpdateProject(ctx context.Context, userID string, p domain.Project) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return domain.ErrInvalid
	}
	instrEnabled := 0
	if p.InstructionsEnabled {
		instrEnabled = 1
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE projects SET name=?, instructions=?, instructions_enabled=?, workdir=?, remote_repo_url=?, remote_branch=?, updated_at=? WHERE id=? AND (user_id=? OR ?='')`,
		p.Name, p.Instructions, instrEnabled, p.Workdir, p.RemoteRepoURL, p.RemoteBranch, now, p.ID, userID, userID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteProject(ctx context.Context, userID, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE conversations SET project_id='' WHERE project_id=? AND (user_id=? OR ?='')`, id, userID, userID)
	if err != nil {
		return err
	}
	var remainingConversations int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE project_id=? AND (user_id=? OR ?='')`, id, userID, userID).Scan(&remainingConversations); err != nil {
		return err
	}
	if remainingConversations != 0 {
		return fmt.Errorf("project removal left %d associated conversations", remainingConversations)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=? AND (user_id=? OR ?='')`, id, userID, userID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return domain.ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) Conversation(ctx context.Context, userID, id string) (domain.ConversationDetail, error) {
	var d domain.ConversationDetail
	err := s.db.QueryRowContext(ctx, `SELECT c.id,c.user_id,c.title,c.provider_id,COALESCE(b.generation_id,''),COALESCE(b.definition_digest,''),COALESCE(c.project_id,''),c.permission_profile,COALESCE(c.parent_conversation_id,''),COALESCE(c.branch_from_message_id,''),c.execution_paused,c.created_at,c.updated_at FROM conversations c LEFT JOIN conversation_agent_bindings b ON b.conversation_id=c.id WHERE c.id=? AND c.user_id=?`, id, userID).Scan(&d.ID, &d.UserID, &d.Title, &d.ProviderID, &d.AgentGenerationID, &d.AgentDefinitionDigest, &d.ProjectID, &d.PermissionProfile, &d.ParentConversationID, &d.BranchFromMessageID, &d.ExecutionPaused, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, domain.ErrNotFound
	}
	if err != nil {
		return d, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,conversation_id,role,content,created_at FROM messages WHERE conversation_id=? ORDER BY created_at,rowid`, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	d.Messages = []domain.Message{}
	for rows.Next() {
		var m domain.Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return d, err
		}
		d.Messages = append(d.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return d, err
	}
	if err := rows.Close(); err != nil {
		return d, err
	}
	events, err := s.db.QueryContext(ctx, `SELECT id,conversation_id,kind,details_json,created_at FROM conversation_lifecycle_events WHERE conversation_id=? ORDER BY created_at,rowid`, id)
	if err != nil {
		return d, err
	}
	defer events.Close()
	d.LifecycleEvents = []domain.ConversationLifecycleEvent{}
	for events.Next() {
		var event domain.ConversationLifecycleEvent
		var details []byte
		if err := events.Scan(&event.ID, &event.ConversationID, &event.Kind, &details, &event.CreatedAt); err != nil {
			return d, err
		}
		event.Details = details
		d.LifecycleEvents = append(d.LifecycleEvents, event)
	}
	return d, events.Err()
}

func (s *Store) AddMessage(ctx context.Context, userID string, m domain.Message) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,role,content,created_at) SELECT ?,?,?,?,? FROM conversations WHERE id=? AND user_id=?`, m.ID, m.ConversationID, m.Role, m.Content, m.CreatedAt, m.ConversationID, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=?`, m.CreatedAt, m.ConversationID)
	return err
}

func (s *Store) AddTraceEvent(ctx context.Context, userID string, event domain.TraceEvent) error {
	result, err := s.db.ExecContext(ctx, `INSERT INTO agent_trace_events(id,conversation_id,turn_id,sequence,kind,details_json,created_at) SELECT ?,?,?,?,?,?,? FROM conversations WHERE id=? AND user_id=?`, event.ID, event.ConversationID, event.TurnID, event.Sequence, event.Kind, []byte(event.Details), event.CreatedAt, event.ConversationID, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) TraceEvents(ctx context.Context, userID, conversationID string) ([]domain.TraceEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id,t.conversation_id,t.turn_id,t.sequence,t.kind,t.details_json,t.created_at
FROM agent_trace_events t
JOIN conversations c ON c.id=t.conversation_id
LEFT JOIN agent_turns a ON a.id=t.turn_id
WHERE t.conversation_id=? AND c.user_id=?
ORDER BY COALESCE(a.started_at,t.created_at),t.sequence,t.created_at`, conversationID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.TraceEvent{}
	for rows.Next() {
		var event domain.TraceEvent
		var details []byte
		if err := rows.Scan(&event.ID, &event.ConversationID, &event.TurnID, &event.Sequence, &event.Kind, &details, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Details = details
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	return nil
}
