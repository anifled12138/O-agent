package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/projectpolicy"
)

type projectPublicationRequest struct {
	TargetBranch      string `json:"targetBranch"`
	ExpectedRemoteSHA string `json:"expectedRemoteSha"`
	CommitSHA         string `json:"commitSha"`
}

func (s *Server) projectPublicationPreview(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.Project(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if s.agent == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "Agent service is unavailable"})
		return
	}
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	if branch == "" {
		branch = project.RemoteBranch
	}
	if project.Workdir == "" || project.RemoteRepoURL == "" || branch == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	preview, err := s.agent.InspectGitPublication(r.Context(), project.RemoteRepoURL, project.Workdir, branch)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, preview)
}

func (s *Server) projectPublicationList(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := s.store.Project(r.Context(), s.workspaceID, projectID); err != nil {
		fail(w, err)
		return
	}
	items, err := s.store.ListProjectPublications(r.Context(), s.workspaceID, projectID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) projectPublicationCreate(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	project, err := s.store.Project(r.Context(), s.workspaceID, projectID)
	if err != nil {
		fail(w, err)
		return
	}
	if project.Workdir == "" || project.RemoteRepoURL == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	_, normalizedURL, err := projectpolicy.ValidateRepositoryURL(project.RemoteRepoURL)
	if err != nil || normalizedURL != project.RemoteRepoURL {
		fail(w, domain.ErrInvalid)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 || strings.ContainsAny(key, "\x00\r\n") {
		fail(w, domain.ErrInvalid)
		return
	}
	var in projectPublicationRequest
	if !decode(w, r, &in) {
		return
	}
	in.TargetBranch = strings.TrimSpace(in.TargetBranch)
	in.ExpectedRemoteSHA = strings.ToLower(strings.TrimSpace(in.ExpectedRemoteSHA))
	in.CommitSHA = strings.ToLower(strings.TrimSpace(in.CommitSHA))
	if in.TargetBranch == "" || len(in.TargetBranch) > 255 || strings.ContainsAny(in.TargetBranch, "\x00\r\n") || !validPublicationSHA(in.ExpectedRemoteSHA) || !validPublicationSHA(in.CommitSHA) {
		fail(w, domain.ErrInvalid)
		return
	}
	if s.agent == nil {
		fail(w, errors.New("Agent service is unavailable"))
		return
	}
	existing, lookupErr := s.store.ProjectPublicationByIdempotencyKey(r.Context(), s.workspaceID, key)
	var submodules []domain.ProjectPublicationSubmodule
	if lookupErr == nil {
		submodules = existing.Submodules
	} else if errors.Is(lookupErr, domain.ErrNotFound) {
		submodules, err = s.agent.PlanProjectSubmodulePublications(r.Context(), project.RemoteRepoURL, project.Workdir, in.CommitSHA)
		if err != nil {
			fail(w, err)
			return
		}
	} else {
		fail(w, lookupErr)
		return
	}
	rawID := make([]byte, 16)
	if _, err := rand.Read(rawID); err != nil {
		fail(w, err)
		return
	}
	record, created, err := s.store.BeginProjectPublication(r.Context(), domain.ProjectPublication{
		ID: "pub_" + hex.EncodeToString(rawID), UserID: s.workspaceID, ProjectID: projectID,
		IdempotencyKey: key, TargetBranch: in.TargetBranch, ExpectedRemoteSHA: in.ExpectedRemoteSHA, CommitSHA: in.CommitSHA, Submodules: submodules,
	})
	if err != nil {
		fail(w, err)
		return
	}
	if created {
		if !s.beginProjectPublicationOperation(record.ID) {
			record, err = s.store.ProjectPublication(r.Context(), s.workspaceID, projectID, record.ID)
			if err != nil {
				fail(w, err)
				return
			}
			write(w, http.StatusOK, record)
			return
		}
		defer s.endProjectPublicationOperation(record.ID)
		record, ready, err := s.publishProjectSubmoduleRefs(r.Context(), project, record)
		if err != nil {
			fail(w, err)
			return
		}
		if !ready {
			write(w, http.StatusOK, record)
			return
		}
		remoteSHA, pushAttempted, pushErr := s.agent.PublishProjectCommit(r.Context(), project.RemoteRepoURL, project.Workdir, in.TargetBranch, in.ExpectedRemoteSHA, in.CommitSHA)
		if pushErr == nil {
			record, err = s.store.CompleteProjectPublication(r.Context(), s.workspaceID, projectID, record.ID, remoteSHA)
			if err != nil {
				fail(w, err)
				return
			}
			write(w, http.StatusOK, record)
			return
		}
		if !pushAttempted {
			record, err = s.store.SetProjectPublicationState(r.Context(), s.workspaceID, projectID, record.ID, "failed", remoteSHA, pushErr.Error())
			if err != nil {
				fail(w, errors.Join(pushErr, err))
				return
			}
			write(w, http.StatusOK, record)
			return
		}
		// The push may have reached the Git server even when the client timed
		// out. A read-back can classify only an exact commit or unchanged base.
		record, err = s.reconcileProjectPublication(r.Context(), project, record)
		if err != nil {
			fail(w, errors.Join(pushErr, err))
			return
		}
		write(w, http.StatusOK, record)
		return
	}
	write(w, http.StatusOK, record)
}

func (s *Server) projectPublicationReconcile(w http.ResponseWriter, r *http.Request) {
	projectID, publicationID := r.PathValue("id"), r.PathValue("publicationId")
	project, err := s.store.Project(r.Context(), s.workspaceID, projectID)
	if err != nil {
		fail(w, err)
		return
	}
	record, err := s.store.ProjectPublication(r.Context(), s.workspaceID, projectID, publicationID)
	if err != nil {
		fail(w, err)
		return
	}
	if record.Status != "publishing" && record.Status != "needs_reconciliation" {
		write(w, http.StatusOK, record)
		return
	}
	if !s.beginProjectPublicationOperation(record.ID) {
		record, err = s.store.ProjectPublication(r.Context(), s.workspaceID, projectID, publicationID)
		if err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, record)
		return
	}
	defer s.endProjectPublicationOperation(record.ID)
	record, err = s.reconcileProjectPublication(r.Context(), project, record)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, record)
}

