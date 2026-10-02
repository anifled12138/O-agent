package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	BaseURL          string `json:"baseUrl"`
	Model            string `json:"model"`
	APIKey           string `json:"apiKey"`
	ContextWindow    int    `json:"contextWindow"`
	SupportsVision   *bool  `json:"supportsVision"`
	SourceProviderID string `json:"sourceProviderId,omitempty"`
}

type ModelConfiguration struct {
	Model          string `json:"model"`
	ContextWindow  int    `json:"contextWindow"`
	SupportsVision bool   `json:"supportsVision"`
}

type BatchInput struct {
	Name             string               `json:"name"`
	Kind             string               `json:"kind"`
	BaseURL          string               `json:"baseUrl"`
	APIKey           string               `json:"apiKey"`
	SourceProviderID string               `json:"sourceProviderId,omitempty"`
	Models           []ModelConfiguration `json:"models"`
}
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	// ProviderItems carries an opaque provider-native input window. Only the
	// matching adapter may serialize it; application code must not inspect or
	// rewrite these items.
	ProviderItems []json.RawMessage `json:"providerItems,omitempty"`
	// Provider identity pins opaque Responses items to the exact source binding.
	ProviderID             string `json:"providerId,omitempty"`
	ProviderModel          string `json:"providerModel,omitempty"`
	ProviderTranscriptHash string `json:"providerTranscriptSha256,omitempty"`
	SourceID               string `json:"contextSourceId,omitempty"`
	Historical             bool   `json:"contextHistorical,omitempty"`
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
	// ProviderItems is the complete raw Responses output window. Keep each
	// item intact so opaque compaction and reasoning items round-trip unchanged.
	ProviderItems []json.RawMessage
	Model         string
	Usage         Usage
	Attempts      int
	Warnings      []CompatibilityWarning
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

func (s *Service) sealOrReuseProviderKey(ctx context.Context, userID, kind, baseURL, apiKey, sourceProviderID string, allowsBlankKey bool) ([]byte, []byte, bool, error) {
	if apiKey != "" {
		cipher, nonce, err := s.vault.Seal([]byte(apiKey))
		return cipher, nonce, true, err
	}
	if sourceProviderID != "" {
		source, cipher, nonce, err := s.store.ProviderSecret(ctx, userID, sourceProviderID)
		if err != nil || source.Kind != kind || source.BaseURL != baseURL {
			return nil, nil, false, domain.ErrInvalid
		}
		var plain []byte
		if len(cipher) > 0 {
			plain, err = s.vault.Open(cipher, nonce)
			if err != nil {
				return nil, nil, false, err
			}
		}
		if !allowsBlankKey && len(plain) == 0 {
			return nil, nil, false, domain.ErrInvalid
		}
		return cipher, nonce, len(plain) > 0, nil
	}
	if !allowsBlankKey {
		return nil, nil, false, domain.ErrInvalid
	}
	return nil, nil, false, nil
}

// SealRunCheckpoint protects the replay transcript and tool payloads at rest
// with the same local master key used for provider credentials.
func (s *Service) SealRunCheckpoint(plain []byte) ([]byte, []byte, error) {
	return s.vault.Seal(plain)
}

// SealRunCheckpointDeterministic creates repeatable ciphertext for immutable,
// content-addressed chunks so an interrupted artifact upload can resume.
func (s *Service) SealRunCheckpointDeterministic(plain, context []byte) ([]byte, []byte, error) {
	return s.vault.SealDeterministic(plain, context)
}

func (s *Service) OpenRunCheckpoint(ciphertext, nonce []byte) ([]byte, error) {
	return s.vault.Open(ciphertext, nonce)
}

func (s *Service) Create(ctx context.Context, userID string, in Input) (domain.Provider, error) {
	in.Name = strings.TrimSpace(in.Name)
	var ok bool
	in.Kind, ok = normalizeKind(in.Kind)
	in.Model = strings.TrimSpace(in.Model)
	baseURL, validURL := canonicalBaseURL(in.BaseURL)
	allowsBlankKey := in.Kind == KindOllama || in.Kind == KindVLLM || in.Kind == KindOpenAICompatible
	if in.Name == "" || in.Model == "" || (!allowsBlankKey && in.APIKey == "" && in.SourceProviderID == "") || !ok || !validURL || in.ContextWindow < 0 || in.ContextWindow > 2_000_000 {
		return domain.Provider{}, domain.ErrInvalid
	}
	in.BaseURL = baseURL
	cipher, nonce, hasKey, err := s.sealOrReuseProviderKey(ctx, userID, in.Kind, in.BaseURL, in.APIKey, in.SourceProviderID, allowsBlankKey)
	if err != nil {
		return domain.Provider{}, err
	}
	now := time.Now().UTC()
	vision := true
	if in.SupportsVision != nil {
		vision = *in.SupportsVision
	}
	p := domain.Provider{ID: newID(), UserID: userID, Name: in.Name, Kind: in.Kind, BaseURL: in.BaseURL, Model: in.Model, ContextWindow: in.ContextWindow, SupportsVision: &vision, HasAPIKey: hasKey, CreatedAt: now, UpdatedAt: now}
	if err := s.store.UpsertProvider(ctx, p, cipher, nonce); err != nil {
		return domain.Provider{}, err
	}
	saved, _, _, err := s.store.ProviderSecret(ctx, userID, p.ID)
	if err != nil || saved.Name != p.Name || saved.Kind != p.Kind || saved.BaseURL != p.BaseURL || saved.Model != p.Model || saved.ContextWindow != p.ContextWindow || saved.SupportsVision == nil || *saved.SupportsVision != vision {
		if err != nil {
			return domain.Provider{}, fmt.Errorf("provider created but could not verify saved model: %w", err)
		}
		return domain.Provider{}, errors.New("provider created but saved model settings did not match")
	}
	return saved, nil
}

