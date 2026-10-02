package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"axiom.local/agent/internal/domain"
	pluginruntime "axiom.local/agent/internal/plugins"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/storage"
)

const (
	compactionTriggerPercent = 75
	largeSnapshotChunkBytes  = 4 << 20
)

type chunkedSnapshotDescriptor struct {
	Format       string `json:"format"`
	ArtifactID   string `json:"artifactId"`
	PlainSize    int64  `json:"plainSize"`
	PlainSHA256  string `json:"plainSha256"`
	ChunkBytes   int64  `json:"chunkBytes"`
	ChunkCount   int64  `json:"chunkCount"`
	AEADOverhead int64  `json:"aeadOverhead"`
	NonceBytes   int64  `json:"nonceBytes"`
}

type runePageWriter struct {
	offset  int
	limit   int
	seen    int
	pending []byte
	page    strings.Builder
}

func (w *runePageWriter) Write(p []byte) (int, error) {
	n := len(p)
	data := append(w.pending, p...)
	w.pending = nil
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			w.pending = append(w.pending, data...)
			break
		}
		r, size := utf8.DecodeRune(data)
		if w.seen >= w.offset && w.seen-w.offset < w.limit {
			w.page.WriteRune(r)
		}
		w.seen++
		data = data[size:]
	}
	return n, nil
}

func (w *runePageWriter) finish() {
	for len(w.pending) > 0 {
		r, size := utf8.DecodeRune(w.pending)
		if w.seen >= w.offset && w.seen-w.offset < w.limit {
			w.page.WriteRune(r)
		}
		w.seen++
		w.pending = w.pending[size:]
	}
}

func (s *turnScope) readContextSourcePage(ctx context.Context, conversationID, sourceID string, offset, limit int) (string, string, int, error) {
	if offset < 0 || limit < 1 {
		return "", "", 0, domain.ErrInvalid
	}
	if s != nil && s.evaluation {
		content, hash, err := s.readContextSourceInConversation(ctx, conversationID, sourceID)
		if err != nil {
			return "", "", 0, err
		}
		runes := []rune(content)
		end := min(offset+limit, len(runes))
		start := min(offset, len(runes))
		return string(runes[start:end]), hash, len(runes), nil
	}
	_, archived, err := s.owner.store.ReadContextSource(ctx, s.userID, conversationID, sourceID)
	if err != nil {
		return "", "", 0, err
	}
	if archived.SourceID != "" {
		payload, err := s.owner.providers.OpenRunCheckpoint(archived.Ciphertext, archived.Nonce)
		if err != nil {
			return "", "", 0, err
		}
		var descriptor chunkedSnapshotDescriptor
		if json.Unmarshal(payload, &descriptor) == nil && descriptor.Format == "encrypted_chunked_snapshot_v1" {
			writer := &runePageWriter{offset: offset, limit: limit}
			if err := s.streamChunkedSnapshot(ctx, descriptor, writer); err != nil {
				return "", "", 0, err
			}
			writer.finish()
			return writer.page.String(), descriptor.PlainSHA256, writer.seen, nil
		}
	}
	content, hash, err := s.readContextSourceInConversation(ctx, conversationID, sourceID)
	if err != nil {
		return "", "", 0, err
	}
	runes := []rune(content)
	start := min(offset, len(runes))
	end := min(start+limit, len(runes))
	return string(runes[start:end]), hash, len(runes), nil
}

func (s *turnScope) archiveLargeToolSource(ctx context.Context, sourceType string, reader io.Reader, size int64, plainSHA256 string) (string, error) {
	if s == nil || s.owner == nil || s.owner.artifacts == nil || s.owner.providers == nil || s.owner.store == nil {
		return "", errors.New("chunked encrypted snapshot archive is unavailable")
	}
	if sourceType != "file_snapshot" || size < 0 || len(plainSHA256) != sha256.Size*2 {
		return "", domain.ErrInvalid
	}
	sourceID := sourceType + ":" + plainSHA256
	chunkCount := size / largeSnapshotChunkBytes
	if size%largeSnapshotChunkBytes != 0 {
		chunkCount++
	}
	probe, probeNonce, err := s.owner.providers.SealRunCheckpoint(nil)
	if err != nil {
		return "", fmt.Errorf("initialize snapshot encryption: %w", err)
	}
	overhead := int64(len(probe) + len(probeNonce))
	encodedSize := size + chunkCount*overhead
	pipeReader, pipeWriter := io.Pipe()
	producerDone := make(chan error, 1)
	go func() {
		plainHasher := sha256.New()
		var readTotal int64
		var writeErr error
		for index := int64(0); index < chunkCount; index++ {
			if err := ctx.Err(); err != nil {
				writeErr = err
				break
			}
			want := int64(largeSnapshotChunkBytes)
			if remaining := size - readTotal; remaining < want {
				want = remaining
			}
			plain := make([]byte, want)
			if _, writeErr = io.ReadFull(reader, plain); writeErr != nil {
				break
			}
			readTotal += want
			_, _ = plainHasher.Write(plain)
			nonceContext := []byte(fmt.Sprintf("%s\x00%d", sourceID, index))
			ciphertext, nonce, sealErr := s.owner.providers.SealRunCheckpointDeterministic(plain, nonceContext)
			if sealErr != nil {
				writeErr = sealErr
				break
			}
			if int64(len(ciphertext)+len(nonce)) != want+overhead {
				writeErr = errors.New("snapshot encryption produced an unexpected chunk size")
				break
			}
			if _, writeErr = io.Copy(pipeWriter, bytes.NewReader(nonce)); writeErr != nil {
				break
			}
			if _, writeErr = io.Copy(pipeWriter, bytes.NewReader(ciphertext)); writeErr != nil {
				break
			}
		}
		if writeErr == nil && readTotal != size {
			writeErr = domain.ErrConflict
		}
		if writeErr == nil && hex.EncodeToString(plainHasher.Sum(nil)) != plainSHA256 {
			writeErr = fmt.Errorf("snapshot plaintext hash mismatch: %w", domain.ErrConflict)
		}
		if writeErr == nil {
			var extra [1]byte
			if n, readErr := reader.Read(extra[:]); n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
				writeErr = errors.Join(domain.ErrConflict, readErr)
			}
		}
		if writeErr != nil {
			_ = pipeWriter.CloseWithError(writeErr)
		} else {
			_ = pipeWriter.Close()
		}
		producerDone <- writeErr
	}()
	artifact, storeErr := s.owner.artifacts.StoreFromReader(ctx, s.userID, "snapshot.enc", "application/octet-stream", "context-source-"+plainSHA256, encodedSize, "", pipeReader, time.Now().UTC())
	closeErr := pipeReader.Close()
	producerErr := <-producerDone
	if err := errors.Join(storeErr, closeErr, producerErr); err != nil {
		return "", fmt.Errorf("store encrypted snapshot artifact: %w", err)
	}
	descriptor := chunkedSnapshotDescriptor{Format: "encrypted_chunked_snapshot_v1", ArtifactID: artifact.ID, PlainSize: size, PlainSHA256: plainSHA256, ChunkBytes: largeSnapshotChunkBytes, ChunkCount: chunkCount, AEADOverhead: overhead, NonceBytes: int64(len(probeNonce))}
	payload, err := json.Marshal(descriptor)
	if err != nil {
		return "", err
	}
	ciphertext, nonce, err := s.owner.providers.SealRunCheckpoint(payload)
	if err != nil {
		return "", fmt.Errorf("encrypt snapshot descriptor: %w", err)
	}
	if err := s.owner.store.SaveContextSource(ctx, s.userID, s.conversationID, storage.ContextSourceCiphertext{SourceID: sourceID, SourceType: sourceType, ContentHash: plainSHA256, Ciphertext: ciphertext, Nonce: nonce}); err != nil {
		return "", err
	}
	_, persisted, err := s.owner.store.ReadContextSource(ctx, s.userID, s.conversationID, sourceID)
	if err != nil {
		return "", fmt.Errorf("read encrypted snapshot descriptor back: %w", err)
	}
	persistedPayload, err := s.owner.providers.OpenRunCheckpoint(persisted.Ciphertext, persisted.Nonce)
	if err != nil {
		return "", fmt.Errorf("decrypt snapshot descriptor read-back: %w", err)
	}
	var persistedDescriptor chunkedSnapshotDescriptor
	if err := json.Unmarshal(persistedPayload, &persistedDescriptor); err != nil || persistedDescriptor != descriptor || persisted.ContentHash != plainSHA256 {
		return "", fmt.Errorf("snapshot descriptor read-back verification failed: %w", errors.Join(err, domain.ErrConflict))
	}
	if err := s.verifyChunkedSnapshot(ctx, persistedDescriptor); err != nil {
		return "", fmt.Errorf("verify encrypted snapshot artifact read-back: %w", err)
	}
	return sourceID, nil
}