func (s *Server) reconcileProjectPublication(ctx context.Context, project domain.Project, record domain.ProjectPublication) (domain.ProjectPublication, error) {
	if s.agent == nil {
		return record, errors.New("Agent service is unavailable")
	}
	var ready bool
	record, ready, err := s.reconcileProjectSubmoduleRefs(ctx, project, record)
	if err != nil || !ready {
		return record, err
	}
	remoteSHA, err := s.agent.ReadProjectRemoteBranch(ctx, project.RemoteRepoURL, project.Workdir, record.TargetBranch)
	if err != nil {
		return s.store.SetProjectPublicationState(ctx, record.UserID, record.ProjectID, record.ID, "needs_reconciliation", "", err.Error())
	}
	if strings.EqualFold(remoteSHA, record.CommitSHA) {
		return s.store.CompleteProjectPublication(ctx, record.UserID, record.ProjectID, record.ID, remoteSHA)
	}
	if strings.EqualFold(remoteSHA, record.ExpectedRemoteSHA) {
		return s.store.SetProjectPublicationState(ctx, record.UserID, record.ProjectID, record.ID, "failed", remoteSHA, "remote branch still matches the expected baseline; push was not observed")
	}
	return s.store.SetProjectPublicationState(ctx, record.UserID, record.ProjectID, record.ID, "needs_reconciliation", remoteSHA, fmt.Sprintf("remote branch is at %q; expected either baseline %q or requested commit %q", remoteSHA, record.ExpectedRemoteSHA, record.CommitSHA))
}

