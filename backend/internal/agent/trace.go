package agent

import (
	"context"
	"encoding/json"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

type traceRecorder struct {
	ctx            context.Context
	store          *storage.Store
	userID         string
	conversationID string
	turnID         string
	sequence       int
}

func newTraceRecorder(ctx context.Context, store *storage.Store, userID, conversationID string) *traceRecorder {
	return &traceRecorder{ctx: ctx, store: store, userID: userID, conversationID: conversationID, turnID: id("turn")}
}

func (t *traceRecorder) emit(kind string, details any) {
	t.sequence++
	raw, err := json.Marshal(details)
	if err != nil {
		raw = json.RawMessage(`{"error":"trace details could not be encoded"}`)
	}
	_ = t.store.AddTraceEvent(t.ctx, t.userID, domain.TraceEvent{ID: id("trc"), ConversationID: t.conversationID, TurnID: t.turnID, Sequence: t.sequence, Kind: kind, Details: raw, CreatedAt: time.Now().UTC()})
}
