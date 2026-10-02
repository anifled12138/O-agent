package gitcredential

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/secure"
)

const settingKey = "git.credentials.v1"

type Service struct {
	store RuntimeSettingsStore
	vault *secure.Vault
	mu    sync.RWMutex
	items []entry
}

// RuntimeSettingsStore is the durable settings surface used by the broker.
// Keeping this contract small also lets persistence rollback behavior be
// exercised without a database fault-injection hook in production code.
type RuntimeSettingsStore interface {
	RuntimeSetting(context.Context, string) (string, error)
	SetRuntimeSetting(context.Context, string, string) error
	DeleteRuntimeSetting(context.Context, string) error
}

type Credential struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Username   string `json:"username"`
	Password   string `json:"password,omitempty"`
}

type Summary struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Username   string `json:"username"`
	Configured bool   `json:"configured"`
}

type entry struct {
	Credential
}

type sealedSettings struct {
	Version    int    `json:"version"`
	Ciphertext string `json:"ciphertext"`
	Nonce      string `json:"nonce"`
}

func New(ctx context.Context, store RuntimeSettingsStore, vault *secure.Vault) (*Service, error) {
	if store == nil || vault == nil {
		return nil, errors.New("Git credential storage or vault is unavailable")
	}
	s := &Service{store: store, vault: vault}
	raw, err := store.RuntimeSetting(ctx, settingKey)
	if errors.Is(err, domain.ErrNotFound) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load Git credential settings: %w", err)
	}
	var sealed sealedSettings
	if err := json.Unmarshal([]byte(raw), &sealed); err != nil || sealed.Version != 1 {
		return nil, errors.New("saved Git credential settings are invalid")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(sealed.Ciphertext)
	if err != nil {
		return nil, errors.New("saved Git credential settings are invalid")
	}
	nonce, err := base64.StdEncoding.DecodeString(sealed.Nonce)
	if err != nil {
		return nil, errors.New("saved Git credential settings are invalid")
	}
	plain, err := vault.Open(ciphertext, nonce)
	if err != nil {
		return nil, fmt.Errorf("decrypt Git credential settings: %w", err)
	}
	defer zero(plain)
	if err := json.Unmarshal(plain, &s.items); err != nil {
		return nil, errors.New("decrypted Git credential settings are invalid")
	}
	for _, item := range s.items {
		if _, _, err := normalizeScope(item.Host, item.Repository); err != nil || item.ID == "" || item.Username == "" || item.Password == "" {
			return nil, errors.New("saved Git credential entry is invalid")
		}
	}
	return s, nil
}

func (s *Service) List() []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Summary, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, Summary{ID: item.ID, Host: item.Host, Repository: item.Repository, Username: item.Username, Configured: true})
	}
	return out
}

func (s *Service) Save(ctx context.Context, credential Credential) (Summary, error) {
	credential.ID = strings.TrimSpace(credential.ID)
	credential.Username = strings.TrimSpace(credential.Username)
	if credential.ID == "" || len(credential.ID) > 128 || strings.ContainsAny(credential.ID, "\r\n\x00") || credential.Username == "" || len(credential.Username) > 256 || strings.ContainsAny(credential.Username, "\r\n\x00") || strings.TrimSpace(credential.Password) == "" || len(credential.Password) > 8192 || strings.ContainsAny(credential.Password, "\r\n\x00") {
		return Summary{}, domain.ErrInvalid
	}
	host, repository, err := normalizeScope(credential.Host, credential.Repository)
	if err != nil {
		return Summary{}, domain.ErrInvalid
	}
	credential.Host, credential.Repository = host, repository
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]entry(nil), s.items...)
	found := false
	for i := range next {
		if next[i].ID == credential.ID || (next[i].Host == host && next[i].Repository == repository) {
			next[i] = entry{Credential: credential}
			found = true
			break
		}
	}
	if !found {
		next = append(next, entry{Credential: credential})
	}
	if err := s.persist(ctx, next); err != nil {
		return Summary{}, err
	}
	s.items = next
	return Summary{ID: credential.ID, Host: host, Repository: repository, Username: credential.Username, Configured: true}, nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make([]entry, 0, len(s.items))
	found := false
	for _, item := range s.items {
		if item.ID == id {
			found = true
			continue
		}
		next = append(next, item)
	}
	if !found {
		return domain.ErrNotFound
	}
	if err := s.persist(ctx, next); err != nil {
		return err
	}
	s.items = next
	return nil
}