func (s *Server) publishProjectSubmoduleRefs(ctx context.Context, project domain.Project, record domain.ProjectPublication) (domain.ProjectPublication, bool, error) {
	for _, item := range record.Submodules {
		if item.Status == "published" && item.RemoteSHA == item.CommitSHA {
			continue
		}
		if item.Status != "pending" {
			return s.reconcileOneProjectSubmodule(ctx, project, record, item)
		}
		record, err := s.store.SetProjectPublicationSubmoduleState(ctx, record.UserID, record.ProjectID, record.ID, item.Path, "publishing", "", "")
		if err != nil {
			return record, false, err
		}
		remoteSHA, attempted, pushErr := s.agent.PublishProjectSubmoduleRef(ctx, project.Workdir, item)
		if pushErr == nil {
			record, err = s.store.SetProjectPublicationSubmoduleState(ctx, record.UserID, record.ProjectID, record.ID, item.Path, "published", remoteSHA, "")
			if err != nil {
				return record, false, err
			}
			continue
		}
		status := "failed"
		if attempted {
			status = "needs_reconciliation"
		}
		message := pushErr.Error()
		record, err = s.store.SetProjectPublicationSubmoduleState(ctx, record.UserID, record.ProjectID, record.ID, item.Path, status, remoteSHA, message)
		if err != nil {
			return record, false, errors.Join(pushErr, err)
		}
		parentStatus := "failed"
		if attempted {
			parentStatus = "needs_reconciliation"
		}
		record, err = s.store.SetProjectPublicationState(ctx, record.UserID, record.ProjectID, record.ID, parentStatus, "", message)
		return record, false, err
	}
	return record, allPublicationSubmodulesPublished(record), nil
}

func (s *Server) reconcileProjectSubmoduleRefs(ctx context.Context, project domain.Project, record domain.ProjectPublication) (domain.ProjectPublication, bool, error) {
	for _, item := range record.Submodules {
		if item.Status == "failed" {
			if record.Status == "publishing" || record.Status == "needs_reconciliation" {
				record, err := s.store.SetProjectPublicationState(ctx, record.UserID, record.ProjectID, record.ID, "failed", "", item.Error)
				return record, false, err
			}
			return record, false, nil
		}
		if item.Status == "pending" {
			return s.publishProjectSubmoduleRefs(ctx, project, record)
		}
		var err error
		record, _, err = s.reconcileOneProjectSubmodule(ctx, project, record, item)
		if err != nil || !publicationSubmodulePublished(record, item.Path) {
			return record, false, err
		}
	}
	return record, allPublicationSubmodulesPublished(record), nil
}

func (s *Server) reconcileOneProjectSubmodule(ctx context.Context, project domain.Project, record domain.ProjectPublication, item domain.ProjectPublicationSubmodule) (domain.ProjectPublication, bool, error) {
	remoteSHA, err := s.agent.ReadProjectSubmoduleRef(ctx, project.Workdir, item)
	if err != nil {
		record, stateErr := s.store.SetProjectPublicationSubmoduleState(ctx, record.UserID, record.ProjectID, record.ID, item.Path, "needs_reconciliation", "", err.Error())
		if stateErr != nil {
			return record, false, errors.Join(err, stateErr)
		}
		if record.Status == "publishing" {
			record, stateErr = s.store.SetProjectPublicationState(ctx, record.UserID, record.ProjectID, record.ID, "needs_reconciliation", "", err.Error())
		}
		return record, false, stateErr
	}
	if remoteSHA == item.CommitSHA {
		record, err = s.store.SetProjectPublicationSubmoduleState(ctx, record.UserID, record.ProjectID, record.ID, item.Path, "published", remoteSHA, "")
		return record, err == nil && allPublicationSubmodulesPublished(record), err
	}
	message := fmt.Sprintf("submodule ref read-back is %q; expected %q", remoteSHA, item.CommitSHA)
	state := "needs_reconciliation"
	if item.Status == "pending" {
		state = "failed"
	}
	record, err = s.store.SetProjectPublicationSubmoduleState(ctx, record.UserID, record.ProjectID, record.ID, item.Path, state, remoteSHA, message)
	if err != nil {
		return record, false, err
	}
	parentState := "needs_reconciliation"
	if state == "failed" {
		parentState = "failed"
	}
	record, err = s.store.SetProjectPublicationState(ctx, record.UserID, record.ProjectID, record.ID, parentState, "", message)
	return record, false, err
}

func allPublicationSubmodulesPublished(record domain.ProjectPublication) bool {
	for _, item := range record.Submodules {
		if item.Status != "published" || item.RemoteSHA != item.CommitSHA {
			return false
		}
	}
	return true
}

func publicationSubmodulePublished(record domain.ProjectPublication, path string) bool {
	for _, item := range record.Submodules {
		if item.Path == path {
			return item.Status == "published" && item.RemoteSHA == item.CommitSHA
		}
	}
	return false
}

func validPublicationSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
