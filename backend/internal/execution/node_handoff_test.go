package execution

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/domain"
)

func TestNodeHandoffCheckpointIsEncryptedAndLeaseBound(t *testing.T) {
	checkpointBody := json.RawMessage(`{"version":1,"messages":[{"role":"user","content":"sensitive prompt"}]}`)
	digest := sha256.Sum256(checkpointBody)
	checkpoint := agent.HandoffCheckpoint{SourceTurnID: "turn-local", SourceConversationID: "conversation-local", SourceInputMessageID: "message-input", ProviderID: "provider-one", GenerationID: "generation-one", DefinitionDigest: "definition-digest", PermissionProfile: domain.DefaultPermissionProfile(), ContentSHA256: hex.EncodeToString(digest[:]), Checkpoint: checkpointBody}
	sealed, err := sealNodeHandoffCheckpoint("task-one", "node-one", "lease-secret", checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "sensitive prompt") || bytes.Contains(sealed, checkpointBody) {
		t.Fatal("encrypted handoff capsule contains checkpoint plaintext")
	}
	opened, err := openNodeHandoffCheckpoint(sealed, "task-one", "node-one", "lease-secret")
	if err != nil || opened.SourceTurnID != checkpoint.SourceTurnID || !bytes.Equal(opened.Checkpoint, checkpointBody) {
		t.Fatalf("lease could not recover handoff checkpoint: checkpoint=%+v err=%v", opened, err)
	}
	if _, err := openNodeHandoffCheckpoint(sealed, "task-one", "node-one", "another-lease"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("checkpoint opened under another lease: %v", err)
	}
	if _, err := sealNodeHandoffCheckpoint("task-one", "node-one", "lease-secret", agent.HandoffCheckpoint{Checkpoint: checkpointBody, ContentSHA256: strings.Repeat("0", 64)}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("invalid local checkpoint digest was accepted: %v", err)
	}
}
