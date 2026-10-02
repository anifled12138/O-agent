package exechandoff

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"axiom.local/agent/internal/domain"
)

const MaxPayloadBytes = 16 << 20

type Envelope struct {
	Version        int    `json:"version"`
	TaskID         string `json:"taskId"`
	NodeID         string `json:"nodeId"`
	ContentSHA256  string `json:"contentSha256"`
	Nonce          []byte `json:"nonce"`
	Ciphertext     []byte `json:"ciphertext"`
	CiphertextHash string `json:"ciphertextSha256"`
}

// Seal binds sensitive checkpoint bytes to one live task lease and node.
func Seal(taskID, nodeID, lease string, payload []byte) (json.RawMessage, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(nodeID) == "" || strings.TrimSpace(lease) == "" || len(payload) == 0 || len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("handoff payload is incomplete: %w", domain.ErrInvalid)
	}
	digest := sha256.Sum256(payload)
	key := deriveKey(taskID, nodeID, lease)
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize task handoff encryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize task handoff authentication: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("create task handoff nonce: %w", err)
	}
	ciphertext := aead.Seal(nil, nonce, payload, aad(taskID, nodeID))
	cipherDigest := sha256.Sum256(ciphertext)
	encoded, err := json.Marshal(Envelope{Version: 1, TaskID: taskID, NodeID: nodeID, ContentSHA256: hex.EncodeToString(digest[:]), Nonce: nonce, Ciphertext: ciphertext, CiphertextHash: hex.EncodeToString(cipherDigest[:])})
	if err != nil {
		return nil, fmt.Errorf("encode encrypted task handoff: %w", err)
	}
	return encoded, nil
}

// Open accepts only a capsule whose task, node, digest, and lease all match.
func Open(raw json.RawMessage, taskID, nodeID, lease string) ([]byte, error) {
	if len(raw) == 0 || len(raw) > MaxPayloadBytes+(1<<20) || strings.TrimSpace(taskID) == "" || strings.TrimSpace(nodeID) == "" || strings.TrimSpace(lease) == "" {
		return nil, domain.ErrInvalid
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Version != 1 || envelope.TaskID != taskID || envelope.NodeID != nodeID || len(envelope.ContentSHA256) != 64 || len(envelope.Nonce) == 0 || len(envelope.Ciphertext) == 0 {
		return nil, fmt.Errorf("encrypted task handoff identity or metadata is invalid: %w", domain.ErrConflict)
	}
	cipherDigest := sha256.Sum256(envelope.Ciphertext)
	if hex.EncodeToString(cipherDigest[:]) != strings.ToLower(envelope.CiphertextHash) {
		return nil, fmt.Errorf("encrypted task handoff digest mismatch: %w", domain.ErrConflict)
	}
	key := deriveKey(taskID, nodeID, lease)
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialize task handoff decryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize task handoff authentication: %w", err)
	}
	if len(envelope.Nonce) != aead.NonceSize() {
		return nil, domain.ErrConflict
	}
	payload, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, aad(taskID, nodeID))
	if err != nil {
		return nil, fmt.Errorf("authenticate task handoff with current lease: %w", domain.ErrUnauthorized)
	}
	if len(payload) == 0 || len(payload) > MaxPayloadBytes {
		clear(payload)
		return nil, domain.ErrConflict
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != strings.ToLower(envelope.ContentSHA256) {
		clear(payload)
		return nil, fmt.Errorf("decrypted task handoff digest mismatch: %w", domain.ErrConflict)
	}
	return payload, nil
}

func deriveKey(taskID, nodeID, lease string) []byte {
	digest := sha256.Sum256([]byte("o-agent-node-handoff-checkpoint-v1\x00" + taskID + "\x00" + nodeID + "\x00" + lease))
	return append([]byte(nil), digest[:]...)
}

func aad(taskID, nodeID string) []byte {
	return []byte("o-agent-node-handoff-checkpoint-v1\x00" + taskID + "\x00" + nodeID)
}
