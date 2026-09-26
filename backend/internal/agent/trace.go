package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"axiom.local/agent/internal/storage"
)

type traceRecorder struct {
	ctx    context.Context
	store  *storage.Store
	userID string
	turnID string
	notify func(string)
	seal   func([]byte) ([]byte, []byte, error)
}

func newTraceRecorder(ctx context.Context, store *storage.Store, userID, turnID string, notify func(string), seal func([]byte) ([]byte, []byte, error)) *traceRecorder {
	return &traceRecorder{ctx: ctx, store: store, userID: userID, turnID: turnID, notify: notify, seal: seal}
}

func (t *traceRecorder) emit(kind string, details any) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if err := t.store.AppendTurnEvent(t.ctx, t.userID, t.turnID, kind, raw, time.Now().UTC()); err != nil {
		return err
	}
	if t.notify != nil {
		t.notify(t.turnID)
	}
	return nil
}

func (t *traceRecorder) checkpoint(kind string, details any, checkpoint loopCheckpoint) error {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return err
	}
	checkpointJSON, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	if t.seal == nil {
		return fmt.Errorf("agent checkpoint encryption is unavailable")
	}
	ciphertext, nonce, err := t.seal(checkpointJSON)
	if err != nil {
		return fmt.Errorf("encrypt agent continuation checkpoint: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.ctx), 5*time.Second)
	defer cancel()
	if err := t.store.SaveAgentTurnCheckpoint(ctx, t.userID, t.turnID, kind, detailsJSON, ciphertext, nonce, checkpoint.Version, checkpoint.ResumeAllowed, time.Now().UTC()); err != nil {
		return err
	}
	if t.notify != nil {
		t.notify(t.turnID)
	}
	return nil
}
