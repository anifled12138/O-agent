package pluginforge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"axiom.local/agent/internal/domain"
	_ "modernc.org/sqlite"
)

type Repository struct{ db *sql.DB }

func OpenRepository(dataDir string) (*Repository, error) {
	path := filepath.Join(dataDir, "axiom.db")
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	r := &Repository{db: db}
	if err := r.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return r, nil
}

func (r *Repository) Close() error { return r.db.Close() }

func (r *Repository) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS plugin_projects (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), name TEXT NOT NULL,
 slug TEXT NOT NULL, description TEXT NOT NULL, state TEXT NOT NULL, source_dir TEXT NOT NULL,
 spec_json BLOB NOT NULL, last_error TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL, UNIQUE(user_id, slug)
);
CREATE TABLE IF NOT EXISTS plugin_releases (
 id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES plugin_projects(id), plugin_id TEXT NOT NULL,
 version TEXT NOT NULL, digest TEXT NOT NULL UNIQUE, bundle_dir TEXT NOT NULL, manifest_json BLOB NOT NULL,
 test_report_json BLOB NOT NULL, permission_hash TEXT NOT NULL, created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_plugin_releases_project ON plugin_releases(project_id, created_at DESC);
CREATE TABLE IF NOT EXISTS plugin_installations (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), plugin_id TEXT NOT NULL,
 project_id TEXT NOT NULL REFERENCES plugin_projects(id), active_release_id TEXT NOT NULL REFERENCES plugin_releases(id),
 status TEXT NOT NULL, installed_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
 UNIQUE(user_id, plugin_id)
);
CREATE TABLE IF NOT EXISTS plugin_grants (
 user_id TEXT NOT NULL REFERENCES users(id), project_id TEXT NOT NULL REFERENCES plugin_projects(id),
 release_id TEXT NOT NULL REFERENCES plugin_releases(id), permission_hash TEXT NOT NULL,
 granted_at DATETIME NOT NULL, PRIMARY KEY(user_id, release_id)
);
CREATE TABLE IF NOT EXISTS plugin_audit_events (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL, project_id TEXT NOT NULL DEFAULT '',
 plugin_id TEXT NOT NULL DEFAULT '', action TEXT NOT NULL, details_json BLOB NOT NULL,
 created_at DATETIME NOT NULL
);
`
	_, err := r.db.ExecContext(ctx, schema)
	return err
}

func (r *Repository) CreateProject(ctx context.Context, p Project) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO plugin_projects(id,user_id,name,slug,description,state,source_dir,spec_json,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, p.ID, p.UserID, p.Name, p.Slug, p.Description, p.State, p.SourceDir, []byte(p.Spec), p.LastError, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create plugin project: %w", err)
	}
	return nil
}

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var spec []byte
	err := row.Scan(&p.ID, &p.UserID, &p.Name, &p.Slug, &p.Description, &p.State, &p.SourceDir, &spec, &p.LastError, &p.CreatedAt, &p.UpdatedAt)
	p.Spec = json.RawMessage(spec)
	return p, err
}

func (r *Repository) Project(ctx context.Context, userID, id string) (Project, error) {
	p, err := scanProject(r.db.QueryRowContext(ctx, `SELECT id,user_id,name,slug,description,state,source_dir,spec_json,last_error,created_at,updated_at FROM plugin_projects WHERE id=? AND user_id=?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return p, domain.ErrNotFound
	}
	return p, err
}

