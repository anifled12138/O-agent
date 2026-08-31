package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

type Input struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"baseUrl"`
	Model   string `json:"model"`
	APIKey  string `json:"apiKey"`
}
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type Service struct {
	store  *storage.Store
	vault  *secure.Vault
	client *http.Client
}

func New(store *storage.Store, vault *secure.Vault) *Service {
	return &Service{store: store, vault: vault, client: &http.Client{Timeout: 90 * time.Second}}
}

func (s *Service) Create(ctx context.Context, userID string, in Input) (domain.Provider, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Kind = strings.TrimSpace(in.Kind)
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	in.Model = strings.TrimSpace(in.Model)
	if in.Kind == "" {
		in.Kind = "openai-compatible"
	}
	if in.Name == "" || in.Model == "" || in.APIKey == "" || !validBaseURL(in.BaseURL) {
		return domain.Provider{}, domain.ErrInvalid
	}
	cipher, nonce, err := s.vault.Seal([]byte(in.APIKey))
	if err != nil {
		return domain.Provider{}, err
	}
	now := time.Now().UTC()
	p := domain.Provider{ID: newID(), UserID: userID, Name: in.Name, Kind: in.Kind, BaseURL: in.BaseURL, Model: in.Model, HasAPIKey: true, CreatedAt: now, UpdatedAt: now}
	return p, s.store.UpsertProvider(ctx, p, cipher, nonce)
}

func (s *Service) List(ctx context.Context, userID string) ([]domain.Provider, error) {
	return s.store.ListProviders(ctx, userID)
}

func (s *Service) Test(ctx context.Context, userID, id string) error {
	p, key, err := s.secret(ctx, userID, id)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("provider returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (s *Service) Complete(ctx context.Context, userID, id string, messages []ChatMessage) (string, error) {
	p, key, err := s.secret(ctx, userID, id)
	if err != nil {
		return "", err
	}
	payload := map[string]any{"model": p.Model, "messages": messages, "temperature": 0.2}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("provider returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if result.Error != nil {
		return "", errors.New(result.Error.Message)
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return "", errors.New("provider returned no assistant message")
	}
	return result.Choices[0].Message.Content, nil
}

func (s *Service) secret(ctx context.Context, userID, id string) (domain.Provider, string, error) {
	p, cipher, nonce, err := s.store.ProviderSecret(ctx, userID, id)
	if err != nil {
		return p, "", err
	}
	plain, err := s.vault.Open(cipher, nonce)
	return p, string(plain), err
}
func validBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}
func newID() string { return fmt.Sprintf("prv_%d", time.Now().UnixNano()) }
