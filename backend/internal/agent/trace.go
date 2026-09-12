package agent

import (
	"context"
	"encoding/json"
	"time"

	"axiom.local/agent/internal/storage"
)

type traceRecorder struct {
	ctx    context.Context
	store  *storage.Store
	userID string
	turnID string
	notify func(string)
}

func newTraceRecorder(ctx context.Context, store *storage.Store, userID, turnID string, notify func(string)) *traceRecorder {
	return &traceRecorder{ctx: ctx, store: store, userID: userID, turnID: turnID, notify: notify}
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
