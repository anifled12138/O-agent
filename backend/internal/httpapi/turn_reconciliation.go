package httpapi

import (
	"net/http"
	"strings"
	"time"
)

func (s *Server) turnReconcile(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "turn reconciliation storage is unavailable"})
		return
	}
	var input struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Outcome = strings.TrimSpace(input.Outcome)
	input.Note = strings.TrimSpace(input.Note)
	reconciliation, err := s.store.RecordAgentTurnReconciliation(r.Context(), s.workspaceID, r.PathValue("id"), input.Outcome, input.Note, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	turn, err := s.store.AgentTurn(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	expectedClass, expectedStatus := reconciliationReadback(input.Outcome)
	if turn.RecoveryClass != expectedClass || turn.Status != expectedStatus || turn.ReconciliationNote != reconciliation.Note {
		write(w, http.StatusConflict, map[string]string{"error": "reconciliation outcome did not read back from the durable turn state"})
		return
	}
	history, err := s.store.AgentTurnReconciliations(r.Context(), s.workspaceID, turn.ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"reconciliation": reconciliation, "turn": turn, "history": history})
}

func reconciliationReadback(outcome string) (recoveryClass, status string) {
	switch outcome {
	case "no_effect_applied":
		return "safe_to_retry", "interrupted"
	case "effect_applied":
		return "external_effect_confirmed", "interrupted"
	case "still_unknown":
		return "unknown_external_effect", "needs_reconciliation"
	default:
		return "", ""
	}
}

func (s *Server) turnReconciliations(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "turn reconciliation storage is unavailable"})
		return
	}
	items, err := s.store.AgentTurnReconciliations(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
