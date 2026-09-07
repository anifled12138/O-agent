package httpapi

import (
	"net/http"

	"axiom.local/agent/internal/bootstrap"
	"axiom.local/agent/internal/evalharness"
	"axiom.local/agent/internal/evolution"
)

func (s *Server) generationList(w http.ResponseWriter, r *http.Request) {
	items, err := s.evolution.ListGenerations(r.Context(), currentUser(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) generationCreateCandidate(w http.ResponseWriter, r *http.Request) {
	var input evolution.CandidateInput
	if !decode(w, r, &input) {
		return
	}
	item, err := s.evolution.CreateCandidate(r.Context(), currentUser(r).ID, input)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, item)
}

func (s *Server) generationPromote(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ExperimentID string `json:"experimentId"`
	}
	if !decode(w, r, &input) {
		return
	}
	item, err := s.evolution.Promote(r.Context(), currentUser(r).ID, r.PathValue("id"), input.ExperimentID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, item)
}

func (s *Server) challengeList(w http.ResponseWriter, r *http.Request) {
	items, err := s.evolution.ListChallenges(r.Context(), currentUser(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) challengeCreate(w http.ResponseWriter, r *http.Request) {
	var input evolution.ChallengeInput
	if !decode(w, r, &input) {
		return
	}
	item, err := s.evolution.CreateChallenge(r.Context(), currentUser(r).ID, input)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, item)
}

func (s *Server) challengeBootstrap(w http.ResponseWriter, r *http.Request) {
	var input bootstrap.GenerateInput
	if !decode(w, r, &input) {
		return
	}
	items, err := s.bootstrap.GenerateCandidates(r.Context(), currentUser(r).ID, r.PathValue("id"), input)
	if err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusCreated, items)
}

func (s *Server) experimentList(w http.ResponseWriter, r *http.Request) {
	items, err := s.evals.List(r.Context(), currentUser(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) experimentStart(w http.ResponseWriter, r *http.Request) {
	var input evalharness.StartInput
	if !decode(w, r, &input) {
		return
	}
	item, err := s.evals.Start(r.Context(), currentUser(r).ID, input)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusAccepted, item)
}

func (s *Server) experimentGet(w http.ResponseWriter, r *http.Request) {
	item, err := s.evals.Get(r.Context(), currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, item)
}