func (s *turnScope) verifyChunkedSnapshot(ctx context.Context, descriptor chunkedSnapshotDescriptor) error {
	return s.streamChunkedSnapshot(ctx, descriptor, io.Discard)
}

func (s *turnScope) streamChunkedSnapshot(ctx context.Context, descriptor chunkedSnapshotDescriptor, destination io.Writer) (retErr error) {
	if s == nil || s.owner == nil || s.owner.artifacts == nil || descriptor.Format != "encrypted_chunked_snapshot_v1" || descriptor.ChunkBytes <= 0 || descriptor.ChunkCount < 0 || descriptor.AEADOverhead <= 0 {
		return domain.ErrInvalid
	}
	artifact, file, err := s.owner.artifacts.Open(ctx, s.userID, descriptor.ArtifactID)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	if artifact.ByteSize != descriptor.PlainSize+descriptor.ChunkCount*descriptor.AEADOverhead {
		return domain.ErrConflict
	}
	hasher := sha256.New()
	var total int64
	for index := int64(0); index < descriptor.ChunkCount; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		plainSize := descriptor.ChunkBytes
		if remaining := descriptor.PlainSize - total; remaining < plainSize {
			plainSize = remaining
		}
		if descriptor.NonceBytes <= 0 || descriptor.AEADOverhead <= descriptor.NonceBytes {
			return domain.ErrInvalid
		}
		nonce := make([]byte, descriptor.NonceBytes)
		if _, err := io.ReadFull(file, nonce); err != nil {
			return err
		}
		sealed := make([]byte, plainSize+descriptor.AEADOverhead-descriptor.NonceBytes)
		if _, err := io.ReadFull(file, sealed); err != nil {
			return err
		}
		plain, err := s.owner.providers.OpenRunCheckpoint(sealed, nonce)
		if err != nil {
			return fmt.Errorf("decrypt snapshot chunk %d: %w", index, err)
		}
		if int64(len(plain)) != plainSize {
			return domain.ErrConflict
		}
		if _, err := destination.Write(plain); err != nil {
			return err
		}
		_, _ = hasher.Write(plain)
		total += plainSize
	}
	if total != descriptor.PlainSize || hex.EncodeToString(hasher.Sum(nil)) != descriptor.PlainSHA256 {
		return domain.ErrConflict
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return errors.Join(domain.ErrConflict, err)
	}
	return nil
}

func (s *turnScope) contextCompactorPlugin() *pluginruntime.ContextCompactorPlugin {
	if s != nil && s.owner != nil && s.owner.plugins != nil {
		return s.owner.plugins.ContextCompactor()
	}
	return pluginruntime.NewContextCompactorPlugin()
}

type contextBudgetError = pluginruntime.ContextCompactionError

// ContextPlan describes the final provider-independent request selected by the
// host before the adapter serializes it. FinalRequestHash is filled from the
// adapter's exact serialized JSON body immediately before the network send.
type ContextPlan struct {
	Version              int      `json:"version"`
	Stage                string   `json:"stage"`
	PlanHash             string   `json:"planHash"`
	FinalRequestHash     string   `json:"finalRequestHash,omitempty"`
	EstimatedInputTokens int      `json:"estimatedInputTokens"`
	ContextWindow        int      `json:"contextWindow"`
	InputBudget          int      `json:"inputBudget"`
	MessageCount         int      `json:"messageCount"`
	ToolCount            int      `json:"toolCount"`
	ProviderID           string   `json:"providerId,omitempty"`
	GenerationID         string   `json:"generationId,omitempty"`
	SourceRefs           []string `json:"sourceRefs,omitempty"`
	RequestBytes         int      `json:"requestBytes,omitempty"`
	Rejected             bool     `json:"rejected,omitempty"`
	RejectionReason      string   `json:"rejectionReason,omitempty"`
}

// completeWithContextPlan is the single Agent model-call preflight. Callers
// pass the final transcript for their stage (after any history projection) and
// the exact tools they intend to expose. The provider adapter reports its exact
// serialized payload through a pre-send audit hook.
func completeWithContextPlan(ctx context.Context, models modelRuntime, scope loopScope, request loopRequest, stage string, messages []provider.ChatMessage, tools []provider.ToolDefinition, emit func(string, any) error) (provider.Completion, ContextPlan, error) {
	contextWindow := 131072
	if configured, ok := scope.(interface{ contextWindowTokens() int }); ok && configured.contextWindowTokens() > 0 {
		contextWindow = configured.contextWindowTokens()
	}
	plan, err := newContextPlan(stage, contextWindow, messages, tools)
	if err != nil {
		return provider.Completion{}, ContextPlan{}, err
	}
	plan.ProviderID = request.ProviderID
	plan.GenerationID = request.Generation.ID
	if plan.EstimatedInputTokens > plan.InputBudget {
		budgetErr := &contextBudgetError{Code: "context_budget_exhausted", Detail: fmt.Sprintf("%s request estimate (%d tokens) exceeds the %d-token preflight budget", stage, plan.EstimatedInputTokens, plan.InputBudget)}
		plan.Rejected, plan.RejectionReason = true, budgetErr.Error()
		if emit != nil {
			if emitErr := emit("context.plan.rejected", plan); emitErr != nil {
				return provider.Completion{}, plan, errors.Join(budgetErr, emitErr)
			}
		}
		return provider.Completion{}, plan, budgetErr
	}
	if emit != nil {
		if err := emit("context.plan.prepared", plan); err != nil {
			return provider.Completion{}, plan, err
		}
	}
	ctx = provider.WithRequestAudit(ctx, func(audit provider.RequestAudit) error {
		plan.FinalRequestHash = audit.PayloadSHA256
		plan.RequestBytes = audit.RequestBytes
		if emit == nil {
			return nil
		}
		return emit("context.plan.serialized", plan)
	})
	completion, err := models.CompleteWithTools(ctx, request.UserID, request.ProviderID, messages, tools)
	return completion, plan, err
}

func newContextPlan(stage string, contextWindow int, messages []provider.ChatMessage, tools []provider.ToolDefinition) (ContextPlan, error) {
	if contextWindow <= 0 {
		contextWindow = 131072
	}
	budget := contextWindow * compactionTriggerPercent / 100
	if budget < 1 {
		budget = 1
	}
	estimated := provider.EstimateInputTokens(messages, tools)
	planInput, err := json.Marshal(struct {
		Stage    string                    `json:"stage"`
		Messages []provider.ChatMessage    `json:"messages"`
		Tools    []provider.ToolDefinition `json:"tools"`
	}{stage, messages, tools})
	if err != nil {
		return ContextPlan{}, err
	}
	planDigest := sha256.Sum256(planInput)
	return ContextPlan{
		Version: 1, Stage: stage, PlanHash: hex.EncodeToString(planDigest[:]),
		EstimatedInputTokens: estimated, ContextWindow: contextWindow, InputBudget: budget,
		MessageCount: len(messages), ToolCount: len(tools), SourceRefs: uniqueSourceRefs(messages),
	}, nil
}

