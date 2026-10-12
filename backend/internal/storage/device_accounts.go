package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

// Device authorization never stores account passwords or plaintext node secrets.
type DeviceAuthorization struct {
	DeviceHash string
	UserHash   string
	ClientID   string    `json:"clientId"`
	Name       string    `json:"name"`
	Platform   string    `json:"platform"`
	ExpiresAt  time.Time `json:"expiresAt"`
	UserID     string
	NodeID     string
	Cipher     []byte
	Nonce      []byte
}

func (s *Store) migrateDeviceAccounts(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS device_authorizations (
 device_hash TEXT PRIMARY KEY,user_hash TEXT NOT NULL UNIQUE,client_id TEXT NOT NULL,
 name TEXT NOT NULL,platform TEXT NOT NULL,expires_at TIMESTAMP NOT NULL,
 user_id TEXT NOT NULL DEFAULT '',node_id TEXT NOT NULL DEFAULT '',cipher BLOB,nonce BLOB);
 CREATE TABLE IF NOT EXISTS device_credential_revocations (token_hash TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id),node_id TEXT NOT NULL REFERENCES execution_nodes(id),revoked_at TIMESTAMP NOT NULL);
 CREATE TABLE IF NOT EXISTS account_devices (
 user_id TEXT NOT NULL REFERENCES users(id),client_id TEXT NOT NULL,
 node_id TEXT NOT NULL REFERENCES execution_nodes(id),cipher BLOB NOT NULL,nonce BLOB NOT NULL,
 PRIMARY KEY(user_id,client_id));
 CREATE TABLE IF NOT EXISTS execution_node_catalogs (
 node_id TEXT PRIMARY KEY REFERENCES execution_nodes(id),catalog_json BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS device_conversation_models(provider_id TEXT PRIMARY KEY REFERENCES providers(id),node_id TEXT NOT NULL REFERENCES execution_nodes(id),local_provider_id TEXT NOT NULL,UNIQUE(node_id,local_provider_id));`)
	return err
}

func (s *Store) CreateDeviceAuthorization(ctx context.Context, a DeviceAuthorization) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM device_authorizations WHERE expires_at<? AND user_id=''`, time.Now().UTC().Add(-time.Hour)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO device_authorizations(device_hash,user_hash,client_id,name,platform,expires_at) VALUES(?,?,?,?,?,?)`, a.DeviceHash, a.UserHash, a.ClientID, a.Name, a.Platform, a.ExpiresAt.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func scanDeviceAuthorization(row interface{ Scan(...any) error }) (a DeviceAuthorization, err error) {
	err = row.Scan(&a.DeviceHash, &a.UserHash, &a.ClientID, &a.Name, &a.Platform, &a.ExpiresAt, &a.UserID, &a.NodeID, &a.Cipher, &a.Nonce)
	if errors.Is(err, sql.ErrNoRows) {
		err = domain.ErrNotFound
	}
	return
}

const authorizationColumns = `device_hash,user_hash,client_id,name,platform,expires_at,user_id,node_id,cipher,nonce`

func (s *Store) DeviceAuthorization(ctx context.Context, hash string, byUser bool) (DeviceAuthorization, error) {
	column := "device_hash"
	if byUser {
		column = "user_hash"
	}
	return scanDeviceAuthorization(s.db.QueryRowContext(ctx, `SELECT `+authorizationColumns+` FROM device_authorizations WHERE `+column+`=?`, hash))
}

// Approval and node enrollment commit together. Repeating approval resumes the
// same enrollment rather than creating duplicate computers or changing secrets.
func (s *Store) ApproveDeviceAuthorization(ctx context.Context, userHash, userID, nodeID, tokenHash string, cipher, nonce []byte, now time.Time) (DeviceAuthorization, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	defer tx.Rollback()
	a, err := scanDeviceAuthorization(tx.QueryRowContext(ctx, `SELECT `+authorizationColumns+` FROM device_authorizations WHERE user_hash=?`, userHash))
	if err != nil {
		return a, err
	}
	if !a.ExpiresAt.After(now) {
		return a, domain.ErrUnauthorized
	}
	if a.UserID != "" {
		if a.UserID != userID {
			return a, domain.ErrUnauthorized
		}
		return a, tx.Commit()
	}
	var priorNode string
	var priorCipher, priorNonce []byte
	err = tx.QueryRowContext(ctx, `SELECT d.node_id,d.cipher,d.nonce FROM account_devices d JOIN execution_nodes n ON n.id=d.node_id WHERE d.user_id=? AND d.client_id=? AND n.revoked_at IS NULL`, userID, a.ClientID).Scan(&priorNode, &priorCipher, &priorNonce)
	if err == nil {
		nodeID, cipher, nonce = priorNode, priorCipher, priorNonce
		if _, err = tx.ExecContext(ctx, `UPDATE execution_nodes SET name=?,updated_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`, a.Name, now.UTC(), nodeID, userID); err != nil {
			return a, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return a, err
	} else {
		// Do not revive a revoked identity while old leases await reconciliation.
		var active int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_tasks WHERE node_id=? AND status IN ('leased','accepted','running','reported_succeeded','reported_failed')`, nodeID).Scan(&active); err != nil {
			return a, err
		}
		if active != 0 {
			return a, domain.ErrBusy
		}
		caps := []byte(`["agent-runtime","conversation-transcript","sandboxed-tools","safe-handoff"]`)
		if _, err = tx.ExecContext(ctx, `INSERT INTO execution_nodes(id,user_id,name,platform,capabilities_json,token_hash,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET token_hash=excluded.token_hash,revoked_at=NULL,last_seen=NULL,name=excluded.name,updated_at=excluded.updated_at WHERE execution_nodes.user_id=excluded.user_id`, nodeID, userID, a.Name, a.Platform, caps, tokenHash, now.UTC(), now.UTC()); err != nil {
			return a, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO account_devices(user_id,client_id,node_id,cipher,nonce) VALUES(?,?,?,?,?) ON CONFLICT(user_id,client_id) DO UPDATE SET node_id=excluded.node_id,cipher=excluded.cipher,nonce=excluded.nonce`, userID, a.ClientID, nodeID, cipher, nonce); err != nil {
			return a, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE device_authorizations SET user_id=?,node_id=?,cipher=?,nonce=? WHERE user_hash=? AND user_id=''`, userID, nodeID, cipher, nonce, userHash); err != nil {
		return a, err
	}
	a, err = scanDeviceAuthorization(tx.QueryRowContext(ctx, `SELECT `+authorizationColumns+` FROM device_authorizations WHERE user_hash=?`, userHash))
	if err != nil {
		return a, err
	}
	if a.UserID != userID || a.NodeID != nodeID || len(a.Cipher) == 0 {
		return a, domain.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return a, err
	}
	return s.DeviceAuthorization(ctx, userHash, true)
}

