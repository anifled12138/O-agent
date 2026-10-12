package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func TestDeviceApprovalAndCatalogRollback(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owner, err := s.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := DeviceAuthorization{DeviceHash: "private", UserHash: "public", ClientID: "client", Name: "PC", Platform: "windows", ExpiresAt: time.Now().Add(time.Hour)}
	if err = s.CreateDeviceAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Failure halfway through enrollment must leave neither an approved request nor a node.
	if _, err = s.db.Exec(`CREATE TRIGGER fail_device_binding BEFORE INSERT ON account_devices BEGIN SELECT RAISE(ABORT,'injected binding failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveDeviceAuthorization(ctx, a.UserHash, owner, "node-test", "hash", []byte{1}, []byte{2}, time.Now()); err == nil {
		t.Fatal("enrollment swallowed binding error")
	}
	nodes, err := s.ExecutionNodes(ctx, owner)
	if err != nil || len(nodes) != 0 {
		t.Fatal("partial node survived rollback", err)
	}
	got, err := s.DeviceAuthorization(ctx, a.DeviceHash, false)
	if err != nil || got.UserID != "" {
		t.Fatal("request approved before enrollment")
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_device_binding`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApproveDeviceAuthorization(ctx, a.UserHash, owner, "node-test", "hash", []byte{1}, []byte{2}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_catalog BEFORE INSERT ON execution_node_catalogs BEGIN SELECT RAISE(ABORT,'injected catalog failure'); END;`); err != nil {
		t.Fatal(err)
	}
	catalog := NodeCatalog{Models: []NodeModel{{ID: "model", Name: "Model", Model: "test"}}, PermissionProfiles: []domain.PermissionProfile{domain.PermissionProfileReadOnly}}
	if _, err = s.HeartbeatExecutionNodeWithCatalog(ctx, "hash", "windows", []string{"agent-runtime"}, ExecutionNodeResources{MaxConcurrentTasks: 1}, &catalog, time.Now()); err == nil {
		t.Fatal("heartbeat ignored catalog failure")
	}
	node, err := s.ExecutionNode(ctx, owner, "node-test")
	if err != nil || node.LastSeen != nil {
		t.Fatal("partial heartbeat survived rollback", err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_catalog`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.HeartbeatExecutionNodeWithCatalog(ctx, "hash", "windows", []string{"agent-runtime"}, ExecutionNodeResources{MaxConcurrentTasks: 1}, &catalog, time.Now()); err != nil {
		t.Fatal(err)
	}
	// An older credential cannot revoke the current epoch of the same node ID.
	if err = s.RevokeExecutionNodeForToken(ctx, owner, "node-test", "old-hash", time.Now()); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err = s.ExecutionNodeForToken(ctx, "hash"); err != nil {
		t.Fatal("delayed logout revoked a newer login", err)
	}
}

func TestDeviceModelReferenceWithoutCloudCompute(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisterExecutionNode(ctx, ExecutionNode{ID: "pc-reference", UserID: owner, Name: "PC", Platform: "windows"}, "hash-reference"); err != nil {
		t.Fatal(err)
	}
	catalog := NodeCatalog{Models: []NodeModel{{ID: "provider-on-pc", Name: "My model", Model: "actual-local-model"}}, PermissionProfiles: []domain.PermissionProfile{domain.PermissionProfileReadOnly}}
	if _, err = s.HeartbeatExecutionNodeWithCatalog(ctx, "hash-reference", "windows", []string{"agent-runtime"}, ExecutionNodeResources{MaxConcurrentTasks: 1}, &catalog, time.Now()); err != nil {
		t.Fatal(err)
	}
	ref, err := s.EnsureDeviceModelReference(ctx, owner, "pc-reference", "provider-on-pc")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = s.CreateConversation(ctx, domain.Conversation{ID: "remote-only", UserID: owner, Title: "Remote conversation", ProviderID: ref.ID, PermissionProfile: domain.PermissionProfileReadOnly, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	detail, err := s.Conversation(ctx, owner, "remote-only")
	if err != nil || detail.ProviderID != ref.ID {
		t.Fatal("device-only conversation did not survive restart", err)
	}
	saved, cipher, nonce, err := s.ProviderSecret(ctx, owner, ref.ID)
	if err != nil || saved.Kind != DeviceModelReferenceKind || saved.BaseURL != "" || saved.HasAPIKey || len(cipher) != 0 || len(nonce) != 0 {
		t.Fatal("reference invented cloud compute", err)
	}
	second, err := s.EnsureDeviceModelReference(ctx, owner, "pc-reference", "provider-on-pc")
	if err != nil || second.ID != ref.ID {
		t.Fatal("duplicate model routing reference", err)
	}
}

func TestDeviceCredentialRevocationReceiptRollbackAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisterExecutionNode(ctx, ExecutionNode{ID: "pc-receipt", UserID: owner, Name: "PC", Platform: "windows"}, "receipt-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("CREATE TRIGGER fail_receipt BEFORE INSERT ON device_credential_revocations BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END;"); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeExecutionNodeForToken(ctx, owner, "pc-receipt", "receipt-hash", time.Now()); err == nil {
		t.Fatal("receipt failure was ignored")
	}
	if _, err = s.ExecutionNodeForToken(ctx, "receipt-hash"); err != nil {
		t.Fatal("failed receipt did not roll back revocation", err)
	}
	if revoked, err := s.DeviceCredentialRevoked(ctx, owner, "receipt-hash"); err != nil || revoked {
		t.Fatal("false revocation receipt", err)
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_receipt"); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeExecutionNodeForToken(ctx, owner, "pc-receipt", "receipt-hash", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if revoked, err := s.DeviceCredentialRevoked(ctx, owner, "receipt-hash"); err != nil || !revoked {
		t.Fatal("revocation proof did not survive restart", err)
	}
	if revoked, err := s.DeviceCredentialRevoked(ctx, owner, "other-hash"); err != nil || revoked {
		t.Fatal("receipt matched another credential", err)
	}
}