func emitRejectedProjection(request loopRequest, messages []provider.ChatMessage, tools []provider.ToolDefinition, contextWindow int, cause error) error {
	if request.Emit == nil {
		return nil
	}
	plan, err := newContextPlan("context_projection", contextWindow, messages, tools)
	if err != nil {
		return errors.Join(cause, err)
	}
	plan.Rejected = true
	if cause != nil {
		plan.RejectionReason = cause.Error()
	}
	if err := request.Emit("context.plan.rejected", plan); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

func uniqueSourceRefs(messages []provider.ChatMessage) []string {
	seen := make(map[string]struct{})
	refs := make([]string, 0)
	for _, message := range messages {
		if message.SourceID == "" {
			continue
		}
		if _, exists := seen[message.SourceID]; exists {
			continue
		}
		seen[message.SourceID] = struct{}{}
		refs = append(refs, message.SourceID)
	}
	return refs
}

func containsProviderItems(messages []provider.ChatMessage) bool {
	for _, message := range messages {
		if len(message.ProviderItems) > 0 {
			return true
		}
	}
	return false
}

func observedModelAttemptCount(completion provider.Completion, err error) int {
	if completion.Attempts > 0 {
		return completion.Attempts
	}
	var providerErr *provider.ProviderError
	if errors.As(err, &providerErr) && providerErr.Attempts > 0 {
		return providerErr.Attempts
	}
	return 0
}

func (s *turnScope) expandCompactionSourceRefs(ctx context.Context, sourceID string, state *storage.ContextCompactionState, seen map[string]struct{}, ordered *[]string) error {
	if sourceID == "" {
		return errors.New("compaction input is missing a durable source reference")
	}
	if _, duplicate := seen[sourceID]; duplicate {
		return nil
	}
	seen[sourceID] = struct{}{}
	switch {
	case strings.HasPrefix(sourceID, "semantic_summary:"):
		if state != nil && state.CanonicalSourceID == sourceID && state.Strategy == "semantic" {
			for _, ref := range state.CoveredSourceIDs {
				if err := s.expandCompactionSourceRefs(ctx, ref, nil, seen, ordered); err != nil {
					return err
				}
			}
			return nil
		}
		content, _, err := s.readContextSourceInConversation(ctx, s.conversationID, sourceID)
		if err != nil {
			return fmt.Errorf("read semantic source %q: %w", sourceID, err)
		}
		refsStart := strings.LastIndex(content, "sourceRefs: ")
		if refsStart < 0 {
			return fmt.Errorf("semantic source %q has no source index", sourceID)
		}
		encoded := strings.TrimSpace(strings.SplitN(content[refsStart+len("sourceRefs: "):], "\n", 2)[0])
		var refs []string
		if err := json.Unmarshal([]byte(encoded), &refs); err != nil || len(refs) == 0 {
			return fmt.Errorf("semantic source %q has an invalid source index: %w", sourceID, errors.Join(err, domain.ErrInvalid))
		}
		for _, ref := range refs {
			if err := s.expandCompactionSourceRefs(ctx, ref, state, seen, ordered); err != nil {
				return err
			}
		}
	case strings.HasPrefix(sourceID, "responses_native_state:"):
		if state == nil || state.CanonicalSourceID != sourceID || state.Strategy != "provider_native" {
			return fmt.Errorf("opaque Responses source %q has no compatible source index", sourceID)
		}
		for _, ref := range state.CoveredSourceIDs {
			if err := s.expandCompactionSourceRefs(ctx, ref, nil, seen, ordered); err != nil {
				return err
			}
		}
	default:
		if _, _, err := s.readContextSourceInConversation(ctx, s.conversationID, sourceID); err != nil {
			return fmt.Errorf("compaction source %q is not readable: %w", sourceID, err)
		}
		*ordered = append(*ordered, sourceID)
	}
	return nil
}

func (s *turnScope) nativeCompact(ctx context.Context, models modelRuntime, request loopRequest, messages []provider.ChatMessage, tools []provider.ToolDefinition, metrics domain.RunMetrics, budget, recentContextTokens, compactionCallBudget int) ([]provider.ChatMessage, string, []string, domain.RunMetrics, bool, error) {
	compactor, supports := models.(interface {
		CompactResponses(context.Context, string, string, []provider.ChatMessage) (provider.Completion, error)
	})
	if !supports || s.evaluation || s.owner == nil || s.owner.store == nil || s.owner.providers == nil {
		return nil, "", nil, metrics, false, nil
	}
	config, _, _, err := s.owner.store.ProviderSecret(ctx, s.userID, request.ProviderID)
	if err != nil {
		return nil, "", nil, metrics, true, fmt.Errorf("read provider binding for native compaction: %w", err)
	}
	if config.Kind != provider.KindOpenAIResponses {
		return nil, "", nil, metrics, false, nil
	}
	keepFrom, latestUser, compactable := s.contextCompactorPlugin().Boundary(messages, recentContextTokens)
	if len(compactable) == 0 {
		return nil, "", nil, metrics, false, nil
	}
	remaining := remainingModelAttempts(request, metrics)
	if remaining < 2 {
		return nil, "", nil, metrics, true, &contextBudgetError{Code: "budget_exhausted_before_compaction", Detail: "native compaction must leave one model attempt for the next Agent work call"}
	}
	callRequest := request
	globalAllowance := remaining - 1
	if globalAllowance > compactionCallBudget {
		globalAllowance = compactionCallBudget
	}
	if globalAllowance < 1 {
		return nil, "", nil, metrics, true, &contextBudgetError{Code: "compaction_call_budget_exhausted", Detail: "native compact has no model-call allowance after reserving Agent work"}
	}
	callRequest.ModelCallBudget = metrics.ModelCalls + globalAllowance
	state, stateErr := s.owner.store.ContextCompactionState(ctx, s.userID, s.conversationID)
	if stateErr != nil && !errors.Is(stateErr, domain.ErrNotFound) {
		return nil, "", nil, metrics, true, fmt.Errorf("read prior compaction source index: %w", stateErr)
	}
	var priorState *storage.ContextCompactionState
	if stateErr == nil {
		priorState = &state
	}
	seenRefs := map[string]struct{}{}
	allRefs := make([]string, 0, len(compactable))
	for _, message := range compactable {
		if err := s.expandCompactionSourceRefs(ctx, message.SourceID, priorState, seenRefs, &allRefs); err != nil {
			return nil, "", nil, metrics, true, &contextBudgetError{Code: "source_incomplete", Detail: err.Error()}
		}
	}
	if len(allRefs) == 0 {
		return nil, "", nil, metrics, true, &contextBudgetError{Code: "source_incomplete", Detail: "native compaction input has no readable original sources"}
	}
	plan, err := newContextPlan("responses_native_compaction", s.contextTokens, compactable, nil)
	if err != nil {
		return nil, "", nil, metrics, true, err
	}
	plan.ProviderID = request.ProviderID
	plan.GenerationID = request.Generation.ID
	if request.Emit != nil {
		if err := request.Emit("context.plan.prepared", plan); err != nil {
			return nil, "", nil, metrics, true, err
		}
	}
	callCtx := modelRequestContext(ctx, callRequest, metrics)
	callCtx = provider.WithRequestAudit(callCtx, func(audit provider.RequestAudit) error {
		plan.FinalRequestHash, plan.RequestBytes = audit.PayloadSHA256, audit.RequestBytes
		if request.Emit == nil {
			return nil
		}
		return request.Emit("context.plan.serialized", plan)
	})
	nativeModelCallsBefore := metrics.ModelCalls
	completion, compactErr := compactor.CompactResponses(callCtx, request.UserID, request.ProviderID, compactable)
	metrics.ModelCalls += observedModelAttemptCount(completion, compactErr)
	addUsage(&metrics, completion.Usage)
	if compactErr != nil {
		return nil, "", nil, metrics, true, fmt.Errorf("Responses native compaction failed: %w", compactErr)
	}
	if metrics.ModelCalls-nativeModelCallsBefore > compactionCallBudget {
		return nil, "", nil, metrics, true, &contextBudgetError{Code: "compaction_call_budget_exhausted", Detail: "native compact retries exceeded the configured compaction model-call budget"}
	}
	if len(completion.ProviderItems) == 0 {
		return nil, "", nil, metrics, true, &contextBudgetError{Code: "native_checkpoint_invalid", Detail: "Responses compact returned no opaque window items"}
	}
	for index, item := range completion.ProviderItems {
		if !json.Valid(item) {
			return nil, "", nil, metrics, true, fmt.Errorf("Responses compact output item %d is invalid JSON", index)
		}
	}
	payload, err := json.Marshal(completion.ProviderItems)
	if err != nil {
		return nil, "", nil, metrics, true, fmt.Errorf("encode opaque Responses window: %w", err)
	}
	sourceID := contextSourceID("responses_native_state", append([]byte(s.turnID+"\x00"), payload...))
	projected := make([]provider.ChatMessage, 0, len(messages)-len(compactable)+1)
	projected = append(projected, messages[0])
	nativeMessage := provider.ChatMessage{Role: "assistant", ProviderItems: s.contextCompactorPlugin().CopyProviderItems(completion.ProviderItems), SourceID: sourceID}
	inserted := false
	for index, message := range messages[1:] {
		absoluteIndex := index + 1
		if absoluteIndex == keepFrom && !inserted {
			projected = append(projected, nativeMessage)
			inserted = true
		}
		if absoluteIndex < keepFrom {
			if absoluteIndex == latestUser {
				projected = append(projected, message)
			}
			continue
		}
		projected = append(projected, message)
	}
	if !inserted {
		projected = append(projected, nativeMessage)
	}
	projectedTokens := provider.EstimateInputTokens(projected, tools)
	if projectedTokens > budget || projectedTokens >= provider.EstimateInputTokens(messages, tools) {
		return nil, "", nil, metrics, true, &contextBudgetError{Code: "context_budget_exhausted", Detail: "Responses native compaction did not produce a smaller window within the 75% host budget"}
	}
	if !s.evaluation {
		digest := sha256.Sum256(payload)
		ciphertext, nonce, err := s.owner.providers.SealRunCheckpoint(payload)
		if err != nil {
			return nil, "", nil, metrics, true, fmt.Errorf("encrypt opaque Responses checkpoint: %w", err)
		}
		if err := s.owner.store.SaveContextCompactionCheckpoint(ctx, storage.ContextCompactionState{
			ConversationID: s.conversationID, UserID: s.userID, StateVersion: 3, Strategy: "provider_native",
			ProviderID: request.ProviderID, Model: config.Model, ProtocolVersion: s.contextCompactorPlugin().ResponsesNativeProtocol(config),
			CanonicalSourceID: sourceID, CoveredSourceIDs: allRefs, TokensAtLastCompaction: projectedTokens,
		}, storage.ContextSourceCiphertext{SourceID: sourceID, SourceType: "responses_native_state", ContentHash: hex.EncodeToString(digest[:]), Ciphertext: ciphertext, Nonce: nonce}); err != nil {
			return nil, "", nil, metrics, true, fmt.Errorf("persist Responses native checkpoint: %w", err)
		}
	}
	readBack, _, err := s.readContextSourceInConversation(ctx, s.conversationID, sourceID)
	if err != nil || readBack != string(payload) {
		return nil, "", nil, metrics, true, fmt.Errorf("Responses native checkpoint failed source read-back verification: %w", errors.Join(err, domain.ErrConflict))
	}
	if !s.evaluation {
		persisted, err := s.owner.store.ContextCompactionState(ctx, s.userID, s.conversationID)
		if err != nil || persisted.CanonicalSourceID != sourceID || persisted.Strategy != "provider_native" || persisted.ProtocolVersion != s.contextCompactorPlugin().ResponsesNativeProtocol(config) || !s.contextCompactorPlugin().SameSourceRefs(persisted.CoveredSourceIDs, allRefs) {
			return nil, "", nil, metrics, true, fmt.Errorf("Responses native checkpoint failed state read-back verification: %w", errors.Join(err, domain.ErrConflict))
		}
	}
	return projected, sourceID, allRefs, metrics, true, nil
}

func (s *turnScope) persistContextSource(sourceID, sourceType string, content []byte) error {
	if s != nil && s.evaluation {
		if s.ephemeralSources == nil {
			s.ephemeralSources = map[string][]byte{}
		}
		s.ephemeralSources[sourceID] = append([]byte(nil), content...)
		return nil
	}
	if s == nil || s.owner == nil || s.owner.store == nil || s.owner.providers == nil {
		return errors.New("context source archive is unavailable")
	}
	digest := sha256.Sum256(content)
	ciphertext, nonce, err := s.owner.providers.SealRunCheckpoint(content)
	if err != nil {
		return fmt.Errorf("encrypt context source %q: %w", sourceID, err)
	}
	if err := s.owner.store.SaveContextSource(context.Background(), s.userID, s.conversationID, storage.ContextSourceCiphertext{
		SourceID: sourceID, SourceType: sourceType, ContentHash: hex.EncodeToString(digest[:]), Ciphertext: ciphertext, Nonce: nonce,
	}); err != nil {
		return err
	}
	persisted, persistedHash, err := s.readContextSourceInConversation(context.Background(), s.conversationID, sourceID)
	if err != nil {
		return fmt.Errorf("read archived context source %q back: %w", sourceID, err)
	}
	if persisted != string(content) || persistedHash != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("archived context source %q failed content read-back verification", sourceID)
	}
	return nil
}

