package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/exechandoff"
)

const nodeHandoffCheckpointMaxBytes = 16 << 20

// sealNodeHandoffCheckpoint encrypts sensitive Agent prompts and tool context
// under the active one-task lease. The bearer lease itself is never persisted.
func sealNodeHandoffCheckpoint(taskID, nodeID, lease string, checkpoint agent.HandoffCheckpoint) (json.RawMessage, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(nodeID) == "" || strings.TrimSpace(lease) == "" || len(checkpoint.Checkpoint) == 0 || len(checkpoint.Checkpoint) > nodeHandoffCheckpointMaxBytes || !json.Valid(checkpoint.Checkpoint) || len(checkpoint.ContentSHA256) != 64 {
		return nil, fmt.Errorf("handoff checkpoint envelope is incomplete: %w", domain.ErrInvalid)
	}
	digest := sha256.Sum256(checkpoint.Checkpoint)
	if hex.EncodeToString(digest[:]) != strings.ToLower(checkpoint.ContentSHA256) {
		return nil, fmt.Errorf("handoff checkpoint content digest mismatch: %w", domain.ErrConflict)
	}
	plain, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, fmt.Errorf("encode verified Agent handoff checkpoint: %w", err)
	}
	defer clear(plain)
	return exechandoff.Seal(taskID, nodeID, lease, plain)
}

// openNodeHandoffCheckpoint is used by the cloud lease-bound handoff endpoint
// before it re-encrypts the verified checkpoint with the cloud vault key.
func openNodeHandoffCheckpoint(raw json.RawMessage, taskID, nodeID, lease string) (agent.HandoffCheckpoint, error) {
	if len(raw) == 0 || len(raw) > nodeHandoffCheckpointMaxBytes+(1<<20) || strings.TrimSpace(taskID) == "" || strings.TrimSpace(nodeID) == "" || strings.TrimSpace(lease) == "" {
		return agent.HandoffCheckpoint{}, domain.ErrInvalid
	}
	plain, err := exechandoff.Open(raw, taskID, nodeID, lease)
	if err != nil {
		return agent.HandoffCheckpoint{}, err
	}
	defer clear(plain)
	var checkpoint agent.HandoffCheckpoint
	if err := json.Unmarshal(plain, &checkpoint); err != nil || len(checkpoint.Checkpoint) == 0 || len(checkpoint.Checkpoint) > nodeHandoffCheckpointMaxBytes || !json.Valid(checkpoint.Checkpoint) || len(checkpoint.ContentSHA256) != 64 {
		return agent.HandoffCheckpoint{}, fmt.Errorf("decrypted task handoff checkpoint is invalid: %w", domain.ErrConflict)
	}
	digest := sha256.Sum256(checkpoint.Checkpoint)
	if hex.EncodeToString(digest[:]) != strings.ToLower(checkpoint.ContentSHA256) {
		return agent.HandoffCheckpoint{}, fmt.Errorf("decrypted task handoff content digest mismatch: %w", domain.ErrConflict)
	}
	return checkpoint, nil
}
