package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/projectquota"
	"axiom.local/agent/internal/storage"
)

// ConfigureWorkspaceQuota installs the privileged Linux project quota client
// and the opt-in per-task block hard limit. Local and cloud runtimes leave
// it unset by default; explicitly enabled quotas retain fail-closed verification.
func (s *Service) ConfigureWorkspaceQuota(manager projectquota.Manager, limitBytes int64) error {
	if s == nil || (manager == nil) != (limitBytes == 0) || limitBytes < 0 {
		return domain.ErrInvalid
	}
	s.workspaceQuota = manager
	s.workspaceQuotaBytes = limitBytes
	return nil
}

// ReconcileWorkspaceQuotas compares durable workspace lifecycle states with
// the helper's kernel read-back after startup. Interrupted preparation is
// quarantined as degraded so normal task retry code must re-validate it before
// execution. Released rows may clear a leftover quota only through the helper,
// which refuses to clear limits while workspace files or quota usage remain.
// This method never removes workspace data.
func (s *Service) ReconcileWorkspaceQuotas(ctx context.Context) (int, error) {
	if s == nil || s.store == nil {
		return 0, domain.ErrInvalid
	}
	if s.workspaceQuota == nil {
		return 0, nil
	}
	workspaces, err := s.store.ExecutionTaskWorkspaces(ctx)
	if err != nil {
		return 0, fmt.Errorf("read durable execution workspace registry: %w", err)
	}
	var degraded int
	for _, binding := range workspaces {
		switch binding.Status {
		case "ready":
			if verifyErr := s.verifyExecutionTaskQuota(ctx, binding); verifyErr != nil {
				reason := boundedWorkspaceQuotaReason("quota_reconciliation_failed: " + verifyErr.Error())
				if _, updateErr := s.persistWorkspaceQuotaState(ctx, binding, "degraded", "degraded", reason); updateErr != nil {
					return degraded, errors.Join(fmt.Errorf("persist degraded task quota reconciliation state for %s: %w", binding.TaskID, updateErr), verifyErr)
				}
			}
		case "preparing":
			// A preparing row surviving process startup is an interrupted
			// multi-step mutation. Record the observed kernel state but keep it
			// unavailable until the normal task path retries and verifies every
			// workspace/Git invariant.
			quotaState := "degraded"
			reason := "startup_interrupted_workspace_preparation"
			if quota, inspectErr := s.inspectAllocatedWorkspaceQuota(ctx, binding); inspectErr == nil && quota.Applied {
				quotaState = "applied"
			} else if inspectErr != nil {
				reason = appendWorkspaceQuotaReason(reason, "quota_inspection_failed: "+inspectErr.Error())
			}
			if _, updateErr := s.persistWorkspaceQuotaState(ctx, binding, "degraded", quotaState, reason); updateErr != nil {
				return degraded, fmt.Errorf("persist interrupted task workspace state for %s: %w", binding.TaskID, updateErr)
			}
		case "degraded":
			// Preserve the task's degraded status and error reason. If the
			// kernel allocation survived, persist that fact so a retry can
			// inspect and reuse the same immutable allocation.
			quotaState := "degraded"
			reason := binding.Error
			if quota, inspectErr := s.inspectAllocatedWorkspaceQuota(ctx, binding); inspectErr == nil && quota.Applied {
				quotaState = "applied"
			} else if inspectErr != nil {
				reason = appendWorkspaceQuotaReason(reason, "quota_inspection_failed: "+inspectErr.Error())
			}
			if quotaState != binding.QuotaState || reason != binding.Error {
				if _, updateErr := s.persistWorkspaceQuotaState(ctx, binding, "degraded", quotaState, reason); updateErr != nil {
					return degraded, fmt.Errorf("persist observed degraded task quota state for %s: %w", binding.TaskID, updateErr)
				}
			}
		case "released":
			if binding.QuotaProjectID > 0 {
				if releaseErr := s.releaseExecutionTaskQuota(ctx, binding); releaseErr != nil {
					reason := boundedWorkspaceQuotaReason("released_quota_reconciliation_failed: " + releaseErr.Error())
					if _, updateErr := s.persistWorkspaceQuotaState(ctx, binding, "degraded", "degraded", reason); updateErr != nil {
						return degraded, errors.Join(fmt.Errorf("persist inconsistent released quota state for %s: %w", binding.TaskID, updateErr), releaseErr)
					}
					degraded++
					continue
				}
			}
			if binding.QuotaState != "released" || binding.Error != "" {
				if _, updateErr := s.persistWorkspaceQuotaState(ctx, binding, "released", "released", ""); updateErr != nil {
					return degraded, fmt.Errorf("persist released task quota read-back for %s: %w", binding.TaskID, updateErr)
				}
			}
		case "releasing":
			// Cleanup is resumed after the artifact store is attached. Never
			// reopen this workspace for execution while its durable release is
			// pending.
		default:
			return degraded, fmt.Errorf("unknown task workspace state %q for %s: %w", binding.Status, binding.TaskID, domain.ErrConflict)
		}
		if binding.Status == "degraded" || binding.Status == "preparing" || binding.Status == "ready" {
			current, readErr := s.store.ExecutionTaskWorkspace(ctx, binding.UserID, binding.TaskID)
			if readErr != nil {
				return degraded, fmt.Errorf("read reconciled task workspace %s: %w", binding.TaskID, readErr)
			}
			if current.Status == "degraded" {
				degraded++
			}
		}
	}
	return degraded, nil
}

