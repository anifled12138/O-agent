package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestArtifactShareLinksPersistExpireAndRevoke(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("durably shareable output")
	digest := sha256.Sum256(data)
	digestText := hex.EncodeToString(digest[:])
	artifacts, err := artifactstore.New(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := artifacts.StoreFromReader(ctx, owner, "output.txt", "text/plain", "share-link-test", int64(len(data)), digestText, bytes.NewReader(data), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(time.Hour)
	if _, err := store.CreateArtifactShareLink(ctx, owner, "missing_artifact", "share_missing", digestText, expires, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("share link creation accepted an artifact that is not persisted: %v", err)
	}
	link, err := store.CreateArtifactShareLink(ctx, owner, artifact.ID, "share_test", digestText, expires, now)
	if err != nil || link.ID != "share_test" || link.ArtifactID != artifact.ID || link.RevokedAt != nil || !link.ExpiresAt.Equal(expires) {
		t.Fatalf("share link creation did not read back: link=%+v err=%v", link, err)
	}
	if _, err := store.ArtifactByShareToken(ctx, digestText, now.Add(time.Second)); err != nil {
		t.Fatalf("live artifact link did not resolve: %v", err)
	}
	if _, err := store.ArtifactByShareToken(ctx, digestText, expires); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired artifact link resolved: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	links, err := store.ArtifactShareLinks(ctx, owner, artifact.ID)
	if err != nil || len(links) != 1 || links[0].ID != link.ID || links[0].RevokedAt != nil {
		t.Fatalf("share-link list did not survive database restart: links=%+v err=%v", links, err)
	}
	revokedAt := expires.Add(time.Second)
	revoked, err := store.RevokeArtifactShareLink(ctx, owner, link.ID, revokedAt)
	if err != nil || revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(revokedAt) {
		t.Fatalf("revocation did not read back: link=%+v err=%v", revoked, err)
	}
	if _, err := store.ArtifactByShareToken(ctx, digestText, revokedAt); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revoked artifact link still resolved: %v", err)
	}
}
