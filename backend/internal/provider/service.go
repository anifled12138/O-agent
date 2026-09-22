package provider

import (
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
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	BaseURL       string `json:"baseUrl"`
	Model         string `json:"model"`
	APIKey        string `json:"apiKey"`
	ContextWindow int    `json:"contextWindow"`
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
	Name                      string `json:"name"`
	Arguments                 string `json:"arguments"`
	normalizedObjectArguments bool
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
	Warnings  []CompatibilityWarning
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
	var ok bool
	in.Kind, ok = normalizeKind(in.Kind)
	in.Model = strings.TrimSpace(in.Model)
	baseURL, validURL := canonicalBaseURL(in.BaseURL)
	allowsBlankKey := in.Kind == KindOllama || in.Kind == KindVLLM || in.Kind == KindOpenAICompatible
	if in.Name == "" || in.Model == "" || (!allowsBlankKey && in.APIKey == "") || !ok || !validURL || in.ContextWindow < 0 || in.ContextWindow > 2_000_000 {
		return domain.Provider{}, domain.ErrInvalid
	}
	in.BaseURL = baseURL
	cipher, nonce, err := s.vault.Seal([]byte(in.APIKey))
	if err != nil {
		return domain.Provider{}, err
	}
	now := time.Now().UTC()
	p := domain.Provider{ID: newID(), UserID: userID, Name: in.Name, Kind: in.Kind, BaseURL: in.BaseURL, Model: in.Model, ContextWindow: in.ContextWindow, HasAPIKey: len(cipher) > 0, CreatedAt: now, UpdatedAt: now}
	return p, s.store.UpsertProvider(ctx, p, cipher, nonce)
}

func (s *Service) Update(ctx context.Context, userID, id string, in Input) (domain.Provider, error) {
	existing, cipher, nonce, err := s.store.ProviderSecret(ctx, userID, id)
	if err != nil {
		return domain.Provider{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if strings.TrimSpace(in.Kind) == "" {
		in.Kind = existing.Kind
	}
	var validKind bool
	in.Kind, validKind = normalizeKind(in.Kind)
	in.Model = strings.TrimSpace(in.Model)
	baseURL, ok := canonicalBaseURL(in.BaseURL)
	if in.Name == "" || in.Model == "" || !ok || !validKind || in.ContextWindow < 0 || in.ContextWindow > 2_000_000 {
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
	if in.ContextWindow > 0 {
		existing.ContextWindow = in.ContextWindow
	}
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
	adapter, err := adapterFor(p.Kind)
	if err != nil {
		return err
	}
	req, err := adapter.modelsRequest(ctx, p, key)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpProviderError(resp, body)
	}
	if !json.Valid(body) {
		return &ProviderError{Class: ErrorProtocol, StatusCode: resp.StatusCode, SafeDetail: fmt.Sprintf("provider returned non-JSON content from %s (%s); check the API base URL", req.URL.String(), responseType(resp))}
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	var ollamaTags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &ollamaTags); err == nil && len(ollamaTags.Models) > 0 {
		for _, m := range ollamaTags.Models {
			id := m.Name
			if id == "" {
				id = m.Model
			}
			if id != "" {
				models.Data = append(models.Data, struct {
					ID string `json:"id"`
				}{ID: id})
			}
		}
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

func (s *Service) ProviderModels(ctx context.Context, userID, id string) ([]string, error) {
	p, key, err := s.secret(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	return s.ProbeModels(ctx, p.BaseURL, key)
}

func (s *Service) ProbeModels(ctx context.Context, rawBaseURL, key string) ([]string, error) {
	baseURL, ok := canonicalBaseURL(rawBaseURL)
	if !ok {
		return nil, domain.ErrInvalid
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	bearer(req, key)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpProviderError(resp, body)
	}
	if !json.Valid(body) {
		return nil, &ProviderError{Class: ErrorProtocol, StatusCode: resp.StatusCode, SafeDetail: fmt.Sprintf("provider returned non-JSON content from %s (%s); check the API base URL", req.URL.String(), responseType(resp))}
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	var ollamaTags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &ollamaTags); err == nil && len(ollamaTags.Models) > 0 {
		for _, m := range ollamaTags.Models {
			id := m.Name
			if id == "" {
				id = m.Model
			}
			if id != "" {
				models.Data = append(models.Data, struct {
					ID string `json:"id"`
				}{ID: id})
			}
		}
	}
	_ = json.Unmarshal(body, &models)
	var list []string
	seen := make(map[string]bool)
	for _, m := range models.Data {
		name := strings.TrimSpace(m.ID)
		if name != "" && !seen[name] {
			seen[name] = true
			list = append(list, name)
		}
	}
	sort.Strings(list)
	return list, nil
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
	adapter, err := adapterFor(p.Kind)
	if err != nil {
		return Completion{}, err
	}
	req, err := adapter.completionRequest(ctx, p, key, messages, tools)
	if err != nil {
		return Completion{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Completion{}, transportError(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Completion{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Completion{}, httpProviderError(resp, raw)
	}
	if !json.Valid(raw) {
		return Completion{}, &ProviderError{Class: ErrorProtocol, StatusCode: resp.StatusCode, SafeDetail: fmt.Sprintf("provider returned non-JSON content from %s (%s); check the API base URL", req.URL.String(), responseType(resp))}
	}
	completion, err := adapter.decodeCompletion(raw)
	if err != nil {
		return Completion{}, &ProviderError{Class: ErrorProtocol, StatusCode: resp.StatusCode, SafeDetail: err.Error(), Cause: err}
	}
	return completion, nil
}

func transportError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &ProviderError{Class: ErrorCancelled, SafeDetail: err.Error(), Cause: err}
	}
	return &ProviderError{Class: ErrorUnavailable, SafeDetail: "could not reach the model provider", Cause: err}
}

func httpProviderError(resp *http.Response, body []byte) error {
	class := ErrorInvalidRequest
	lowerBody := strings.ToLower(string(body))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		class = ErrorAuthentication
	case resp.StatusCode == http.StatusNotFound:
		class = ErrorNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		class = ErrorRateLimit
	case resp.StatusCode >= 500:
		class = ErrorUnavailable
	case strings.Contains(lowerBody, "context_length_exceeded") || strings.Contains(lowerBody, "context window") || strings.Contains(lowerBody, "too many tokens"):
		class = ErrorContextOverflow
	case strings.Contains(lowerBody, "unsupported") || strings.Contains(lowerBody, "not supported"):
		class = ErrorUnsupported
	}
	detail := fmt.Sprintf("provider returned %s from %s: %s", resp.Status, resp.Request.URL.String(), bodyPreview(body))
	return &ProviderError{Class: class, StatusCode: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After")), SafeDetail: detail}
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if wait, err := time.ParseDuration(value + "s"); err == nil && wait > 0 {
		return wait
	}
	if when, err := http.ParseTime(value); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
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
