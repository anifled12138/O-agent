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
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type ToolDefinition struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}
type Completion struct {
	Content   string
	ToolCalls []ToolCall
	Model     string
	Usage     Usage
}
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
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
	result, err := s.CompleteWithTools(ctx, userID, id, messages, nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Content) == "" {
		return "", errors.New("provider returned no assistant message")
	}
	return result.Content, nil
}

func (s *Service) CompleteWithTools(ctx context.Context, userID, id string, messages []ChatMessage, tools []ToolDefinition) (Completion, error) {
	p, key, err := s.secret(ctx, userID, id)
	if err != nil {
		return Completion{}, err
	}
	payload := map[string]any{"model": p.Model, "messages": messages, "temperature": 0.2}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Completion{}, err
	}
	endpoint := apiEndpoint(p.BaseURL, "/chat/completions")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Completion{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return Completion{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Completion{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Completion{}, fmt.Errorf("provider returned %s from %s: %s", resp.Status, endpoint, bodyPreview(raw))
	}
	if !json.Valid(raw) {
		return Completion{}, fmt.Errorf("provider returned non-JSON content from %s (%s); check the API base URL", endpoint, responseType(resp))
	}
	var result struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return Completion{}, err
	}
	if result.Error != nil {
		return Completion{}, errors.New(result.Error.Message)
	}
	if len(result.Choices) == 0 {
		return Completion{}, errors.New("provider returned no assistant choice")
	}
	message := result.Choices[0].Message
	if strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
		return Completion{}, errors.New("provider returned neither content nor tool calls")
	}
	return Completion{Content: message.Content, ToolCalls: message.ToolCalls, Model: result.Model, Usage: Usage{PromptTokens: result.Usage.PromptTokens, CompletionTokens: result.Usage.CompletionTokens, TotalTokens: result.Usage.TotalTokens}}, nil
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
