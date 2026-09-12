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
}

func newTraceRecorder(ctx context.Context, store *storage.Store, userID, turnID string) *traceRecorder {
	return &traceRecorder{ctx: ctx, store: store, userID: userID, turnID: turnID}
}

func (t *traceRecorder) emit(kind string, details any) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return t.store.AppendTurnEvent(t.ctx, t.userID, t.turnID, kind, raw, time.Now().UTC())
}
