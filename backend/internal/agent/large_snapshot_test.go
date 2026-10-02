package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func TestLargeSnapshotIsEncryptedChunkedAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC()
	user := domain.User{ID: "large_snapshot_user", Email: "large-snapshot@example.test", DisplayName: "Snapshot", CreatedAt: now}
	if err := store.CreateUser(ctx, user, "test"); err != nil {
		t.Fatal(err)
	}
	model := domain.Provider{ID: "large_snapshot_provider", UserID: user.ID, Name: "Local", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "test", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, model, []byte{}, []byte{}); err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "large_snapshot_conversation", UserID: user.ID, ProviderID: model.ID, Title: "snapshot", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	vault, err := secure.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	providers := provider.New(store, vault)
	artifacts, err := artifactstore.New(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	owner := &Service{store: store, providers: providers, artifacts: artifacts}
	scope := &turnScope{owner: owner, userID: user.ID, conversationID: conversation.ID}
	content := []byte(strings.Repeat("界a\n", (32<<20)/5+2))
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	probe, probeNonce, err := providers.SealRunCheckpoint(nil)
	if err != nil {
		t.Fatal(err)
	}
	chunkCount := (int64(len(content)) + largeSnapshotChunkBytes - 1) / largeSnapshotChunkBytes
	partialUpload, _, err := artifacts.Begin(ctx, user.ID, artifactstore.BeginRequest{FileName: "snapshot.enc", MediaType: "application/octet-stream", ExpectedSize: int64(len(content)) + chunkCount*int64(len(probe)+len(probeNonce)), IdempotencyKey: "context-source-" + hash}, now)
	if err != nil {
		t.Fatal(err)
	}
	partial := &limitedFailureReader{reader: bytes.NewReader(content), remaining: 12 << 20}
	if _, err := scope.archiveLargeToolSource(ctx, "file_snapshot", partial, int64(len(content)), hash); err == nil {
		t.Fatal("interrupted source stream unexpectedly published a snapshot")
	}
	state, chunks, count, err := artifacts.Status(ctx, user.ID, partialUpload.ID, 0, 10)
	if err != nil || state.Status != "uploading" || count < 1 || len(chunks) < 1 {
		t.Fatalf("interrupted snapshot did not leave a durable resumable chunk: state=%+v chunks=%+v count=%d err=%v", state, chunks, count, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err = artifactstore.New(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	scope = &turnScope{owner: &Service{store: store, providers: provider.New(store, vault), artifacts: artifacts}, userID: user.ID, conversationID: conversation.ID}
	sourceID, err := scope.archiveLargeToolSource(ctx, "file_snapshot", bytes.NewReader(content), int64(len(content)), hash)
	if err != nil {
		t.Fatal(err)
	}
	if sourceID != "file_snapshot:"+hash {
		t.Fatalf("unexpected stable source id %q", sourceID)
	}
	retriedID, err := scope.archiveLargeToolSource(ctx, "file_snapshot", bytes.NewReader(content), int64(len(content)), hash)
	if err != nil || retriedID != sourceID {
		t.Fatalf("retrying the same chunked snapshot did not resume idempotently: source=%q err=%v", retriedID, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	artifacts, err = artifactstore.New(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	scope = &turnScope{owner: &Service{store: store, providers: provider.New(store, vault), artifacts: artifacts}, userID: user.ID, conversationID: conversation.ID}
	got, gotHash, err := scope.readContextSourceInConversation(ctx, conversation.ID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if gotHash != hash || !bytes.Equal([]byte(got), content) {
		t.Fatalf("large archived source failed restart/read-back verification (len=%d, hash=%s)", len(got), gotHash)
	}
	offset := largeSnapshotChunkBytes/5 - 2
	page, pageHash, total, err := scope.readContextSourcePage(ctx, conversation.ID, sourceID, offset, 512)
	if err != nil {
		t.Fatal(err)
	}
	runes := []rune(string(content))
	if pageHash != hash || total != len(runes) || page != string(runes[offset:offset+512]) {
		t.Fatalf("chunked page read failed rune-boundary verification (hash=%s total=%d page=%q)", pageHash, total, page[:min(len(page), 30)])
	}
	arguments, err := json.Marshal(map[string]any{"sourceId": sourceID, "offset": offset, "limit": 512})
	if err != nil {
		t.Fatal(err)
	}
	var toolResponse struct {
		OK     bool `json:"ok"`
		Result struct {
			Content string `json:"content"`
			Total   int    `json:"totalCharacters"`
			Next    int    `json:"nextOffset"`
		} `json:"result"`
	}
	if err := json.Unmarshal(scope.execute(ctx, "axiom_context_source_read", arguments), &toolResponse); err != nil {
		t.Fatal(err)
	}
	if !toolResponse.OK || toolResponse.Result.Content != page || toolResponse.Result.Total != total || toolResponse.Result.Next != offset+512 {
		t.Fatalf("Agent context source tool did not preserve bounded page contract: %#v", toolResponse)
	}
}

type limitedFailureReader struct {
	reader    *bytes.Reader
	remaining int64
}

func (r *limitedFailureReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func TestLargeSnapshotHashFailureDoesNotPublishContextSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	user := domain.User{ID: "large_snapshot_failure_user", Email: "large-snapshot-failure@example.test", DisplayName: "Snapshot", CreatedAt: now}
	if err := store.CreateUser(ctx, user, "test"); err != nil {
		t.Fatal(err)
	}
	model := domain.Provider{ID: "large_snapshot_failure_provider", UserID: user.ID, Name: "Local", Kind: "openai-compatible", BaseURL: "http://localhost/v1", Model: "test", CreatedAt: now, UpdatedAt: now}
	if err := store.UpsertProvider(ctx, model, []byte{}, []byte{}); err != nil {
		t.Fatal(err)
	}
	conversation := domain.Conversation{ID: "large_snapshot_failure_conversation", UserID: user.ID, ProviderID: model.ID, Title: "snapshot", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	vault, err := secure.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	providers := provider.New(store, vault)
	artifacts, err := artifactstore.New(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	scope := &turnScope{owner: &Service{store: store, providers: providers, artifacts: artifacts}, userID: user.ID, conversationID: conversation.ID}
	content := bytes.Repeat([]byte("x"), largeSnapshotChunkBytes+1)
	wrongHash := strings.Repeat("0", sha256.Size*2)
	if _, err := scope.archiveLargeToolSource(ctx, "file_snapshot", bytes.NewReader(content), int64(len(content)), wrongHash); err == nil {
		t.Fatal("snapshot with a mismatched plaintext digest was archived")
	}
	if _, _, err := store.ReadContextSource(ctx, user.ID, conversation.ID, "file_snapshot:"+wrongHash); err == nil {
		t.Fatal("failed snapshot published a context source")
	}
}
