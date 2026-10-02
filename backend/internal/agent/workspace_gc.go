package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

// ReapCompletedScratchWorkspaces releases old, completed cloud scratch
// workspaces only after their immutable workspace_output artifact has been
// verified from disk. Git worktrees and any workspace with unresolved task,
// project, lease, checkpoint, or artifact references are retained.
func (s *Service) ReapCompletedScratchWorkspaces(ctx context.Context, now time.Time, retention time.Duration) (int, error) {
	if s == nil || s.store == nil || s.artifacts == nil || strings.TrimSpace(s.workspaceRoot) == "" || retention < 0 {
		return 0, domain.ErrInvalid
	}
	workspaces, err := s.store.ExecutionTaskWorkspaces(ctx)
	if err != nil {
		return 0, fmt.Errorf("read task workspaces for safe retention cleanup: %w", err)
	}
	cutoff := now.UTC().Add(-retention)
	removed := 0
	var failures []error
	for _, binding := range workspaces {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(err, errors.Join(failures...))
		}
		eligible, artifactID, eligibilityErr := s.scratchWorkspaceGCCandidate(ctx, binding, cutoff, retention)
		if eligibilityErr != nil {
			failures = append(failures, fmt.Errorf("inspect scratch workspace %s for cleanup: %w", binding.TaskID, eligibilityErr))
			continue
		}
		if !eligible {
			continue
		}
		if err := s.verifyWorkspaceOutputArtifact(ctx, binding.UserID, binding.TaskID, artifactID); err != nil {
			failures = append(failures, fmt.Errorf("preserve scratch workspace %s because its durable output failed verification: %w", binding.TaskID, err))
			continue
		}
		if binding.Status != "releasing" {
			updated, err := s.store.SetExecutionTaskWorkspaceState(ctx, binding.UserID, binding.TaskID, binding.Status, "releasing", binding.QuotaState, binding.QuotaProjectID, binding.QuotaLimitBytes, "", now.UTC())
			if err != nil {
				failures = append(failures, fmt.Errorf("reserve scratch workspace %s for cleanup: %w", binding.TaskID, err))
				continue
			}
			binding = updated
		}
		referenced, referenceErr := s.store.ProjectReferencesWorkdir(ctx, binding.Workdir)
		if referenceErr != nil || referenced {
			if referenceErr == nil {
				referenceErr = fmt.Errorf("scratch workspace gained a Project reference during cleanup: %w", domain.ErrConflict)
			}
			failures = append(failures, s.markWorkspaceGCFailure(ctx, binding, referenceErr))
			continue
		}
		if err := s.removeVerifiedScratchWorkspace(binding); err != nil {
			failures = append(failures, s.markWorkspaceGCFailure(ctx, binding, err))
			continue
		}
		if err := s.releaseExecutionTaskQuota(ctx, binding); err != nil {
			failures = append(failures, s.markWorkspaceGCFailure(ctx, binding, err))
			continue
		}
		if _, err := s.store.SetExecutionTaskWorkspaceState(ctx, binding.UserID, binding.TaskID, "releasing", "released", "released", binding.QuotaProjectID, binding.QuotaLimitBytes, "", now.UTC()); err != nil {
			failures = append(failures, fmt.Errorf("persist released workspace %s after verified filesystem and quota read-back: %w", binding.TaskID, err))
			continue
		}
		readBack, err := s.store.ExecutionTaskWorkspace(ctx, binding.UserID, binding.TaskID)
		if err != nil || readBack.Status != "released" || readBack.QuotaState != "released" {
			failures = append(failures, errors.Join(fmt.Errorf("workspace %s release did not read back", binding.TaskID), err))
			continue
		}
		removed++
	}
	return removed, errors.Join(failures...)
}