func persistLoopContextSource(scope loopScope, sourceID, sourceType string, content []byte) error {
	archiver, ok := scope.(interface {
		persistContextSource(string, string, []byte) error
	})
	if !ok {
		if _, builtInScope := scope.(*turnScope); !builtInScope {
			return nil
		}
		return errors.New("durable context source archive is unavailable")
	}
	return archiver.persistContextSource(sourceID, sourceType, content)
}

func contextSourceID(prefix string, content []byte) string {
	digest := sha256.Sum256(content)
	return prefix + ":" + hex.EncodeToString(digest[:])
}

func remainingModelAttempts(request loopRequest, metrics domain.RunMetrics) int {
	if request.ModelCallBudget <= 0 {
		return int(^uint(0) >> 1)
	}
	return request.ModelCallBudget - metrics.ModelCalls
}

func (s *turnScope) readContextSource(ctx context.Context, sourceID string) (string, string, error) {
	if s == nil {
		return "", "", errors.New("context source archive is unavailable")
	}
	return s.readContextSourceInConversation(ctx, s.conversationID, sourceID)
}

func (s *turnScope) ReadCompactionSource(ctx context.Context, sourceID string) (string, error) {
	content, _, err := s.readContextSource(ctx, sourceID)
	return content, err
}

func (s *turnScope) CoveredCompactionSourceIDs(ctx context.Context, sourceID string) ([]string, error) {
	if s.evaluation {
		return append([]string(nil), s.ephemeralCovered[sourceID]...), nil
	}
	if s.owner == nil || s.owner.store == nil {
		return nil, errors.New("context compaction state storage is unavailable")
	}
	state, err := s.owner.store.ContextCompactionState(ctx, s.userID, s.conversationID)
	if err != nil {
		return nil, err
	}
	if state.CanonicalSourceID != sourceID {
		return nil, nil
	}
	return append([]string(nil), state.CoveredSourceIDs...), nil
}

func (s *turnScope) readContextSourceInConversation(ctx context.Context, conversationID, sourceID string) (string, string, error) {
	if s != nil && s.evaluation {
		content, ok := s.ephemeralSources[sourceID]
		if !ok {
			return "", "", domain.ErrNotFound
		}
		digest := sha256.Sum256(content)
		return string(content), hex.EncodeToString(digest[:]), nil
	}
	if s == nil || s.owner == nil || s.owner.store == nil {
		return "", "", errors.New("context source archive is unavailable")
	}
	content, archived, err := s.owner.store.ReadContextSource(ctx, s.userID, conversationID, sourceID)
	if err != nil {
		return "", "", err
	}
	if archived.SourceID != "" {
		contentBytes, err := s.owner.providers.OpenRunCheckpoint(archived.Ciphertext, archived.Nonce)
		if err != nil {
			return "", "", fmt.Errorf("decrypt context source %q: %w", sourceID, err)
		}
		var descriptor chunkedSnapshotDescriptor
		if json.Unmarshal(contentBytes, &descriptor) == nil && descriptor.Format == "encrypted_chunked_snapshot_v1" {
			var restored bytes.Buffer
			if err := s.streamChunkedSnapshot(ctx, descriptor, &restored); err != nil {
				return "", "", fmt.Errorf("read chunked context source %q: %w", sourceID, err)
			}
			content = restored.String()
		} else {
			content = string(contentBytes)
		}
	}
	digest := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(digest[:])
	if archived.SourceID != "" && archived.ContentHash != hash {
		return "", "", fmt.Errorf("context source %q hash verification failed", sourceID)
	}
	return content, hash, nil
}

