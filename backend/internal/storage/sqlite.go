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

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
 id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL,
 password_hash TEXT NOT NULL, created_at DATETIME NOT NULL
);
CREATE TABLE IF NOT EXISTS auth_sessions (
 token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
 expires_at DATETIME NOT NULL, created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_auth_sessions_user ON auth_sessions(user_id);
CREATE TABLE IF NOT EXISTS providers (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), name TEXT NOT NULL,
 kind TEXT NOT NULL, base_url TEXT NOT NULL, model TEXT NOT NULL,
 api_key_cipher BLOB NOT NULL, api_key_nonce BLOB NOT NULL,
 created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
 UNIQUE(user_id, name)
);
CREATE TABLE IF NOT EXISTS conversations (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), title TEXT NOT NULL,
 provider_id TEXT NOT NULL REFERENCES providers(id), created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversations_user ON conversations(user_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS messages (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id),
 role TEXT NOT NULL, content TEXT NOT NULL, created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, created_at);
`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

func (s *Store) CreateUser(ctx context.Context, u domain.User, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id,email,display_name,password_hash,created_at) VALUES(?,?,?,?,?)`, u.ID, strings.ToLower(u.Email), u.DisplayName, passwordHash, u.CreatedAt)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return domain.ErrConflict
	}
	return err
}

func (s *Store) UserByEmail(ctx context.Context, email string) (domain.User, string, error) {
	var u domain.User
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id,email,display_name,password_hash,created_at FROM users WHERE email=?`, strings.ToLower(email)).Scan(&u.ID, &u.Email, &u.DisplayName, &hash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, "", domain.ErrNotFound
	}
	return u, hash, err
}

func (s *Store) CreateAuthSession(ctx context.Context, tokenHash, userID string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_sessions(token_hash,user_id,expires_at,created_at) VALUES(?,?,?,?)`, tokenHash, userID, expires, time.Now().UTC())
	return err
}

func (s *Store) UserBySession(ctx context.Context, tokenHash string) (domain.User, error) {
	var u domain.User
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.email,u.display_name,u.created_at FROM auth_sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`, tokenHash, time.Now().UTC()).Scan(&u.ID, &u.Email, &u.DisplayName, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, domain.ErrUnauthorized
	}
	return u, err
}

func (s *Store) RevokeAuthSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE auth_sessions SET expires_at=? WHERE token_hash=?`, time.Now().UTC(), tokenHash)
	return err
}

func (s *Store) UpsertProvider(ctx context.Context, p domain.Provider, cipher, nonce []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO providers(id,user_id,name,kind,base_url,model,api_key_cipher,api_key_nonce,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, p.ID, p.UserID, p.Name, p.Kind, p.BaseURL, p.Model, cipher, nonce, p.CreatedAt, p.UpdatedAt)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return domain.ErrConflict
	}
	return err
}

func (s *Store) ListProviders(ctx context.Context, userID string) ([]domain.Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,name,kind,base_url,model,length(api_key_cipher)>0,created_at,updated_at FROM providers WHERE user_id=? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Provider{}
	for rows.Next() {
		var p domain.Provider
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Kind, &p.BaseURL, &p.Model, &p.HasAPIKey, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) ProviderSecret(ctx context.Context, userID, id string) (domain.Provider, []byte, []byte, error) {
	var p domain.Provider
	var cipher, nonce []byte
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,name,kind,base_url,model,api_key_cipher,api_key_nonce,created_at,updated_at FROM providers WHERE id=? AND user_id=?`, id, userID).Scan(&p.ID, &p.UserID, &p.Name, &p.Kind, &p.BaseURL, &p.Model, &cipher, &nonce, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil, nil, domain.ErrNotFound
	}
	p.HasAPIKey = len(cipher) > 0
	return p, cipher, nonce, err
}

func (s *Store) CreateConversation(ctx context.Context, c domain.Conversation) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversations(id,user_id,title,provider_id,created_at,updated_at) VALUES(?,?,?,?,?,?)`, c.ID, c.UserID, c.Title, c.ProviderID, c.CreatedAt, c.UpdatedAt)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		return domain.ErrInvalid
	}
	return err
}

func (s *Store) ListConversations(ctx context.Context, userID string) ([]domain.Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,title,provider_id,created_at,updated_at FROM conversations WHERE user_id=? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Conversation{}
	for rows.Next() {
		var c domain.Conversation
		if err := rows.Scan(&c.ID, &c.UserID, &c.Title, &c.ProviderID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) Conversation(ctx context.Context, userID, id string) (domain.ConversationDetail, error) {
	var d domain.ConversationDetail
	err := s.db.QueryRowContext(ctx, `SELECT id,user_id,title,provider_id,created_at,updated_at FROM conversations WHERE id=? AND user_id=?`, id, userID).Scan(&d.ID, &d.UserID, &d.Title, &d.ProviderID, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, domain.ErrNotFound
	}
	if err != nil {
		return d, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,conversation_id,role,content,created_at FROM messages WHERE conversation_id=? ORDER BY created_at`, id)
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
	return d, rows.Err()
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

func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	return nil
}
