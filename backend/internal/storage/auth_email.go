package storage

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

type AuthEmailChallenge struct {
	ID           string
	Purpose      string
	UserID       string
	Email        string
	DisplayName  string
	PasswordHash string
	CodeHash     string
	State        string
	ProviderID   string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	Attempts     int
}

func (s *Store) migrateAuthEmail(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS auth_email_challenges (
 id TEXT PRIMARY KEY, purpose TEXT NOT NULL, user_id TEXT NOT NULL REFERENCES users(id),
 email TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '', password_hash TEXT NOT NULL DEFAULT '',
 code_hash TEXT NOT NULL, state TEXT NOT NULL, provider_id TEXT NOT NULL DEFAULT '',
 created_at DATETIME NOT NULL, expires_at DATETIME NOT NULL, attempts INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX IF NOT EXISTS idx_auth_email_recipient ON auth_email_challenges(email,purpose,created_at);
 CREATE TABLE IF NOT EXISTS auth_verified_emails (
 user_id TEXT PRIMARY KEY REFERENCES users(id), email TEXT NOT NULL, verified_at DATETIME NOT NULL);`)
	return err
}

func (s *Store) AuthEmailChallenge(ctx context.Context, id string) (AuthEmailChallenge, error) {
	var c AuthEmailChallenge
	err := s.db.QueryRowContext(ctx, `SELECT id,purpose,user_id,email,display_name,password_hash,code_hash,state,provider_id,created_at,expires_at,attempts FROM auth_email_challenges WHERE id=?`, id).
		Scan(&c.ID, &c.Purpose, &c.UserID, &c.Email, &c.DisplayName, &c.PasswordHash, &c.CodeHash, &c.State, &c.ProviderID, &c.CreatedAt, &c.ExpiresAt, &c.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return c, domain.ErrNotFound
	}
	return c, err
}

// PrepareAuthEmailChallenge reserves the recipient cooldown before contacting
// a provider. A delivery-pending challenge cannot authorize a mutation.
func (s *Store) PrepareAuthEmailChallenge(ctx context.Context, c AuthEmailChallenge) error {
	if c.ID == "" || (c.Purpose != "register" && c.Purpose != "reset") || c.CodeHash == "" || c.UserID == "" || !c.ExpiresAt.After(c.CreatedAt) {
		return domain.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recent int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_email_challenges WHERE email=? AND created_at>?`, c.Email, c.CreatedAt.Add(-time.Minute)).Scan(&recent); err != nil {
		return err
	}
	if recent > 0 {
		return domain.ErrBusy
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_email_challenges(id,purpose,user_id,email,display_name,password_hash,code_hash,state,created_at,expires_at) VALUES(?,?,?,?,?,?,?,'delivery_pending',?,?)`, c.ID, c.Purpose, c.UserID, c.Email, c.DisplayName, c.PasswordHash, c.CodeHash, c.CreatedAt, c.ExpiresAt)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	got, err := s.AuthEmailChallenge(ctx, c.ID)
	if err != nil {
		return err
	}
	if got.State != "delivery_pending" || got.CodeHash != c.CodeHash || got.PasswordHash != c.PasswordHash || got.UserID != c.UserID {
		return domain.ErrConflict
	}
	return nil
}

func (s *Store) SetAuthEmailDelivery(ctx context.Context, id, providerID string, accepted bool) error {
	state := "delivery_unconfirmed"
	if accepted {
		if providerID == "" {
			return domain.ErrInvalid
		}
		state = "awaiting_code"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE auth_email_challenges SET state=?,provider_id=? WHERE id=? AND state='delivery_pending'`, state, providerID, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.ErrConflict
	}
	if accepted {
		_, err = tx.ExecContext(ctx, `UPDATE auth_email_challenges SET state='superseded' WHERE id<>? AND state='awaiting_code' AND email=(SELECT email FROM auth_email_challenges WHERE id=?) AND purpose=(SELECT purpose FROM auth_email_challenges WHERE id=?)`, id, id, id)
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	got, err := s.AuthEmailChallenge(ctx, id)
	if err != nil {
		return err
	}
	if got.State != state || got.ProviderID != providerID {
		return domain.ErrConflict
	}
	return nil
}

// ConsumeAuthEmailChallenge changes credentials and consumes the proof in one
// transaction. Password recovery also revokes every previously issued session.
func (s *Store) ConsumeAuthEmailChallenge(ctx context.Context, id, purpose, codeHash, newPasswordHash string, now time.Time, clearBuckets ...string) (domain.User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.User{}, err
	}
	defer tx.Rollback()
	var c AuthEmailChallenge
	err = tx.QueryRowContext(ctx, `SELECT purpose,user_id,email,display_name,password_hash,code_hash,state,expires_at,attempts FROM auth_email_challenges WHERE id=?`, id).Scan(&c.Purpose, &c.UserID, &c.Email, &c.DisplayName, &c.PasswordHash, &c.CodeHash, &c.State, &c.ExpiresAt, &c.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, domain.ErrUnauthorized
	}
	if err != nil {
		return domain.User{}, err
	}
	if c.State == "awaiting_code" && !c.ExpiresAt.After(now) {
		if _, err = tx.ExecContext(ctx, `UPDATE auth_email_challenges SET state='expired',code_hash='',password_hash='' WHERE id=?`, id); err != nil {
			return domain.User{}, err
		}
		if err = tx.Commit(); err != nil {
			return domain.User{}, err
		}
		return domain.User{}, domain.ErrUnauthorized
	}
	if c.State != "awaiting_code" || c.Purpose != purpose || c.Attempts >= 5 {
		return domain.User{}, domain.ErrUnauthorized
	}
	if subtle.ConstantTimeCompare([]byte(c.CodeHash), []byte(codeHash)) != 1 {
		state := "awaiting_code"
		if c.Attempts+1 >= 5 {
			state = "attempts_exhausted"
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_email_challenges SET attempts=attempts+1,state=? WHERE id=?`, state, id)
		if err != nil {
			return domain.User{}, err
		}
		if err = tx.Commit(); err != nil {
			return domain.User{}, err
		}
		return domain.User{}, domain.ErrUnauthorized
	}
	var hash string
	if purpose == "register" {
		hash = c.PasswordHash
		result, e := tx.ExecContext(ctx, `UPDATE users SET email=?,display_name=?,password_hash=? WHERE id=? AND (password_hash='' OR password_hash NOT LIKE 'pbkdf2-sha256$%')`, c.Email, c.DisplayName, hash, c.UserID)
		if e != nil {
			return domain.User{}, e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return domain.User{}, e
		}
		if n != 1 {
			return domain.User{}, domain.ErrConflict
		}
	} else {
		hash = newPasswordHash
		if !strings.HasPrefix(hash, "pbkdf2-sha256$") {
			return domain.User{}, domain.ErrInvalid
		}
		result, e := tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=? AND email=? AND password_hash LIKE 'pbkdf2-sha256$%'`, hash, c.UserID, c.Email)
		if e != nil {
			return domain.User{}, e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return domain.User{}, e
		}
		if n != 1 {
			return domain.User{}, domain.ErrConflict
		}
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") {
		return domain.User{}, domain.ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_verified_emails(user_id,email,verified_at) VALUES(?,?,?) ON CONFLICT(user_id) DO UPDATE SET email=excluded.email,verified_at=excluded.verified_at`, c.UserID, c.Email, now.UTC()); err != nil {
		return domain.User{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_email_challenges SET state='consumed',password_hash='',code_hash='' WHERE id=?`, id); err != nil {
		return domain.User{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_email_challenges SET state='superseded',password_hash='',code_hash='' WHERE user_id=? AND id<>? AND state IN ('awaiting_code','delivery_pending')`, c.UserID, id); err != nil {
		return domain.User{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id=?`, c.UserID); err != nil {
		return domain.User{}, err
	}
	for _, bucket := range clearBuckets {
		if _, err = tx.ExecContext(ctx, `DELETE FROM auth_login_limits WHERE bucket_hash=?`, bucket); err != nil {
			return domain.User{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.User{}, err
	}
	user, persisted, err := s.AuthUserByID(ctx, c.UserID)
	if err != nil {
		return domain.User{}, err
	}
	got, err := s.AuthEmailChallenge(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	if persisted != hash || user.Email != c.Email || got.State != "consumed" {
		return domain.User{}, domain.ErrConflict
	}
	return user, nil
}

func (s *Store) DeleteUserAuthSessions(ctx context.Context, userID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE user_id=?`, userID).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return domain.ErrConflict
	}
	return nil
}