func restoreContextTail(ctx context.Context, scope *turnScope, conversationID string, sourceIDs []string) ([]provider.ChatMessage, error) {
	messages := make([]provider.ChatMessage, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		content, _, err := scope.readContextSourceInConversation(ctx, conversationID, sourceID)
		if err != nil {
			return nil, fmt.Errorf("read retained context source %q: %w", sourceID, err)
		}
		switch {
		case strings.HasPrefix(sourceID, "tool_result:"):
			callID := strings.TrimPrefix(sourceID, "tool_result:")
			if callID == "" {
				return nil, fmt.Errorf("retained tool source %q has no call id", sourceID)
			}
			messages = append(messages, provider.ChatMessage{Role: "tool", ToolCallID: callID, Content: content, SourceID: sourceID, Historical: true})
		case strings.HasPrefix(sourceID, "assistant_tool_calls:"):
			var message provider.ChatMessage
			if err := json.Unmarshal([]byte(content), &message); err != nil || message.Role != "assistant" || len(message.ToolCalls) == 0 {
				return nil, fmt.Errorf("retained assistant source %q is invalid: %w", sourceID, errors.Join(err, domain.ErrInvalid))
			}
			message, include, err := prepareArchivedResponsesMessage(message, scope.providerID, scope.providerModel, true)
			if err != nil {
				return nil, fmt.Errorf("retained assistant source %q is invalid: %w", sourceID, err)
			}
			if !include {
				return nil, fmt.Errorf("retained assistant source %q has no compatible replay form: %w", sourceID, domain.ErrInvalid)
			}
			message.SourceID = sourceID
			message.Historical = true
			messages = append(messages, message)
		case strings.HasPrefix(sourceID, "assistant_responses_output:"):
			var message provider.ChatMessage
			if err := json.Unmarshal([]byte(content), &message); err != nil || message.Role != "assistant" || len(message.ProviderItems) == 0 {
				return nil, fmt.Errorf("retained Responses output source %q is invalid: %w", sourceID, errors.Join(err, domain.ErrInvalid))
			}
			message, include, err := prepareArchivedResponsesMessage(message, scope.providerID, scope.providerModel, false)
			if err != nil {
				return nil, fmt.Errorf("retained Responses output source %q is invalid: %w", sourceID, err)
			}
			if !include {
				continue
			}
			message.SourceID = sourceID
			message.Historical = true
			messages = append(messages, message)
		case strings.HasPrefix(sourceID, "planner_brief:"):
			messages = append(messages, provider.ChatMessage{Role: "assistant", Content: content, SourceID: sourceID, Historical: true})
		case strings.HasPrefix(sourceID, "local_skill:") || strings.HasPrefix(sourceID, "local_skill_catalog:"):
			messages = append(messages, provider.ChatMessage{Role: "assistant", Content: content, SourceID: sourceID, Historical: true})
		default:
			return nil, fmt.Errorf("retained context source %q has an unsupported source id", sourceID)
		}
	}
	return messages, nil
}

func prepareArchivedResponsesMessage(message provider.ChatMessage, providerID, model string, allowNormalizedFallback bool) (provider.ChatMessage, bool, error) {
	if message.Role != "assistant" {
		return provider.ChatMessage{}, false, domain.ErrInvalid
	}
	if len(message.ProviderItems) == 0 {
		if allowNormalizedFallback && len(message.ToolCalls) > 0 {
			return message, true, nil
		}
		return provider.ChatMessage{}, false, domain.ErrInvalid
	}
	if providerID != "" && model != "" && message.ProviderID == providerID && message.ProviderModel == model {
		message.Content = ""
		message.ToolCalls = nil
		return message, true, nil
	}
	message.ProviderItems = nil
	if allowNormalizedFallback && len(message.ToolCalls) > 0 {
		return message, true, nil
	}
	return provider.ChatMessage{}, false, nil
}

// restoreCoveredContextArchives rehydrates archived in-turn tool messages when
// semantic compaction is disabled. Conversation messages remain in the durable
// transcript, so archived tool messages are placed immediately before the next
// covered conversation message in source order.
func restoreCoveredContextArchives(ctx context.Context, scope *turnScope, detail domain.ConversationDetail, sourceIDs []string) (map[string][]provider.ChatMessage, map[string][]provider.ChatMessage, []provider.ChatMessage, error) {
	messageRoles := make(map[string]string, len(detail.Messages))
	currentSources := make(map[string]string, len(detail.Messages))
	messageIndices := make(map[string]int, len(detail.Messages))
	latestUserSource := ""
	latestUserIndex := -1
	for index, message := range detail.Messages {
		sourceID := messageSourceID(message.ID, message.Content)
		messageRoles[message.ID] = message.Role
		currentSources[message.ID] = sourceID
		messageIndices[message.ID] = index
		if message.Role == "user" {
			latestUserSource, latestUserIndex = sourceID, index
		}
	}
	before := map[string][]provider.ChatMessage{}
	after := map[string][]provider.ChatMessage{}
	var fallback []provider.ChatMessage
	pending := make([]string, 0)
	lastMessageSource := ""
	lastCoveredMessageIndex := -1
	for _, sourceID := range sourceIDs {
		if strings.HasPrefix(sourceID, "semantic_summary:") || strings.HasPrefix(sourceID, "responses_native_state:") {
			continue
		}
		if strings.HasPrefix(sourceID, "message:") {
			messageRef := strings.TrimPrefix(sourceID, "message:")
			messageID, _, hasVersion := strings.Cut(messageRef, "#")
			role, found := messageRoles[messageID]
			if messageID == "" || !found {
				return nil, nil, nil, fmt.Errorf("covered conversation source %q is not present in the durable transcript", sourceID)
			}
			content, _, readErr := scope.readContextSourceInConversation(ctx, detail.ID, sourceID)
			if readErr != nil {
				return nil, nil, nil, fmt.Errorf("read covered conversation source %q: %w", sourceID, readErr)
			}
			anchorSource := currentSources[messageID]
			if len(pending) > 0 {
				restored, err := restoreContextTail(ctx, scope, detail.ID, pending)
				if err != nil {
					return nil, nil, nil, err
				}
				before[anchorSource] = append(before[anchorSource], restored...)
				pending = pending[:0]
			}
			lastMessageSource = anchorSource
			lastCoveredMessageIndex = messageIndices[messageID]
			if hasVersion && sourceID != anchorSource {
				before[anchorSource] = append(before[anchorSource], provider.ChatMessage{Role: role, Content: content, SourceID: sourceID, Historical: true})
			}
			continue
		}
		pending = append(pending, sourceID)
	}
	if len(pending) > 0 {
		restored, err := restoreContextTail(ctx, scope, detail.ID, pending)
		if err != nil {
			return nil, nil, nil, err
		}
		anchorFromIndex := lastCoveredMessageIndex
		if latestUserIndex > anchorFromIndex {
			anchorFromIndex = latestUserIndex
		}
		assistantAnchor := ""
		for index := anchorFromIndex + 1; index < len(detail.Messages); index++ {
			if detail.Messages[index].Role == "assistant" {
				assistantAnchor = currentSources[detail.Messages[index].ID]
				break
			}
		}
		switch {
		case assistantAnchor != "":
			before[assistantAnchor] = append(before[assistantAnchor], restored...)
		case latestUserIndex > lastCoveredMessageIndex:
			after[latestUserSource] = append(after[latestUserSource], restored...)
		case lastMessageSource != "":
			after[lastMessageSource] = append(after[lastMessageSource], restored...)
		default:
			fallback = append(fallback, restored...)
		}
	}
	return before, after, fallback, nil
}