func (s *Service) inspectAllocatedWorkspaceQuota(ctx context.Context, binding storage.ExecutionTaskWorkspace) (projectquota.WorkspaceQuota, error) {
	if s.workspaceQuota == nil || binding.QuotaProjectID <= 0 || binding.QuotaProjectID > int64(^uint32(0)) || binding.QuotaLimitBytes <= 0 {
		return projectquota.WorkspaceQuota{}, domain.ErrConflict
	}
	relativePath, err := quotaRelativePath(s.workspaceRoot, binding.Workdir)
	if err != nil {
		return projectquota.WorkspaceQuota{}, err
	}
	quota, err := s.workspaceQuota.Inspect(ctx, relativePath, uint32(binding.QuotaProjectID))
	if err != nil {
		return projectquota.WorkspaceQuota{}, err
	}
	if !quota.Applied || quota.ProjectID != uint32(binding.QuotaProjectID) || quota.LimitBytes != binding.QuotaLimitBytes {
		return quota, domain.ErrConflict
	}
	return quota, nil
}

func (s *Service) persistWorkspaceQuotaState(ctx context.Context, binding storage.ExecutionTaskWorkspace, status, quotaState, reason string) (storage.ExecutionTaskWorkspace, error) {
	reason = boundedWorkspaceQuotaReason(reason)
	updated, err := s.store.SetExecutionTaskWorkspaceState(ctx, binding.UserID, binding.TaskID, binding.Status, status, quotaState, binding.QuotaProjectID, binding.QuotaLimitBytes, reason, time.Now().UTC())
	if err != nil {
		return storage.ExecutionTaskWorkspace{}, err
	}
	if updated.Status != status || updated.QuotaState != quotaState || updated.QuotaProjectID != binding.QuotaProjectID || updated.QuotaLimitBytes != binding.QuotaLimitBytes || updated.Error != reason {
		return storage.ExecutionTaskWorkspace{}, fmt.Errorf("task quota reconciliation did not read back for %s: %w", binding.TaskID, domain.ErrConflict)
	}
	return updated, nil
}

func boundedWorkspaceQuotaReason(reason string) string {
	if len(reason) > 4096 {
		return reason[:4096]
	}
	return reason
}

func appendWorkspaceQuotaReason(existing, next string) string {
	existing = strings.TrimSpace(existing)
	next = strings.TrimSpace(next)
	if next == "" || strings.Contains(existing, next) {
		return boundedWorkspaceQuotaReason(existing)
	}
	if existing == "" {
		return boundedWorkspaceQuotaReason(next)
	}
	return boundedWorkspaceQuotaReason(existing + "; " + next)
}

func (s *Service) prepareExecutionTaskQuota(ctx context.Context, userID, taskID string, binding storage.ExecutionTaskWorkspace) (storage.ExecutionTaskWorkspace, error) {
	if s.workspaceQuota == nil {
		return binding, nil
	}
	if binding.Status != "preparing" || s.workspaceQuotaBytes <= 0 {
		return storage.ExecutionTaskWorkspace{}, fmt.Errorf("task workspace is not in a state that can reserve its kernel quota: %w", domain.ErrConflict)
	}
	allocated, err := s.store.ReserveExecutionTaskWorkspaceQuota(ctx, userID, taskID, s.workspaceQuotaBytes, time.Now().UTC())
	if err != nil {
		return storage.ExecutionTaskWorkspace{}, fmt.Errorf("reserve durable task quota identity: %w", err)
	}
	if allocated.QuotaProjectID <= 0 || allocated.QuotaProjectID > int64(^uint32(0)) {
		return allocated, fmt.Errorf("durable task quota project ID is outside the Linux ABI range: %w", domain.ErrConflict)
	}
	relativePath, err := quotaRelativePath(s.workspaceRoot, allocated.Workdir)
	if err != nil {
		return allocated, err
	}
	quota, err := s.workspaceQuota.Inspect(ctx, relativePath, uint32(allocated.QuotaProjectID))
	if err != nil {
		return allocated, fmt.Errorf("inspect task quota before workspace creation: %w", err)
	}
	if quota.Applied {
		if quota.ProjectID != uint32(allocated.QuotaProjectID) || quota.LimitBytes != allocated.QuotaLimitBytes {
			return allocated, fmt.Errorf("kernel task quota does not match the durable allocation: %w", domain.ErrConflict)
		}
		updated, persistErr := s.persistAppliedWorkspaceQuota(ctx, allocated)
		if persistErr != nil {
			return allocated, persistErr
		}
		return updated, persistErr
	}
	quota, err = s.workspaceQuota.Apply(ctx, relativePath, uint32(allocated.QuotaProjectID), allocated.QuotaLimitBytes)
	if err != nil {
		return allocated, fmt.Errorf("apply kernel task workspace hard quota: %w", err)
	}
	if !quota.Applied || quota.ProjectID != uint32(allocated.QuotaProjectID) || quota.LimitBytes != allocated.QuotaLimitBytes {
		return allocated, fmt.Errorf("kernel task workspace hard quota did not verify after apply: %w", domain.ErrConflict)
	}
	updated, persistErr := s.persistAppliedWorkspaceQuota(ctx, allocated)
	if persistErr != nil {
		return allocated, persistErr
	}
	return updated, persistErr
}

