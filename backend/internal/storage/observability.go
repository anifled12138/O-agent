package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"

	"axiom.local/agent/internal/domain"
)

func (s *Store) ToolUsageMetrics(ctx context.Context, userID string, since time.Time) ([]domain.ToolUsageMetric, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.kind,e.details_json,e.created_at FROM agent_trace_events e
JOIN agent_turns t ON t.id=e.turn_id WHERE t.user_id=? AND e.created_at>=?
AND e.kind IN ('tool.started','tool.completed','permission.checked','context.compacted') ORDER BY e.created_at`, userID, since)
	if err != nil {
		return nil, err
	}
	metrics := map[string]*domain.ToolUsageMetric{}
	get := func(name, pluginID, releaseID string, projects ...string) *domain.ToolUsageMetric {
		projectID := ""
		if len(projects) > 0 {
			projectID = projects[0]
		}
		key := name + "\x00" + projectID + "\x00" + pluginID + "\x00" + releaseID
		item := metrics[key]
		if item == nil {
			item = &domain.ToolUsageMetric{ToolName: name, ProjectID: projectID, PluginID: pluginID, ReleaseID: releaseID}
			metrics[key] = item
		}
		return item
	}
	for rows.Next() {
		var kind string
		var raw []byte
		var at time.Time
		if err := rows.Scan(&kind, &raw, &at); err != nil {
			rows.Close()
			return nil, err
		}
		var details struct {
			Name           string `json:"name"`
			Tool           string `json:"tool"`
			PluginID       string `json:"pluginId"`
			ReleaseID      string `json:"releaseId"`
			Outcome        string `json:"outcome"`
			OK             bool   `json:"ok"`
			DurationMillis int64  `json:"durationMillis"`
			Scope          string `json:"scope"`
			OriginalChars  int64  `json:"originalChars"`
			CompactedChars int64  `json:"compactedChars"`
		}
		if err := json.Unmarshal(raw, &details); err != nil {
			rows.Close()
			return nil, err
		}
		name := details.Name
		if kind == "permission.checked" {
			name = details.Tool
		}
		if kind == "context.compacted" {
			if details.Scope != "turn" && details.Scope != "conversation" {
				continue
			}
			name = "context_compaction"
		}
		if name == "" {
			continue
		}
		item := get(name, details.PluginID, details.ReleaseID)
		if at.After(item.LastUsedAt) {
			item.LastUsedAt = at
		}
		switch kind {
		case "tool.started":
			item.Calls++
		case "tool.completed":
			item.Completed++
			item.DurationMillis += details.DurationMillis
			if !details.OK {
				item.Failures++
			}
		case "permission.checked":
			switch details.Outcome {
			case "allow":
				item.PermissionAllows++
			case "deny":
				item.PermissionDenials++
			case "ask":
				item.PermissionAsks++
			}
		case "context.compacted":
			item.ContextCompactions++
			item.OriginalContextChars += details.OriginalChars
			item.CompactedContextChars += details.CompactedChars
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	approvalRows, err := s.db.QueryContext(ctx, `SELECT tool_name,plugin_id,release_id,status,COUNT(*) FROM agent_approvals WHERE user_id=? AND created_at>=? GROUP BY tool_name,plugin_id,release_id,status`, userID, since)
	if err != nil {
		return nil, err
	}
	for approvalRows.Next() {
		var name, pluginID, releaseID, status string
		var count int64
		if err := approvalRows.Scan(&name, &pluginID, &releaseID, &status, &count); err != nil {
			approvalRows.Close()
			return nil, err
		}
		item := get(name, pluginID, releaseID)
		item.ApprovalRequests += count
		switch status {
		case "pending":
			item.ApprovalsPending += count
		case "approved":
			item.ApprovalsGranted += count
		case "denied":
			item.ApprovalsDenied += count
		case "expired":
			item.ApprovalsExpired += count
		case "cancelled":
			item.ApprovalsCancelled += count
		}
	}
	if err := approvalRows.Err(); err != nil {
		approvalRows.Close()
		return nil, err
	}
	if err := approvalRows.Close(); err != nil {
		return nil, err
	}
	type lifecycleEvent struct {
		projectID, pluginID, action string
		details                     []byte
		createdAt                   time.Time
	}
	lifecycleRows, err := s.db.QueryContext(ctx, `SELECT project_id,plugin_id,action,details_json,created_at FROM plugin_audit_events WHERE user_id=? AND created_at>=? ORDER BY created_at`, userID, since)
	if err != nil {
		return nil, err
	}
	lifecycle := []lifecycleEvent{}
	for lifecycleRows.Next() {
		var item lifecycleEvent
		if err := lifecycleRows.Scan(&item.projectID, &item.pluginID, &item.action, &item.details, &item.createdAt); err != nil {
			lifecycleRows.Close()
			return nil, err
		}
		lifecycle = append(lifecycle, item)
	}
	if err := lifecycleRows.Err(); err != nil {
		lifecycleRows.Close()
		return nil, err
	}
	if err := lifecycleRows.Close(); err != nil {
		return nil, err
	}
	for _, event := range lifecycle {
		var details struct {
			ReleaseID string `json:"releaseId"`
		}
		if err := json.Unmarshal(event.details, &details); err != nil {
			return nil, err
		}
		pluginID := event.pluginID
		if pluginID == "" && details.ReleaseID != "" {
			if err := s.db.QueryRowContext(ctx, `SELECT plugin_id FROM plugin_releases WHERE id=? AND project_id=?`, details.ReleaseID, event.projectID).Scan(&pluginID); err != nil && err != sql.ErrNoRows {
				return nil, err
			}
		}
		item := get("plugin.lifecycle", pluginID, details.ReleaseID, event.projectID)
		if event.createdAt.After(item.LastUsedAt) {
			item.LastUsedAt = event.createdAt
		}
		switch event.action {
		case "release.tested":
			item.PluginBuilds++
		case "release.build_failed":
			item.PluginBuildFailures++
		case "plugin.activated":
			item.PluginActivations++
		case "plugin.deactivated":
			item.PluginDeactivations++
		case "plugin.rolled_back":
			item.PluginRollbacks++
		case "permission.requested":
			item.PluginPermissionRequests++
		case "permission.approved":
			item.PluginPermissionsGranted++
		case "runtime.crashed":
			item.PluginRuntimeFailures++
		case "source.updated", "source.patch_applied":
			item.PluginSourceChanges++
		case "release.marked_unusable":
			item.PluginUnusableMarks++
		case "release.bundle_removed":
			item.PluginBundleCleanups++
		}
	}
	result := make([]domain.ToolUsageMetric, 0, len(metrics))
	for _, item := range metrics {
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Calls == result[j].Calls {
			if result[i].ToolName == result[j].ToolName {
				return result[i].ReleaseID < result[j].ReleaseID
			}
			return result[i].ToolName < result[j].ToolName
		}
		return result[i].Calls > result[j].Calls
	})
	return result, nil
}