func retainedContextSourceIDs(messages []provider.ChatMessage, coveredSourceIDs []string) []string {
	covered := make(map[string]struct{}, len(coveredSourceIDs))
	for _, sourceID := range coveredSourceIDs {
		covered[sourceID] = struct{}{}
	}
	seen := map[string]struct{}{}
	retained := make([]string, 0)
	for _, message := range messages {
		sourceID := message.SourceID
		if sourceID == "" || strings.HasPrefix(sourceID, "message:") || strings.HasPrefix(sourceID, "semantic_summary:") {
			continue
		}
		if _, ok := covered[sourceID]; ok {
			continue
		}
		if !strings.HasPrefix(sourceID, "tool_result:") && !strings.HasPrefix(sourceID, "assistant_tool_calls:") && !strings.HasPrefix(sourceID, "assistant_responses_output:") && !strings.HasPrefix(sourceID, "planner_brief:") {
			continue
		}
		if _, exists := seen[sourceID]; exists {
			continue
		}
		seen[sourceID] = struct{}{}
		retained = append(retained, sourceID)
	}
	return retained
}

func (s *turnScope) prepareTurnMessages(ctx context.Context, models modelRuntime, request loopRequest, messages []provider.ChatMessage, tools []provider.ToolDefinition, step int, metrics domain.RunMetrics) (preparedMessages []provider.ChatMessage, compactionStats turnCompaction, updatedMetrics domain.RunMetrics, returnErr error) {
	settings := pluginruntime.CompactionSettings{Mode: "auto", TriggerPercent: compactionTriggerPercent, MinimumGrowthBeforeRecompactTokens: 8000, RecentContextTokens: 20000, CompactionCallBudget: 12, Version: 2}
	if s.owner != nil && s.owner.plugins != nil {
		settings = s.owner.plugins.ContextCompactionSettings()
	}
	triggerPercent := settings.TriggerPercent
	if triggerPercent < 10 || triggerPercent > compactionTriggerPercent {
		triggerPercent = compactionTriggerPercent
	}
	if settings.Mode != "auto" && settings.Mode != "semantic" && settings.Mode != "provider_native" && settings.Mode != "extractive" {
		settings.Mode = "auto"
	}
	strategyPlan := s.contextCompactorPlugin().Plan(settings.Mode)
	recentContextTokens := settings.RecentContextTokens
	if recentContextTokens < 1000 || recentContextTokens > 100000 {
		recentContextTokens = 20000
	}
	compactionCallBudget := settings.CompactionCallBudget
	if compactionCallBudget < 1 || compactionCallBudget > 32 {
		compactionCallBudget = 12
	}
	stats := turnCompaction{OriginalChars: messageChars(messages), Forced: s.forceCompact}
	cacheHits, cacheMisses, cacheWrites := 0, 0, 0
	triggered := false
	compactionStartedAt := time.Time{}
	defer func() {
		if returnErr == nil || !triggered || request.Emit == nil {
			return
		}
		code := "semantic_compaction_failed"
		var budgetErr *contextBudgetError
		if errors.As(returnErr, &budgetErr) && budgetErr.Code != "" {
			code = budgetErr.Code
		}
		durationMillis := int64(0)
		if !compactionStartedAt.IsZero() {
			durationMillis = time.Since(compactionStartedAt).Milliseconds()
		}
		if emitErr := request.Emit("context.compaction.failed", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "errorClass": code, "durationMillis": durationMillis, "triggerPercent": triggerPercent, "sourceRefs": uniqueSourceRefs(messages)}); emitErr != nil {
			returnErr = errors.Join(returnErr, emitErr)
		}
	}()
	force := s.forceCompact
	focus := s.compactionFocus
	s.forceCompact, s.compactionFocus = false, ""
	contextTokens := s.contextTokens
	if contextTokens <= 0 {
		contextTokens = 131072
	}
	budget := contextTokens * triggerPercent / 100
	if budget < 1 {
		budget = 1
	}
	contextRetentionCeiling := contextTokens * 20 / 100
	if contextRetentionCeiling < 1 {
		contextRetentionCeiling = 1
	}
	if recentContextTokens > contextRetentionCeiling {
		recentContextTokens = contextRetentionCeiling
	}
	inputTokens := provider.EstimateInputTokens(messages, tools)
	if inputTokens <= budget && !force {
		stats.CompactedChars = stats.OriginalChars
		return messages, stats, metrics, nil
	}
	triggered = true
	compactionStartedAt = time.Now()
	if request.Emit != nil {
		if err := request.Emit("context.compaction.requested", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "trigger": map[bool]string{true: "manual", false: "automatic"}[force], "triggerPercent": triggerPercent, "estimatedInputTokens": inputTokens, "inputBudget": budget, "recentContextTokens": recentContextTokens, "compactionCallBudget": compactionCallBudget, "sourceRefs": uniqueSourceRefs(messages)}); err != nil {
			return nil, stats, metrics, err
		}
	}
	enabled := s.owner != nil && s.owner.plugins != nil && s.owner.plugins.IsContextCompactorEnabled()
	if !enabled {
		budgetErr := &contextBudgetError{Code: "context_budget_exhausted", Detail: "core:context_compactor is disabled and the complete request exceeds the 75% host trigger"}
		return nil, stats, metrics, errors.Join(budgetErr, emitRejectedProjection(request, messages, tools, contextTokens, budgetErr))
	}
	if models == nil {
		budgetErr := &contextBudgetError{Code: "context_budget_exhausted", Detail: "model runtime is unavailable for context projection"}
		return nil, stats, metrics, errors.Join(budgetErr, emitRejectedProjection(request, messages, tools, contextTokens, budgetErr))
	}
	if !force && !s.evaluation && strategyPlan.AllowSemantic {
		state, stateErr := s.owner.store.ContextCompactionState(ctx, s.userID, s.conversationID)
		if stateErr != nil && !errors.Is(stateErr, domain.ErrNotFound) {
			budgetErr := &contextBudgetError{Code: "context_checkpoint_unavailable", Detail: stateErr.Error()}
			return nil, stats, metrics, errors.Join(budgetErr, emitRejectedProjection(request, messages, tools, contextTokens, budgetErr))
		}
		minimumGrowth := settings.MinimumGrowthBeforeRecompactTokens
		if minimumGrowth < 1000 {
			minimumGrowth = 8000
		}
		if windowGrowth := contextTokens * 5 / 100; windowGrowth > minimumGrowth {
			minimumGrowth = windowGrowth
		}
		if stateErr == nil && state.TokensAtLastCompaction > 0 && !containsProviderItems(messages) {
			growth := inputTokens - state.TokensAtLastCompaction
			if growth >= 0 && growth < minimumGrowth && inputTokens < contextTokens*90/100 {
				fallback := s.contextCompactorPlugin().Extractive(ctx, messages, budget*2, true)
				fallbackTokens := provider.EstimateInputTokens(fallback, tools)
				if fallbackTokens <= budget && fallbackTokens < inputTokens {
					stats.CompactedChars = messageChars(fallback)
					stats.Applied = true
					if request.Emit != nil {
						if err := request.Emit("context.compaction.degraded", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "strategy": "degraded_fallback", "reason": "minimum_growth_not_reached", "triggerPercent": triggerPercent, "durationMillis": time.Since(compactionStartedAt).Milliseconds()}); err != nil {
							return nil, stats, metrics, err
						}
						if err := request.Emit("context.compaction.completed", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "strategy": "degraded_fallback", "reason": "minimum_growth_not_reached", "triggerPercent": triggerPercent, "originalTokensEstimated": inputTokens, "projectedTokensEstimated": fallbackTokens, "durationMillis": time.Since(compactionStartedAt).Milliseconds(), "sourceRefs": uniqueSourceRefs(fallback)}); err != nil {
							return nil, stats, metrics, err
						}
					}
					return fallback, stats, metrics, nil
				}
			}
		}
	}
	compactionCallsBefore := metrics.ModelCalls
	compactionPromptTokensBefore := metrics.PromptTokens
	compactionCompletionTokensBefore := metrics.CompletionTokens
	if request.Emit != nil {
		if err := request.Emit("context.compaction.started", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "strategy": settings.Mode, "triggerPercent": triggerPercent, "recentContextTokens": recentContextTokens, "compactionCallBudget": compactionCallBudget, "sourceRefs": uniqueSourceRefs(messages)}); err != nil {
			return nil, stats, metrics, err
		}
	}
	if strategyPlan.UseExtractiveOnly {
		if containsProviderItems(messages) {
			budgetErr := &contextBudgetError{Code: "checkpoint_incompatible", Detail: "extractive compaction cannot rewrite provider-native context items"}
			return nil, stats, metrics, errors.Join(budgetErr, emitRejectedProjection(request, messages, tools, contextTokens, budgetErr))
		}
		prepared := s.contextCompactorPlugin().Extractive(ctx, messages, budget*2, true)
		projectedTokens := provider.EstimateInputTokens(prepared, tools)
		if projectedTokens > budget || projectedTokens >= inputTokens {
			budgetErr := &contextBudgetError{Code: "context_budget_exhausted", Detail: "configured extractive projection does not reduce the request below the trigger budget"}
			return nil, stats, metrics, errors.Join(budgetErr, emitRejectedProjection(request, messages, tools, contextTokens, budgetErr))
		}
		stats.CompactedChars, stats.Applied = messageChars(prepared), true
		if request.Emit != nil {
			if err := request.Emit("context.compaction.completed", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "strategy": "degraded_fallback", "reason": "configured_extractive", "triggerPercent": triggerPercent, "originalTokensEstimated": inputTokens, "projectedTokensEstimated": projectedTokens, "durationMillis": time.Since(compactionStartedAt).Milliseconds(), "sourceRefs": uniqueSourceRefs(prepared)}); err != nil {
				return nil, stats, metrics, err
			}
		}
		return prepared, stats, metrics, nil
	}
	hasOpaqueState := containsProviderItems(messages)
	var nativePrepared []provider.ChatMessage
	var nativeSourceID string
	var nativeRefs []string
	nativeMetrics := metrics
	nativeAttempted := false
	var nativeErr error
	if strategyPlan.TryProviderNative {
		nativePrepared, nativeSourceID, nativeRefs, nativeMetrics, nativeAttempted, nativeErr = s.nativeCompact(ctx, models, request, messages, tools, metrics, budget, recentContextTokens, compactionCallBudget)
	}
	metrics = nativeMetrics
	if nativeErr == nil && nativeAttempted && len(nativePrepared) > 0 {
		stats.CompactedChars = messageChars(nativePrepared)
		stats.Applied = true
		if request.Emit != nil {
			if err := request.Emit("context.compaction.completed", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "strategy": "provider_native", "triggerPercent": triggerPercent, "recentContextTokens": recentContextTokens, "compactionCallBudget": compactionCallBudget, "durationMillis": time.Since(compactionStartedAt).Milliseconds(), "originalTokensEstimated": inputTokens, "projectedTokensEstimated": provider.EstimateInputTokens(nativePrepared, tools), "sourceRefs": nativeRefs, "canonicalSourceRef": nativeSourceID, "compactionModelCalls": metrics.ModelCalls - compactionCallsBefore, "compactionPromptTokens": metrics.PromptTokens - compactionPromptTokensBefore, "compactionCompletionTokens": metrics.CompletionTokens - compactionCompletionTokensBefore}); err != nil {
				return nil, stats, metrics, err
			}
		}
		return nativePrepared, stats, metrics, nil
	}
	if hasOpaqueState {
		detail := "opaque provider-native context cannot be semantically summarized or deterministically truncated"
		if nativeErr != nil {
			detail = nativeErr.Error()
		}
		return nil, stats, metrics, errors.Join(&contextBudgetError{Code: "checkpoint_incompatible", Detail: detail}, emitRejectedProjection(request, messages, tools, contextTokens, nativeErr))
	}
	if !strategyPlan.AllowSemantic {
		if nativeErr == nil {
			nativeErr = &contextBudgetError{Code: "provider_native_unavailable", Detail: "selected provider does not support native Responses compaction"}
		}
		return nil, stats, metrics, errors.Join(nativeErr, emitRejectedProjection(request, messages, tools, contextTokens, nativeErr))
	}
	if nativeErr != nil && request.Emit != nil {
		var nativeBudgetErr *contextBudgetError
		code := "provider_native_compaction_failed"
		if errors.As(nativeErr, &nativeBudgetErr) {
			code = nativeBudgetErr.Code
		}
		if err := request.Emit("context.compaction.degraded", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "strategy": "semantic", "reason": code, "triggerPercent": triggerPercent, "durationMillis": time.Since(compactionStartedAt).Milliseconds(), "compactionModelCalls": metrics.ModelCalls - compactionCallsBefore, "compactionPromptTokens": metrics.PromptTokens - compactionPromptTokensBefore, "compactionCompletionTokens": metrics.CompletionTokens - compactionCompletionTokensBefore}); err != nil {
			return nil, stats, metrics, errors.Join(nativeErr, err)
		}
	}
	remainingCompactionCallBudget := compactionCallBudget - (metrics.ModelCalls - compactionCallsBefore)
	prepared, summary, refs, summaryMetrics, semanticCacheHits, semanticCacheMisses, semanticCacheWrites, err := s.semanticSummary(ctx, models, request, messages, tools, metrics, budget, focus, recentContextTokens, remainingCompactionCallBudget)
	metrics = summaryMetrics
	cacheHits, cacheMisses, cacheWrites = semanticCacheHits, semanticCacheMisses, semanticCacheWrites
	state := "semantic_completed"
	if err != nil {
		var compactionErr *contextBudgetError
		if !strategyPlan.AllowFallback {
			return nil, stats, metrics, errors.Join(err, emitRejectedProjection(request, messages, tools, contextTokens, err))
		}
		if errors.As(err, &compactionErr) && compactionErr.Code == "no_compactable_history" {
			if inputTokens <= budget {
				stats.CompactedChars = stats.OriginalChars
				if request.Emit != nil {
					if emitErr := request.Emit("context.compaction.noop", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "reason": compactionErr.Code, "triggerPercent": triggerPercent, "durationMillis": time.Since(compactionStartedAt).Milliseconds()}); emitErr != nil {
						return nil, stats, metrics, emitErr
					}
				}
				return messages, stats, metrics, nil
			}
		}
		prepared = s.contextCompactorPlugin().Extractive(ctx, messages, budget*2, true)
		fallbackTokens := provider.EstimateInputTokens(prepared, tools)
		if fallbackTokens > budget {
			code := "context_budget_exhausted"
			if errors.As(err, &compactionErr) && compactionErr.Code == "source_incomplete" {
				code = "source_incomplete"
			}
			budgetErr := &contextBudgetError{Code: code, Detail: "semantic compaction could not be completed safely and deterministic projection does not fit: " + err.Error()}
			return nil, stats, metrics, errors.Join(budgetErr, emitRejectedProjection(request, messages, tools, contextTokens, budgetErr))
		}
		if fallbackTokens >= inputTokens {
			budgetErr := &contextBudgetError{Code: "context_budget_exhausted", Detail: "semantic compaction failed and deterministic projection did not reduce the request"}
			return nil, stats, metrics, errors.Join(budgetErr, emitRejectedProjection(request, messages, tools, contextTokens, budgetErr))
		}
		state = "degraded_fallback"
		summary, refs = "", nil
		if request.Emit != nil {
			code := "semantic_compaction_failed"
			if errors.As(err, &compactionErr) && compactionErr.Code != "" {
				code = compactionErr.Code
			}
			if emitErr := request.Emit("context.compaction.degraded", map[string]any{"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "strategy": state, "reason": code, "triggerPercent": triggerPercent, "durationMillis": time.Since(compactionStartedAt).Milliseconds(), "projectedTokensEstimated": fallbackTokens}); emitErr != nil {
				return nil, stats, metrics, emitErr
			}
		}
	}
	if provider.EstimateInputTokens(prepared, tools) > budget {
		budgetErr := &contextBudgetError{Code: "context_budget_exhausted", Detail: "compacted projection still exceeds the host input budget"}
		return nil, stats, metrics, errors.Join(budgetErr, emitRejectedProjection(request, messages, tools, contextTokens, budgetErr))
	}
	stats.CompactedChars = messageChars(prepared)
	stats.Applied = stats.CompactedChars < stats.OriginalChars || force
	if stats.Applied && request.Emit != nil {
		if err := request.Emit("context.compaction.completed", map[string]any{
			"scope": "unified", "step": step + 1, "providerId": request.ProviderID, "generationId": request.Generation.ID, "strategy": state, "triggerPercent": triggerPercent, "durationMillis": time.Since(compactionStartedAt).Milliseconds(),
			"originalTokensEstimated": inputTokens, "projectedTokensEstimated": provider.EstimateInputTokens(prepared, tools), "recentContextTokens": recentContextTokens, "compactionCallBudget": compactionCallBudget,
			"sourceRefs": refs, "summaryBytes": len(summary),
			"compactionModelCalls":   metrics.ModelCalls - compactionCallsBefore,
			"compactionPromptTokens": metrics.PromptTokens - compactionPromptTokensBefore,
			"semanticCacheHits":      cacheHits, "semanticCacheMisses": cacheMisses, "semanticCacheWrites": cacheWrites,
		}); err != nil {
			return nil, stats, metrics, err
		}
	}
	return prepared, stats, metrics, nil
}

