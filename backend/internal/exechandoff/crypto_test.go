package exechandoff

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"axiom.local/agent/internal/domain"
)

func TestLeaseBoundHandoffAuthenticatedAndTaskScoped(t *testing.T) {
	payload := []byte(`{"sourceTurnId":"turn-local","checkpoint":{"messages":[{"role":"user","content":"private prompt"}]}}`)
	encrypted, err := Seal("task-one", "node-one", "lease-secret", payload)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := Open(encrypted, "task-one", "node-one", "lease-secret")
	if err != nil || !bytes.Equal(opened, payload) {
		t.Fatalf("valid lease could not open checkpoint: payload=%s err=%v", opened, err)
	}
	clear(opened)
	for _, identity := range []struct{ task, node, lease string }{
		{"task-two", "node-one", "lease-secret"},
		{"task-one", "node-two", "lease-secret"},
		{"task-one", "node-one", "different-lease"},
	} {
		if _, err := Open(encrypted, identity.task, identity.node, identity.lease); !errors.Is(err, domain.ErrUnauthorized) && !errors.Is(err, domain.ErrConflict) {
			t.Errorf("checkpoint opened for mismatched identity %+v: %v", identity, err)
		}
	}
	var envelope Envelope
	if err := json.Unmarshal(encrypted, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Ciphertext[0] ^= 0xff
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(tampered, "task-one", "node-one", "lease-secret"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("tampered checkpoint was not rejected: %v", err)
	}
}

func TestLeaseBoundHandoffRejectsInvalidAndOversizedPayloads(t *testing.T) {
	if _, err := Seal("task", "node", "lease", nil); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty checkpoint error = %v, want invalid", err)
	}
	if _, err := Seal("task", "node", "lease", make([]byte, MaxPayloadBytes+1)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("oversized checkpoint error = %v, want invalid", err)
	}
}