func (s *Service) persistAppliedWorkspaceQuota(ctx context.Context, binding storage.ExecutionTaskWorkspace) (storage.ExecutionTaskWorkspace, error) {
	updated, err := s.store.SetExecutionTaskWorkspaceState(ctx, binding.UserID, binding.TaskID, binding.Status, binding.Status, "applied", binding.QuotaProjectID, binding.QuotaLimitBytes, binding.Error, time.Now().UTC())
	if err != nil {
		return storage.ExecutionTaskWorkspace{}, fmt.Errorf("persist verified kernel task quota state: %w", err)
	}
	if updated.QuotaState != "applied" || updated.QuotaProjectID != binding.QuotaProjectID || updated.QuotaLimitBytes != binding.QuotaLimitBytes {
		return storage.ExecutionTaskWorkspace{}, fmt.Errorf("verified kernel quota allocation did not read back from storage: %w", domain.ErrConflict)
	}
	return updated, nil
}

func (s *Service) verifyExecutionTaskQuota(ctx context.Context, binding storage.ExecutionTaskWorkspace) error {
	if s.workspaceQuota == nil {
		if binding.QuotaProjectID != 0 || binding.QuotaLimitBytes != 0 {
			return fmt.Errorf("existing workspace quota allocation requires its configured helper; create a new unmetered task or re-enable quota verification: %w", domain.ErrConflict)
		}
		return nil
	}
	if binding.QuotaState != "applied" || binding.QuotaProjectID <= 0 || binding.QuotaProjectID > int64(^uint32(0)) || binding.QuotaLimitBytes <= 0 {
		return fmt.Errorf("task workspace quota is not durably marked applied: %w", domain.ErrConflict)
	}
	relativePath, err := quotaRelativePath(s.workspaceRoot, binding.Workdir)
	if err != nil {
		return err
	}
	quota, err := s.workspaceQuota.Inspect(ctx, relativePath, uint32(binding.QuotaProjectID))
	if err != nil {
		return fmt.Errorf("read back task workspace kernel quota: %w", err)
	}
	if !quota.Applied || quota.ProjectID != uint32(binding.QuotaProjectID) || quota.LimitBytes != binding.QuotaLimitBytes {
		return fmt.Errorf("task workspace kernel quota differs from its durable allocation: %w", domain.ErrConflict)
	}
	return nil
}

func (s *Service) releaseExecutionTaskQuota(ctx context.Context, binding storage.ExecutionTaskWorkspace) error {
	if binding.QuotaProjectID == 0 {
		return nil
	}
	if s.workspaceQuota == nil {
		return fmt.Errorf("workspace has a durable quota allocation but no quota helper is configured: %w", domain.ErrConflict)
	}
	if binding.QuotaState != "applied" && binding.QuotaState != "degraded" && binding.QuotaState != "preparing" && binding.QuotaState != "released" && binding.QuotaState != "not_configured" && binding.QuotaState != "unavailable" {
		return fmt.Errorf("workspace quota cannot be released from state %s: %w", binding.QuotaState, domain.ErrConflict)
	}
	if binding.QuotaProjectID < 0 || binding.QuotaProjectID > int64(^uint32(0)) {
		return domain.ErrConflict
	}
	relativePath, err := quotaRelativePath(s.workspaceRoot, binding.Workdir)
	if err != nil {
		return err
	}
	quota, err := s.workspaceQuota.Release(ctx, relativePath, uint32(binding.QuotaProjectID))
	if err != nil {
		return fmt.Errorf("release task workspace kernel quota: %w", err)
	}
	if quota.Applied || quota.ProjectID != uint32(binding.QuotaProjectID) || quota.LimitBytes != 0 {
		return fmt.Errorf("released task workspace quota did not read back as cleared: %w", domain.ErrConflict)
	}
	return nil
}

func quotaRelativePath(root, workdir string) (string, error) {
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	absoluteWorkdir, err := filepath.Abs(filepath.Clean(workdir))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteWorkdir)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.Join(fmt.Errorf("task workspace path is outside the configured cloud root: %w", domain.ErrConflict), err)
	}
	path := filepath.ToSlash(relative)
	if strings.Contains(path, "\\") {
		return "", domain.ErrConflict
	}
	return path, nil
}