func (s *turnScope) semanticSummary(ctx context.Context, models modelRuntime, request loopRequest, messages []provider.ChatMessage, tools []provider.ToolDefinition, metrics domain.RunMetrics, budget int, focus string, recentContextTokens, compactionCallBudget int) ([]provider.ChatMessage, string, []string, domain.RunMetrics, int, int, int, error) {
	plugin := s.contextCompactorPlugin()
	cacheEnabled := !s.evaluation && s.owner != nil && s.owner.store != nil && s.owner.providers != nil
	if !s.evaluation && (s.owner == nil || s.owner.store == nil || s.owner.providers == nil) {
		return nil, "", nil, metrics, 0, 0, 0, errors.New("durable semantic compaction storage is unavailable")
	}
	var providerConfig domain.Provider
	var err error
	if cacheEnabled {
		providerConfig, _, _, err = s.owner.store.ProviderSecret(ctx, s.userID, request.ProviderID)
		if err != nil {
			return nil, "", nil, metrics, 0, 0, 0, fmt.Errorf("read provider binding for semantic summary: %w", err)
		}
	}
	runtime := pluginruntime.SemanticCompactionRuntime{
		Sources: s,
		Emit:    request.Emit,
		ReadCache: func(ctx context.Context, userID, conversationID, key string) (pluginruntime.SemanticCacheEntry, error) {
			entry, err := s.owner.store.ReadContextSummaryCache(ctx, userID, conversationID, key)
			return pluginruntime.SemanticCacheEntry{ConversationID: entry.ConversationID, UserID: entry.UserID, CacheKey: entry.CacheKey, ConfigHash: entry.ConfigHash, SourceIDs: entry.SourceIDs, ContentHash: entry.ContentHash, Ciphertext: entry.Ciphertext, Nonce: entry.Nonce}, err
		},
		DeleteCache: func(ctx context.Context, userID, conversationID, key, hash string) error {
			return s.owner.store.DeleteContextSummaryCacheEntry(ctx, userID, conversationID, key, hash)
		},
		SaveCache: func(ctx context.Context, entry pluginruntime.SemanticCacheEntry) error {
			return s.owner.store.SaveContextSummaryCache(ctx, storage.ContextSummaryCache{ConversationID: entry.ConversationID, UserID: entry.UserID, CacheKey: entry.CacheKey, ConfigHash: entry.ConfigHash, SourceIDs: entry.SourceIDs, ContentHash: entry.ContentHash, Ciphertext: entry.Ciphertext, Nonce: entry.Nonce})
		},
		OpenCheckpoint: func(ciphertext, nonce []byte) ([]byte, error) {
			return s.owner.providers.OpenRunCheckpoint(ciphertext, nonce)
		},
		SealCheckpoint: func(plain []byte) ([]byte, []byte, error) { return s.owner.providers.SealRunCheckpoint(plain) },
		SaveCheckpoint: func(ctx context.Context, state pluginruntime.SemanticCheckpoint, source pluginruntime.SemanticSourceArchive) error {
			storageState := storage.ContextCompactionState{ConversationID: state.ConversationID, UserID: state.UserID, StateVersion: state.StateVersion, Strategy: state.Strategy, ProviderID: state.ProviderID, Model: state.Model, ProtocolVersion: state.ProtocolVersion, CanonicalSourceID: state.CanonicalSourceID, CoveredSourceIDs: state.CoveredSourceIDs, TokensAtLastCompaction: state.TokensAtLastCompaction}
			storageSource := storage.ContextSourceCiphertext{SourceID: source.SourceID, SourceType: source.SourceType, ContentHash: source.ContentHash, Ciphertext: source.Ciphertext, Nonce: source.Nonce}
			return s.owner.store.SaveContextCompactionCheckpoint(ctx, storageState, storageSource)
		},
		PersistSource: s.persistContextSource,
		ReadSourceInConversation: func(ctx context.Context, conversationID, sourceID string) (string, error) {
			content, _, err := s.readContextSourceInConversation(ctx, conversationID, sourceID)
			return content, err
		},
		RecordEphemeralCovered: func(sourceID string, refs []string) {
			if s.ephemeralCovered == nil {
				s.ephemeralCovered = map[string][]string{}
			}
			s.ephemeralCovered[sourceID] = append([]string(nil), refs...)
		},
		NewSourceID: func(prefix string, content []byte) string { return contextSourceID(prefix, content) },
		InvokeModel: func(ctx context.Context, stage string, allowance int, current domain.RunMetrics, messages []provider.ChatMessage) (provider.Completion, int, domain.RunMetrics, error) {
			callRequest := request
			if callRequest.ModelCallBudget > 0 {
				callRequest.ModelCallBudget = current.ModelCalls + allowance
			}
			callCtx := modelRequestContext(ctx, callRequest, current)
			completion, plan, callErr := completeWithContextPlan(callCtx, models, request.Scope, callRequest, stage, messages, nil, request.Emit)
			current.ModelCalls += modelAttemptCount(completion, callErr)
			addUsage(&current, completion.Usage)
			return completion, plan.EstimatedInputTokens, current, callErr
		},
	}
	result, err := plugin.SemanticSummary(ctx, pluginruntime.SemanticCompactionInput{
		UserID: s.userID, ConversationID: s.conversationID, TurnID: s.turnID, ProviderID: request.ProviderID,
		ContextWindow: s.contextTokens, InputBudget: budget, RecentContextTokens: recentContextTokens,
		CompactionCallBudget: compactionCallBudget, ModelCallBudget: request.ModelCallBudget, Focus: focus,
		Evaluation: s.evaluation, CacheEnabled: cacheEnabled, ProviderConfig: providerConfig,
		Messages: messages, Tools: tools, Metrics: metrics,
	}, runtime)
	return result.Messages, result.Summary, result.SourceIDs, result.Metrics, result.CacheHits, result.CacheMisses, result.CacheWrites, err
}

// providerItemsCopy preserves opaque provider payloads across the generic
// Agent loop; compaction policy uses the plugin's equivalent helper.
func providerItemsCopy(items []json.RawMessage) []json.RawMessage {
	copyOfItems := make([]json.RawMessage, len(items))
	for index := range items {
		copyOfItems[index] = append(json.RawMessage(nil), items[index]...)
	}
	return copyOfItems
}