func (s *Service) CreateBatch(ctx context.Context, userID string, in BatchInput) ([]domain.Provider, error) {
	in.Name = strings.TrimSpace(in.Name)
	kind, validKind := normalizeKind(in.Kind)
	baseURL, validURL := canonicalBaseURL(in.BaseURL)
	allowsBlankKey := kind == KindOllama || kind == KindVLLM || kind == KindOpenAICompatible
	if in.Name == "" || !validKind || !validURL || (!allowsBlankKey && strings.TrimSpace(in.APIKey) == "" && in.SourceProviderID == "") || len(in.Models) == 0 || len(in.Models) > 128 {
		return nil, domain.ErrInvalid
	}
	cipher, nonce, hasKey, err := s.sealOrReuseProviderKey(ctx, userID, kind, baseURL, in.APIKey, in.SourceProviderID, allowsBlankKey)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	records := make([]storage.ProviderRecord, 0, len(in.Models))
	seen := make(map[string]struct{}, len(in.Models))
	for _, configured := range in.Models {
		model := strings.TrimSpace(configured.Model)
		if model == "" || configured.ContextWindow < 1 || configured.ContextWindow > 2_000_000 {
			return nil, domain.ErrInvalid
		}
		if _, exists := seen[model]; exists {
			return nil, domain.ErrInvalid
		}
		seen[model] = struct{}{}
		vision := configured.SupportsVision
		p := domain.Provider{
			ID: newID(), UserID: userID, Name: in.Name + " · " + model, Kind: kind,
			BaseURL: baseURL, Model: model, ContextWindow: configured.ContextWindow,
			SupportsVision: &vision, HasAPIKey: hasKey, CreatedAt: now, UpdatedAt: now,
		}
		records = append(records, storage.ProviderRecord{Provider: p, Cipher: append([]byte(nil), cipher...), Nonce: append([]byte(nil), nonce...)})
	}
	if err := s.store.CreateProviders(ctx, records); err != nil {
		return nil, err
	}

	// Read the committed rows back so the API only reports models that the
	// provider store can observe after the transaction has completed.
	created := make([]domain.Provider, 0, len(records))
	for _, record := range records {
		p, _, _, err := s.store.ProviderSecret(ctx, userID, record.Provider.ID)
		if err != nil {
			return nil, fmt.Errorf("provider batch committed but could not verify model %q: %w", record.Provider.Model, err)
		}
		if p.Name != record.Provider.Name || p.Kind != record.Provider.Kind || p.BaseURL != record.Provider.BaseURL || p.Model != record.Provider.Model || p.ContextWindow != record.Provider.ContextWindow || p.SupportsVision == nil || *p.SupportsVision != *record.Provider.SupportsVision {
			return nil, fmt.Errorf("provider batch committed but model %q did not match its saved settings", record.Provider.Model)
		}
		created = append(created, p)
	}
	return created, nil
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
	if in.SupportsVision != nil {
		existing.SupportsVision = in.SupportsVision
	}
	existing.HasAPIKey = len(cipher) > 0
	existing.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateProvider(ctx, existing, cipher, nonce); err != nil {
		return domain.Provider{}, err
	}
	saved, _, _, err := s.store.ProviderSecret(ctx, userID, id)
	if err != nil {
		return domain.Provider{}, fmt.Errorf("provider updated but could not verify saved settings: %w", err)
	}
	if saved.Name != existing.Name || saved.Kind != existing.Kind || saved.BaseURL != existing.BaseURL || saved.Model != existing.Model || saved.ContextWindow != existing.ContextWindow || (existing.SupportsVision == nil && saved.SupportsVision != nil) || (existing.SupportsVision != nil && (saved.SupportsVision == nil || *saved.SupportsVision != *existing.SupportsVision)) {
		return domain.Provider{}, errors.New("provider updated but saved settings did not match")
	}
	return saved, nil
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
	contextWindow := p.ContextWindow
	if contextWindow <= 0 {
		contextWindow = 131072
	}
	inputTokens := EstimateInputTokens(messages, tools)
	inputBudget := contextWindow * 75 / 100
	if inputBudget < 1 {
		inputBudget = 1
	}
	if inputTokens > inputBudget {
		return Completion{}, &ProviderError{Class: ErrorContextOverflow, SafeDetail: fmt.Sprintf("estimated model input (%d tokens) exceeds the preflight budget (%d of %d context tokens)", inputTokens, inputBudget, contextWindow)}
	}
	if p.SupportsVision != nil && !*p.SupportsVision {
		for _, message := range messages {
			if imageMarkdownRegex.MatchString(message.Content) {
				return Completion{}, errors.New("the selected model is configured for text-only input; remove image attachments or enable vision for this model")
			}
		}
	}
	adapter, err := adapterFor(p.Kind)
	if err != nil {
		return Completion{}, err
	}
	for _, message := range messages {
		if len(message.ProviderItems) > 0 && p.Kind != KindOpenAIResponses {
			return Completion{}, &ProviderError{Class: ErrorProtocol, SafeDetail: "provider-native context state is incompatible with the selected provider protocol"}
		}
	}
	req, err := adapter.completionRequest(ctx, p, key, messages, tools)
	if err != nil {
		return Completion{}, err
	}
	if observer := requestAuditObserver(ctx); observer != nil {
		if req.GetBody == nil {
			return Completion{}, errors.New("provider adapter did not expose a replayable serialized request for audit")
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return Completion{}, fmt.Errorf("read serialized provider request for audit: %w", bodyErr)
		}
		digest := sha256.New()
		requestBytes, readErr := io.Copy(digest, body)
		closeErr := body.Close()
		if readErr != nil || closeErr != nil {
			return Completion{}, fmt.Errorf("read serialized provider request for audit: %w", errors.Join(readErr, closeErr))
		}
		if auditErr := observer(RequestAudit{PayloadSHA256: hex.EncodeToString(digest.Sum(nil)), RequestBytes: int(requestBytes)}); auditErr != nil {
			return Completion{}, fmt.Errorf("model request pre-send audit failed: %w", auditErr)
		}
	}
	resp, attempts, err := s.doCompletionRequest(req)
	if err != nil {
		return Completion{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Completion{}, &ProviderError{Class: ErrorProtocol, Attempts: attempts, SafeDetail: "could not read the model provider response", Cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		providerErr := asProviderError(httpProviderError(resp, raw))
		providerErr.Attempts = attempts
		return Completion{}, providerErr
	}
	if !json.Valid(raw) {
		return Completion{}, &ProviderError{Class: ErrorProtocol, StatusCode: resp.StatusCode, Attempts: attempts, SafeDetail: fmt.Sprintf("provider returned non-JSON content from %s (%s); check the API base URL", req.URL.String(), responseType(resp))}
	}
	completion, err := adapter.decodeCompletion(raw)
	if err != nil {
		return Completion{}, &ProviderError{Class: ErrorProtocol, StatusCode: resp.StatusCode, Attempts: attempts, SafeDetail: err.Error(), Cause: err}
	}
	completion.Attempts = attempts
	return completion, nil
}

// CompactResponses asks the Responses API to compact a historical window and
// returns every item in the resulting canonical window without decoding or
// filtering provider-owned state.
func (s *Service) CompactResponses(ctx context.Context, userID, id string, messages []ChatMessage) (Completion, error) {
	p, key, err := s.secret(ctx, userID, id)
	if err != nil {
		return Completion{}, err
	}
	if p.Kind != KindOpenAIResponses {
		return Completion{}, &ProviderError{Class: ErrorProtocol, SafeDetail: "native compaction requires the OpenAI Responses protocol"}
	}
	adapter, err := adapterFor(p.Kind)
	if err != nil {
		return Completion{}, err
	}
	responses, ok := adapter.(interface {
		compactionRequest(context.Context, domain.Provider, string, []ChatMessage) (*http.Request, error)
		decodeCompaction([]byte) (Completion, error)
	})
	if !ok {
		return Completion{}, errors.New("Responses adapter does not support native compaction")
	}
	window := p.ContextWindow
	if window <= 0 {
		window = 131072
	}
	if estimate := EstimateInputTokens(messages, nil); estimate > window {
		return Completion{}, &ProviderError{Class: ErrorContextOverflow, SafeDetail: fmt.Sprintf("native compaction input estimate (%d tokens) exceeds the configured context window (%d)", estimate, window)}
	}
	req, err := responses.compactionRequest(ctx, p, key, messages)
	if err != nil {
		return Completion{}, err
	}
	if observer := requestAuditObserver(ctx); observer != nil {
		if req.GetBody == nil {
			return Completion{}, errors.New("Responses compact adapter did not expose a replayable request for audit")
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return Completion{}, fmt.Errorf("read serialized Responses compact request for audit: %w", bodyErr)
		}
		digest := sha256.New()
		requestBytes, readErr := io.Copy(digest, body)
		closeErr := body.Close()
		if readErr != nil || closeErr != nil {
			return Completion{}, fmt.Errorf("read serialized Responses compact request for audit: %w", errors.Join(readErr, closeErr))
		}
		if auditErr := observer(RequestAudit{PayloadSHA256: hex.EncodeToString(digest.Sum(nil)), RequestBytes: int(requestBytes)}); auditErr != nil {
			return Completion{}, fmt.Errorf("Responses compact pre-send audit failed: %w", auditErr)
		}
	}
	resp, attempts, err := s.doCompletionRequest(req)
	if err != nil {
		return Completion{Attempts: attempts}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Completion{Attempts: attempts}, &ProviderError{Class: ErrorProtocol, Attempts: attempts, SafeDetail: "could not read the Responses compact result", Cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		providerErr := asProviderError(httpProviderError(resp, raw))
		providerErr.Attempts = attempts
		return Completion{Attempts: attempts}, providerErr
	}
	if !json.Valid(raw) {
		return Completion{Attempts: attempts}, &ProviderError{Class: ErrorProtocol, StatusCode: resp.StatusCode, Attempts: attempts, SafeDetail: "Responses compact endpoint returned non-JSON content"}
	}
	completion, err := responses.decodeCompaction(raw)
	completion.Attempts = attempts
	if err != nil {
		return completion, &ProviderError{Class: ErrorProtocol, StatusCode: resp.StatusCode, Attempts: attempts, SafeDetail: err.Error(), Cause: err}
	}
	return completion, nil
}

const (
	maxProviderAttempts  = MaxCompletionAttempts
	maxProviderRetryWait = 5 * time.Second
)

func (s *Service) doCompletionRequest(request *http.Request) (*http.Response, int, error) {
	attemptLimit := maxProviderAttempts
	if budget, ok := attemptBudget(request.Context()); ok && budget < attemptLimit {
		attemptLimit = budget
	}
	for attempt := 1; attempt <= attemptLimit; attempt++ {
		current := request.Clone(request.Context())
		if request.Body != nil {
			if request.GetBody == nil {
				return nil, attempt, errors.New("provider request body cannot be replayed safely")
			}
			body, err := request.GetBody()
			if err != nil {
				return nil, attempt, err
			}
			current.Body = body
		}
		resp, err := s.client.Do(current)
		if err != nil {
			providerErr := transportError(err).(*ProviderError)
			providerErr.Attempts = attempt
			if attempt == attemptLimit || !retryableProviderError(providerErr) {
				return nil, attempt, providerErr
			}
			if waitErr := waitProviderRetry(request.Context(), providerRetryDelay(attempt, 0)); waitErr != nil {
				providerErr.Cause = errors.Join(providerErr.Cause, waitErr)
				return nil, attempt, providerErr
			}
			continue
		}
		if resp.StatusCode < http.StatusInternalServerError && resp.StatusCode != http.StatusTooManyRequests {
			return resp, attempt, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, attempt, &ProviderError{Class: ErrorProtocol, StatusCode: resp.StatusCode, Attempts: attempt, SafeDetail: "could not read the model provider error response", Cause: readErr}
		}
		providerErr := asProviderError(httpProviderError(resp, body))
		providerErr.Attempts = attempt
		if attempt == attemptLimit || !retryableProviderError(providerErr) || providerErr.RetryAfter > maxProviderRetryWait {
			return nil, attempt, providerErr
		}
		if waitErr := waitProviderRetry(request.Context(), providerRetryDelay(attempt, providerErr.RetryAfter)); waitErr != nil {
			providerErr.Cause = errors.Join(providerErr.Cause, waitErr)
			return nil, attempt, providerErr
		}
	}
	return nil, attemptLimit, &ProviderError{Class: ErrorUnavailable, Attempts: attemptLimit, SafeDetail: "model provider request failed after bounded retries"}
}

func retryableProviderError(err *ProviderError) bool {
	return err.Class == ErrorUnavailable || err.Class == ErrorRateLimit
}

func asProviderError(err error) *ProviderError {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return providerErr
	}
	return &ProviderError{Class: ErrorProtocol, SafeDetail: err.Error(), Cause: err}
}

func providerRetryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	return time.Duration(attempt*attempt) * 250 * time.Millisecond
}

func waitProviderRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 || delay > maxProviderRetryWait {
		return errors.New("provider retry delay exceeds the configured limit")
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
	if len(cipher) == 0 {
		return p, "", nil
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