func (r *Repository) ListProjects(ctx context.Context, userID string) ([]Project, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,user_id,name,slug,description,state,source_dir,spec_json,last_error,created_at,updated_at FROM plugin_projects WHERE user_id=? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (r *Repository) Transition(ctx context.Context, userID, id string, to State, lastError string) (Project, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()
	p, err := scanProject(tx.QueryRowContext(ctx, `SELECT id,user_id,name,slug,description,state,source_dir,spec_json,last_error,created_at,updated_at FROM plugin_projects WHERE id=? AND user_id=?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, domain.ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	if !CanTransition(p.State, to) {
		return Project{}, fmt.Errorf("invalid plugin transition %s -> %s", p.State, to)
	}
	p.State = to
	p.LastError = lastError
	p.UpdatedAt = time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE plugin_projects SET state=?,last_error=?,updated_at=? WHERE id=?`, p.State, p.LastError, p.UpdatedAt, p.ID); err != nil {
		return Project{}, err
	}
	if err = tx.Commit(); err != nil {
		return Project{}, err
	}
	return p, nil
}

func (r *Repository) CreateRelease(ctx context.Context, release Release) error {
	manifest, _ := json.Marshal(release.Manifest)
	_, err := r.db.ExecContext(ctx, `INSERT INTO plugin_releases(id,project_id,plugin_id,version,digest,bundle_dir,manifest_json,test_report_json,permission_hash,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, release.ID, release.ProjectID, release.PluginID, release.Version, release.Digest, release.BundleDir, manifest, []byte(release.TestReport), release.PermissionHash, release.CreatedAt)
	return err
}

func scanRelease(row interface{ Scan(...any) error }) (Release, error) {
	var release Release
	var manifest, test []byte
	err := row.Scan(&release.ID, &release.ProjectID, &release.PluginID, &release.Version, &release.Digest, &release.BundleDir, &manifest, &test, &release.PermissionHash, &release.CreatedAt)
	if err == nil {
		err = json.Unmarshal(manifest, &release.Manifest)
		release.TestReport = json.RawMessage(test)
	}
	return release, err
}
func (r *Repository) Release(ctx context.Context, id string) (Release, error) {
	release, err := scanRelease(r.db.QueryRowContext(ctx, `SELECT id,project_id,plugin_id,version,digest,bundle_dir,manifest_json,test_report_json,permission_hash,created_at FROM plugin_releases WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return release, domain.ErrNotFound
	}
	return release, err
}
func (r *Repository) LatestRelease(ctx context.Context, projectID string) (Release, error) {
	release, err := scanRelease(r.db.QueryRowContext(ctx, `SELECT id,project_id,plugin_id,version,digest,bundle_dir,manifest_json,test_report_json,permission_hash,created_at FROM plugin_releases WHERE project_id=? ORDER BY created_at DESC LIMIT 1`, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return release, domain.ErrNotFound
	}
	return release, err
}

func (r *Repository) Releases(ctx context.Context, projectID string) ([]Release, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,project_id,plugin_id,version,digest,bundle_dir,manifest_json,test_report_json,permission_hash,created_at FROM plugin_releases WHERE project_id=? ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Release{}
	for rows.Next() {
		release, scanErr := scanRelease(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, release)
	}
	return result, rows.Err()
}

func (r *Repository) Grant(ctx context.Context, userID string, release Release) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO plugin_grants(user_id,project_id,release_id,permission_hash,granted_at) VALUES(?,?,?,?,?) ON CONFLICT(user_id,release_id) DO UPDATE SET permission_hash=excluded.permission_hash,granted_at=excluded.granted_at`, userID, release.ProjectID, release.ID, release.PermissionHash, time.Now().UTC())
	return err
}
func (r *Repository) HasGrant(ctx context.Context, userID string, release Release) (bool, error) {
	var hash string
	err := r.db.QueryRowContext(ctx, `SELECT permission_hash FROM plugin_grants WHERE user_id=? AND release_id=?`, userID, release.ID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return hash == release.PermissionHash, err
}

func (r *Repository) Activate(ctx context.Context, userID string, release Release) (Installation, error) {
	now := time.Now().UTC()
	installation := Installation{ID: "ins_" + release.Digest[:16], UserID: userID, PluginID: release.PluginID, ProjectID: release.ProjectID, ActiveReleaseID: release.ID, Status: "active", InstalledAt: now, UpdatedAt: now}
	_, err := r.db.ExecContext(ctx, `INSERT INTO plugin_installations(id,user_id,plugin_id,project_id,active_release_id,status,installed_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(user_id,plugin_id) DO UPDATE SET active_release_id=excluded.active_release_id,status='active',updated_at=excluded.updated_at`, installation.ID, userID, installation.PluginID, installation.ProjectID, installation.ActiveReleaseID, installation.Status, installation.InstalledAt, installation.UpdatedAt)
	return installation, err
}

func (r *Repository) SetInstallationStatus(ctx context.Context, userID, pluginID, status string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE plugin_installations SET status=?,updated_at=? WHERE user_id=? AND plugin_id=?`, status, time.Now().UTC(), userID, pluginID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return domain.ErrNotFound
	}
	return nil
}
func (r *Repository) ListInstallations(ctx context.Context, userID string) ([]Installation, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,user_id,plugin_id,project_id,active_release_id,status,installed_at,updated_at FROM plugin_installations WHERE user_id=? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Installation{}
	for rows.Next() {
		var i Installation
		if err := rows.Scan(&i.ID, &i.UserID, &i.PluginID, &i.ProjectID, &i.ActiveReleaseID, &i.Status, &i.InstalledAt, &i.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, i)
	}
	return result, rows.Err()
}
func (r *Repository) ActiveInstallations(ctx context.Context) ([]Installation, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,user_id,plugin_id,project_id,active_release_id,status,installed_at,updated_at FROM plugin_installations WHERE status='active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Installation{}
	for rows.Next() {
		var i Installation
		if err := rows.Scan(&i.ID, &i.UserID, &i.PluginID, &i.ProjectID, &i.ActiveReleaseID, &i.Status, &i.InstalledAt, &i.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, i)
	}
	return result, rows.Err()
}

func (r *Repository) Audit(ctx context.Context, event AuditEvent) error {
	if len(event.Details) == 0 {
		event.Details = json.RawMessage(`{}`)
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO plugin_audit_events(id,user_id,project_id,plugin_id,action,details_json,created_at) VALUES(?,?,?,?,?,?,?)`, event.ID, event.UserID, event.ProjectID, event.PluginID, event.Action, []byte(event.Details), event.CreatedAt)
	return err
}