func (s *Service) scratchWorkspaceGCCandidate(ctx context.Context, binding storage.ExecutionTaskWorkspace, cutoff time.Time, retention time.Duration) (bool, string, error) {
	pendingRelease := binding.Status == "releasing" || (binding.Status == "degraded" && strings.HasPrefix(binding.Error, "workspace_gc_failed:"))
	if binding.SourceProjectID != "" || (binding.Status != "ready" && binding.Status != "degraded" && binding.Status != "releasing") || (!pendingRelease && (retention == 0 || binding.CreatedAt.After(cutoff))) {
		return false, "", nil
	}
	task, err := s.store.ExecutionTask(ctx, binding.UserID, binding.TaskID)
	if err != nil {
		return false, "", err
	}
	if task.NodeID != storage.CloudExecutionNodeID(binding.UserID) || task.Status != "completed" || task.LeaseUntil != nil || task.CancelRequested || (!pendingRelease && task.UpdatedAt.After(cutoff)) || len(task.Result) == 0 {
		return false, "", nil
	}
	var result struct {
		WorkspaceOutputArtifact struct {
			ID       string `json:"id"`
			SHA256   string `json:"sha256"`
			ByteSize int64  `json:"byteSize"`
		} `json:"workspaceOutputArtifact"`
	}
	if err := json.Unmarshal(task.Result, &result); err != nil {
		return false, "", fmt.Errorf("decode completed task result: %w", err)
	}
	artifactID := strings.TrimSpace(result.WorkspaceOutputArtifact.ID)
	if artifactID == "" || result.WorkspaceOutputArtifact.SHA256 == "" || result.WorkspaceOutputArtifact.ByteSize <= 0 {
		return false, "", nil
	}
	artifacts, err := s.store.ExecutionTaskArtifacts(ctx, binding.UserID, binding.TaskID)
	if err != nil {
		return false, "", err
	}
	outputAttached := false
	for _, artifact := range artifacts {
		if artifact.ID == artifactID && artifact.Role == "workspace_output" {
			if artifact.SHA256 != result.WorkspaceOutputArtifact.SHA256 || artifact.ByteSize != result.WorkspaceOutputArtifact.ByteSize {
				return false, "", fmt.Errorf("task result and durable workspace artifact manifest differ: %w", domain.ErrConflict)
			}
			outputAttached = true
		}
		if artifact.Role == "continuation_checkpoint" {
			return false, "", nil
		}
	}
	if !outputAttached {
		return false, "", nil
	}
	if _, err := s.store.ExecutionTaskHandoffCheckpoint(ctx, binding.UserID, binding.TaskID); err == nil {
		return false, "", nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return false, "", err
	}
	referenced, err := s.store.ProjectReferencesWorkdir(ctx, binding.Workdir)
	if err != nil || referenced {
		return false, "", err
	}
	wantPath, err := expectedScratchWorkspacePath(s.workspaceRoot, binding.TaskID)
	if err != nil {
		return false, "", err
	}
	if filepath.Clean(binding.Workdir) != wantPath {
		return false, "", fmt.Errorf("durable scratch path does not match task-derived path: %w", domain.ErrConflict)
	}
	return true, artifactID, nil
}

func expectedScratchWorkspacePath(root, taskID string) (string, error) {
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte("scratch\x00" + taskID))
	path := filepath.Join(resolvedRoot, ".o-projects", "scratch-"+hex.EncodeToString(digest[:12]))
	if !pathWithin(resolvedRoot, path) {
		return "", domain.ErrConflict
	}
	return filepath.Clean(path), nil
}

func (s *Service) verifyWorkspaceOutputArtifact(ctx context.Context, userID, taskID, artifactID string) error {
	items, err := s.store.ExecutionTaskArtifacts(ctx, userID, taskID)
	if err != nil {
		return err
	}
	var expected *storage.ExecutionTaskArtifact
	for index := range items {
		if items[index].ID == artifactID && items[index].Role == "workspace_output" {
			expected = &items[index]
			break
		}
	}
	if expected == nil {
		return domain.ErrNotFound
	}
	artifact, file, err := s.artifacts.Open(ctx, userID, artifactID)
	if err != nil {
		return err
	}
	closeErr := file.Close()
	if closeErr != nil {
		return closeErr
	}
	if artifact.SHA256 != expected.SHA256 || artifact.ByteSize != expected.ByteSize || artifact.ID != expected.ID {
		return domain.ErrConflict
	}
	return nil
}

func (s *Service) removeVerifiedScratchWorkspace(binding storage.ExecutionTaskWorkspace) error {
	root, err := filepath.EvalSymlinks(filepath.Clean(s.workspaceRoot))
	if err != nil {
		return err
	}
	projectsRoot := filepath.Join(root, ".o-projects")
	projectsInfo, err := os.Lstat(projectsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !projectsInfo.IsDir() || projectsInfo.Mode()&os.ModeSymlink != 0 {
		return errors.Join(fmt.Errorf("scratch workspace parent is not a plain directory: %w", domain.ErrConflict), err)
	}
	want, err := expectedScratchWorkspacePath(root, binding.TaskID)
	if err != nil || filepath.Clean(binding.Workdir) != want || filepath.Dir(want) != projectsRoot {
		return errors.Join(fmt.Errorf("refuse to remove a noncanonical task workspace: %w", domain.ErrConflict), err)
	}
	info, err := os.Lstat(want)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("scratch workspace is not a plain directory: %w", domain.ErrConflict)
	}
	resolved, err := filepath.EvalSymlinks(want)
	if err != nil || filepath.Clean(resolved) != filepath.Clean(want) || !pathWithin(projectsRoot, resolved) {
		return errors.Join(fmt.Errorf("scratch workspace path changed before cleanup: %w", domain.ErrConflict), err)
	}
	if err := os.RemoveAll(want); err != nil {
		return err
	}
	if _, err := os.Lstat(want); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(fmt.Errorf("scratch workspace remains after cleanup: %w", domain.ErrConflict), err)
	}
	return nil
}

func (s *Service) markWorkspaceGCFailure(ctx context.Context, binding storage.ExecutionTaskWorkspace, cause error) error {
	reason := boundedWorkspaceQuotaReason("workspace_gc_failed: " + cause.Error())
	_, updateErr := s.store.SetExecutionTaskWorkspaceState(ctx, binding.UserID, binding.TaskID, "releasing", "degraded", "degraded", binding.QuotaProjectID, binding.QuotaLimitBytes, reason, time.Now().UTC())
	return errors.Join(cause, updateErr)
}