func (s *Service) Lookup(repositoryURL string) (username, password string, ok bool) {
	host, repository, err := scopeFromURL(repositoryURL)
	if err != nil {
		return "", "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	best := -1
	var match *entry
	for i := range s.items {
		item := &s.items[i]
		if item.Host == host && repository == item.Repository && len(item.Repository) > best {
			best, match = len(item.Repository), item
		}
	}
	if match == nil {
		return "", "", false
	}
	return strings.Clone(match.Username), strings.Clone(match.Password), true
}

func (s *Service) persist(ctx context.Context, items []entry) error {
	plain, err := json.Marshal(items)
	if err != nil {
		return err
	}
	defer zero(plain)
	ciphertext, nonce, err := s.vault.Seal(plain)
	if err != nil {
		return fmt.Errorf("encrypt Git credentials: %w", err)
	}
	raw, err := json.Marshal(sealedSettings{Version: 1, Ciphertext: base64.StdEncoding.EncodeToString(ciphertext), Nonce: base64.StdEncoding.EncodeToString(nonce)})
	if err != nil {
		return err
	}
	previous, previousErr := s.store.RuntimeSetting(ctx, settingKey)
	if previousErr != nil && !errors.Is(previousErr, domain.ErrNotFound) {
		return fmt.Errorf("read previous Git credential state: %w", previousErr)
	}
	if err := s.store.SetRuntimeSetting(ctx, settingKey, string(raw)); err != nil {
		return fmt.Errorf("persist Git credential state: %w", err)
	}
	verifyCtx, cancel := detachedPersistenceContext(ctx)
	defer cancel()
	readBack, err := s.store.RuntimeSetting(verifyCtx, settingKey)
	if err == nil && subtle.ConstantTimeCompare([]byte(readBack), raw) == 1 {
		return nil
	}
	verifyErr := errors.New("Git credential setting read-back did not match")
	if err != nil {
		verifyErr = fmt.Errorf("read back Git credential state: %w", err)
	}
	var rollbackErr error
	if errors.Is(previousErr, domain.ErrNotFound) {
		rollbackErr = s.store.DeleteRuntimeSetting(verifyCtx, settingKey)
	} else {
		rollbackErr = s.store.SetRuntimeSetting(verifyCtx, settingKey, previous)
	}
	if rollbackErr == nil {
		rolledBack, rollbackReadErr := s.store.RuntimeSetting(verifyCtx, settingKey)
		if errors.Is(previousErr, domain.ErrNotFound) {
			if !errors.Is(rollbackReadErr, domain.ErrNotFound) {
				rollbackErr = errors.New("Git credential rollback was not durable")
			}
		} else if rollbackReadErr != nil || rolledBack != previous {
			rollbackErr = errors.Join(errors.New("Git credential rollback read-back did not match"), rollbackReadErr)
		}
	}
	return errors.Join(verifyErr, rollbackErr)
}

// Once the settings write has returned successfully, finish verification or
// rollback even if the HTTP caller disconnects and cancels its request.
func detachedPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func normalizeScope(host, repository string) (string, string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if strings.ContainsAny(host+repository, "\r\n\x00") || host == "" || repository == "" || strings.Contains(repository, "\\") {
		return "", "", errors.New("invalid Git credential scope")
	}
	parsed, err := url.Parse("https://" + host)
	if err != nil || parsed.Hostname() != host || parsed.User != nil || parsed.Port() != "" || strings.Contains(host, ":") {
		return "", "", errors.New("invalid Git credential host")
	}
	for _, part := range strings.Split(repository, "/") {
		if part == "" || part == "." || part == ".." {
			return "", "", errors.New("invalid Git repository scope")
		}
	}
	if strings.HasSuffix(strings.ToLower(repository), ".git") {
		repository = repository[:len(repository)-len(".git")]
	}
	return host, repository, nil
}

func scopeFromURL(raw string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.ToLower(parsed.Scheme) != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" || parsed.RawPath != "" || strings.Contains(parsed.EscapedPath(), "%") {
		return "", "", errors.New("Git credential URL must be a plain HTTPS repository URL")
	}
	return normalizeScope(parsed.Hostname(), strings.Trim(parsed.Path, "/"))
}

func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
