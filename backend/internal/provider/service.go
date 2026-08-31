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
	"sort"
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
	in.Model = strings.TrimSpace(in.Model)
	if in.Kind == "" {
		in.Kind = "openai-compatible"
	}
	baseURL, ok := canonicalBaseURL(in.BaseURL)
	if in.Name == "" || in.Model == "" || in.APIKey == "" || !ok {
		return domain.Provider{}, domain.ErrInvalid
	}
	in.BaseURL = baseURL
	cipher, nonce, err := s.vault.Seal([]byte(in.APIKey))
	if err != nil {
		return domain.Provider{}, err
	}
	now := time.Now().UTC()
	p := domain.Provider{ID: newID(), UserID: userID, Name: in.Name, Kind: in.Kind, BaseURL: in.BaseURL, Model: in.Model, HasAPIKey: true, CreatedAt: now, UpdatedAt: now}
	return p, s.store.UpsertProvider(ctx, p, cipher, nonce)
}

func (s *Service) Update(ctx context.Context, userID, id string, in Input) (domain.Provider, error) {
	existing, cipher, nonce, err := s.store.ProviderSecret(ctx, userID, id)
	if err != nil {
		return domain.Provider{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Kind = strings.TrimSpace(in.Kind)
	in.Model = strings.TrimSpace(in.Model)
	if in.Kind == "" {
		in.Kind = "openai-compatible"
	}
	baseURL, ok := canonicalBaseURL(in.BaseURL)
	if in.Name == "" || in.Model == "" || !ok {
		return domain.Provider{}, domain.ErrInvalid
	}
	if in.APIKey != "" {
		cipher, nonce, err = s.vault.Seal([]byte(in.APIKey))
		if err != nil {
			return domain.Provider{}, err
		}
	}
	existing.Name = in.Name
	existing.Kind = in.Kind
	existing.BaseURL = baseURL
	existing.Model = in.Model
	existing.HasAPIKey = len(cipher) > 0
	existing.UpdatedAt = time.Now().UTC()
	return existing, s.store.UpdateProvider(ctx, existing, cipher, nonce)
}

func (s *Service) List(ctx context.Context, userID string) ([]domain.Provider, error) {
	return s.store.ListProviders(ctx, userID)
}

func (s *Service) Test(ctx context.Context, userID, id string) error {
	p, key, err := s.secret(ctx, userID, id)
	if err != nil {
		return err
	}
	endpoint := apiEndpoint(p.BaseURL, "/models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider returned %s from %s: %s", resp.Status, endpoint, bodyPreview(body))
	}
	if !json.Valid(body) {
		return fmt.Errorf("provider returned non-JSON content from %s (%s); check the API base URL", endpoint, responseType(resp))
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &models); err == nil && len(models.Data) > 0 {
		available := make([]string, 0, len(models.Data))
		found := false
		for _, model := range models.Data {
			available = append(available, model.ID)
			found = found || model.ID == p.Model
		}
		if !found {
			return fmt.Errorf("model %q is not listed by the provider; nearby models: %s", p.Model, strings.Join(modelExamples(available, p.Model, 8), ", "))
		}
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
	endpoint := apiEndpoint(p.BaseURL, "/chat/completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
		return "", fmt.Errorf("provider returned %s from %s: %s", resp.Status, endpoint, bodyPreview(raw))
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("provider returned non-JSON content from %s (%s); check the API base URL", endpoint, responseType(resp))
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
func canonicalBaseURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = "/v1"
	}
	return strings.TrimRight(u.String(), "/"), true
}

func apiEndpoint(baseURL, path string) string {
	canonical, ok := canonicalBaseURL(baseURL)
	if !ok {
		canonical = strings.TrimRight(baseURL, "/")
	}
	return canonical + path
}

func responseType(resp *http.Response) string {
	if value := resp.Header.Get("Content-Type"); value != "" {
		return value
	}
	return "unknown content type"
}

func bodyPreview(body []byte) string {
	value := strings.TrimSpace(string(body))
	if strings.HasPrefix(value, "<") {
		return "HTML response"
	}
	if len(value) > 512 {
		return value[:512] + "…"
	}
	return value
}

func modelExamples(ids []string, target string, limit int) []string {
	sort.Strings(ids)
	family := strings.SplitN(target, "-", 2)[0]
	result := make([]string, 0, limit)
	for _, id := range ids {
		if strings.HasPrefix(id, family+"-") {
			result = append(result, id)
			if len(result) == limit {
				return result
			}
		}
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, family+"-") {
			result = append(result, id)
			if len(result) == limit {
				return result
			}
		}
	}
	return result
}
func newID() string { return fmt.Sprintf("prv_%d", time.Now().UnixNano()) }