type NodeModel struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Model          string `json:"model"`
	SupportsVision *bool  `json:"supportsVision,omitempty"`
}
type NodeCatalog struct {
	Models             []NodeModel                `json:"models"`
	PermissionProfiles []domain.PermissionProfile `json:"permissionProfiles"`
}

func (c NodeCatalog) Validate() error {
	seen := map[string]bool{}
	for _, m := range c.Models {
		if m.ID == "" || strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Model) == "" || seen[m.ID] {
			return domain.ErrInvalid
		}
		seen[m.ID] = true
	}
	for _, p := range c.PermissionProfiles {
		if !p.Valid() {
			return domain.ErrInvalid
		}
	}
	return nil
}
func (s *Store) SetExecutionNodeCatalog(ctx context.Context, nodeID string, c NodeCatalog) error {
	if err := c.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO execution_node_catalogs(node_id,catalog_json) VALUES(?,?) ON CONFLICT(node_id) DO UPDATE SET catalog_json=excluded.catalog_json`, nodeID, body)
	if err != nil {
		return err
	}
	got, err := s.ExecutionNodeCatalog(ctx, nodeID)
	if err != nil {
		return err
	}
	readBack, err := json.Marshal(got)
	if err != nil {
		return err
	}
	if string(readBack) != string(body) {
		return fmt.Errorf("device catalog did not read back")
	}
	return nil
}
func (s *Store) ExecutionNodeCatalog(ctx context.Context, nodeID string) (NodeCatalog, error) {
	c := NodeCatalog{Models: []NodeModel{}, PermissionProfiles: []domain.PermissionProfile{}}
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT catalog_json FROM execution_node_catalogs WHERE node_id=?`, nodeID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(body, &c)
	return c, err
}

