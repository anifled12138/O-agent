package storage

import (
	"axiom.local/agent/internal/domain"
	"context"
	"errors"
	"testing"
	"time"
)

func TestEmailChallengeAttemptsExpiryAndTransactionRollback(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner, err := s.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	makeChallenge := func(id, email string) AuthEmailChallenge {
		return AuthEmailChallenge{ID: id, Purpose: "register", UserID: owner, Email: email, DisplayName: "Owner", PasswordHash: "pbkdf2-sha256$test", CodeHash: "code-hash", CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	}
	c := makeChallenge("challenge-attempts", "one@example.com")
	if err = s.PrepareAuthEmailChallenge(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeAuthEmailChallenge(ctx, c.ID, "register", c.CodeHash, "", now); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("delivery-pending challenge accepted")
	}
	if err = s.SetAuthEmailDelivery(ctx, c.ID, "receipt", true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err = s.ConsumeAuthEmailChallenge(ctx, c.ID, "register", "wrong", "", now); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	exhausted, err := s.AuthEmailChallenge(ctx, c.ID)
	if err != nil || exhausted.Attempts != 5 || exhausted.State != "attempts_exhausted" {
		t.Fatal("attempt lockout not durable")
	}
	if _, err = s.ConsumeAuthEmailChallenge(ctx, c.ID, "register", c.CodeHash, "", now); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("exhausted proof accepted")
	}
	c = makeChallenge("challenge-expiry", "two@example.com")
	if err = s.PrepareAuthEmailChallenge(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthEmailDelivery(ctx, c.ID, "receipt", true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeAuthEmailChallenge(ctx, c.ID, "register", c.CodeHash, "", now.Add(11*time.Minute)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("expired proof accepted")
	}
	c = makeChallenge("challenge-rollback", "three@example.com")
	if err = s.PrepareAuthEmailChallenge(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAuthEmailDelivery(ctx, c.ID, "receipt", true); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateAuthSession(ctx, "old-session", owner, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `CREATE TRIGGER fail_auth_revoke BEFORE DELETE ON auth_sessions BEGIN SELECT RAISE(ABORT,'injected revoke failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeAuthEmailChallenge(ctx, c.ID, "register", c.CodeHash, "", now); err == nil {
		t.Fatal("failed multi-step mutation returned success")
	}
	_, hash, err := s.AuthUserByID(ctx, owner)
	if err != nil || hash != "" {
		t.Fatal("credential update survived failed transaction")
	}
	got, err := s.AuthEmailChallenge(ctx, c.ID)
	if err != nil || got.State != "awaiting_code" {
		t.Fatal("proof consumed by rolled-back transaction")
	}
	if _, err = s.AuthSessionUser(ctx, "old-session", now); err != nil {
		t.Fatal("session lost in failed transaction")
	}
}