// Pending-only deletion races safely with approval: the caller retries the authorized state.
func (s *Store) DeletePendingDeviceAuthorization(ctx context.Context, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM device_authorizations WHERE device_hash=? AND user_id=''`, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
func (s *Store) DeleteDeviceAuthorization(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM device_authorizations WHERE device_hash=?`, hash)
	if err != nil {
		return err
	}
	_, err = s.DeviceAuthorization(ctx, hash, false)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return domain.ErrConflict
}

// DeviceModelReference is routing metadata, not a cloud model endpoint or a copy
// of a local API key. It satisfies conversation identity without cloud compute.
const DeviceModelReferenceKind = "remote-node"

func (s *Store) EnsureDeviceModelReference(ctx context.Context, userID, nodeID, localProviderID string) (domain.Provider, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Provider{}, err
	}
	defer tx.Rollback()
	node, err := executionNodeByID(ctx, tx, userID, nodeID)
	if err != nil {
		return domain.Provider{}, err
	}
	if node.RevokedAt != nil || node.LastSeen == nil || time.Since(*node.LastSeen) > 90*time.Second {
		return domain.Provider{}, domain.ErrConflict
	}
	var body []byte
	if err = tx.QueryRowContext(ctx, `SELECT catalog_json FROM execution_node_catalogs WHERE node_id=?`, nodeID).Scan(&body); err != nil {
		return domain.Provider{}, err
	}
	var catalog NodeCatalog
	if err = json.Unmarshal(body, &catalog); err != nil {
		return domain.Provider{}, err
	}
	var model NodeModel
	for _, m := range catalog.Models {
		if m.ID == localProviderID {
			model = m
			break
		}
	}
	if model.ID == "" {
		return domain.Provider{}, domain.ErrNotFound
	}
	digest := sha256.Sum256([]byte(nodeID + ":" + localProviderID))
	id := "device_model_" + hex.EncodeToString(digest[:16])
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `INSERT INTO providers(id,user_id,name,kind,base_url,model,context_window,supports_vision,api_key_cipher,api_key_nonce,created_at,updated_at) VALUES(?,?,?,?,'',?,0,?,X'',X'',?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,model=excluded.model,supports_vision=excluded.supports_vision,updated_at=excluded.updated_at WHERE providers.user_id=excluded.user_id AND providers.kind=excluded.kind`, id, userID, node.Name+" · "+model.Name, DeviceModelReferenceKind, model.Model, nullableVision(model.SupportsVision), now, now); err != nil {
		return domain.Provider{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO device_conversation_models(provider_id,node_id,local_provider_id) VALUES(?,?,?) ON CONFLICT(provider_id) DO NOTHING`, id, nodeID, localProviderID); err != nil {
		return domain.Provider{}, err
	}
	var savedNode, savedModel string
	if err = tx.QueryRowContext(ctx, `SELECT node_id,local_provider_id FROM device_conversation_models WHERE provider_id=?`, id).Scan(&savedNode, &savedModel); err != nil {
		return domain.Provider{}, err
	}
	if savedNode != nodeID || savedModel != localProviderID {
		return domain.Provider{}, domain.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return domain.Provider{}, err
	}
	p, cipher, nonce, err := s.ProviderSecret(ctx, userID, id)
	if err != nil {
		return p, err
	}
	if p.Kind != DeviceModelReferenceKind || p.Model != model.Model || p.BaseURL != "" || len(cipher) != 0 || len(nonce) != 0 {
		return p, domain.ErrConflict
	}
	return p, nil
}

// DeviceCredentialRevoked proves a particular credential epoch was durably revoked.
// It does not authenticate that credential for any other node operation.
func (s *Store) DeviceCredentialRevoked(ctx context.Context, userID, tokenHash string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM device_credential_revocations WHERE user_id=? AND token_hash=?", userID, tokenHash).Scan(&count)
	return count == 1, err
}
