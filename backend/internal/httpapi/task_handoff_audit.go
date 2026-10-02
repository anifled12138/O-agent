package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"axiom.local/agent/internal/domain"
)

type taskHandoffExternalEffect struct {
	ToolCallID string `json:"toolCallId"`
	Tool       string `json:"tool"`
	Effect     string `json:"effect"`
	State      string `json:"state"`
}

type taskHandoffEffectResolution struct {
	ToolCallID string `json:"toolCallId"`
	Outcome    string `json:"outcome"`
}

type handoffEffectTrace struct {
	effect    string
	tool      string
	started   bool
	completed bool
	ok        bool
}

// summarizeTaskHandoffEffects derives a safe metadata-only audit from the
// durable trace. Arguments and tool output are deliberately excluded because
// they can contain secrets. Unknown or malformed effects fail closed.
func summarizeTaskHandoffEffects(trace []domain.TraceEvent, conversationID, turnID string) ([]taskHandoffExternalEffect, error) {
	byCall := map[string]*handoffEffectTrace{}
	order := []string{}
	for _, event := range trace {
		if event.ConversationID != conversationID {
			return nil, fmt.Errorf("handoff trace event escaped its task turn: %w", domain.ErrConflict)
		}
		// A transcript contains the entire conversation trace. Earlier turns
		// are valid history and must not block handoff or be replayed as effects
		// of the turn currently being transferred.
		if event.TurnID != turnID {
			continue
		}
		if event.Kind != "permission.checked" && event.Kind != "tool.started" && event.Kind != "tool.completed" {
			continue
		}
		var details struct {
			ToolCallID string `json:"toolCallId"`
			Tool       string `json:"tool"`
			Name       string `json:"name"`
			Effect     string `json:"effect"`
			Outcome    string `json:"outcome"`
			OK         *bool  `json:"ok"`
		}
		if len(event.Details) == 0 || !json.Valid(event.Details) || json.Unmarshal(event.Details, &details) != nil || strings.TrimSpace(details.ToolCallID) == "" || len(details.ToolCallID) > 256 || strings.IndexFunc(details.ToolCallID, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("handoff tool trace is malformed: %w", domain.ErrConflict)
		}
		if details.Effect != "" && len(details.Effect) > 64 {
			return nil, fmt.Errorf("handoff effect classification is invalid: %w", domain.ErrConflict)
		}
		tool := details.Tool
		if tool == "" {
			tool = details.Name
		}
		if tool != "" && (len(tool) > 256 || strings.TrimSpace(tool) != tool || strings.IndexFunc(tool, unicode.IsControl) >= 0) {
			return nil, fmt.Errorf("handoff tool trace name is invalid: %w", domain.ErrConflict)
		}
		entry := byCall[details.ToolCallID]
		if entry == nil {
			entry = &handoffEffectTrace{}
			byCall[details.ToolCallID] = entry
		}
		if details.Effect != "" {
			switch details.Effect {
			case "read", "ephemeral", "workspace_write", "shell", "external_read", "external_write", "destructive", "sensitive", "unknown":
			default:
				details.Effect = "unknown"
			}
			if entry.effect != "" && entry.effect != details.Effect {
				return nil, fmt.Errorf("handoff effect classification changed within a tool call: %w", domain.ErrConflict)
			}
			entry.effect = details.Effect
		}
		if tool != "" {
			if entry.tool != "" && entry.tool != tool {
				return nil, fmt.Errorf("handoff tool identity changed within a tool call: %w", domain.ErrConflict)
			}
			entry.tool = tool
		}
		switch event.Kind {
		case "permission.checked":
			if details.Outcome == "deny" || details.Outcome == "ask" {
				continue
			}
		case "tool.started":
			if entry.started {
				return nil, fmt.Errorf("handoff trace repeats a tool start: %w", domain.ErrConflict)
			}
			entry.started = true
			order = append(order, details.ToolCallID)
		case "tool.completed":
			if !entry.started || entry.completed || details.OK == nil {
				return nil, fmt.Errorf("handoff trace has an unmatched or incomplete tool result: %w", domain.ErrConflict)
			}
			entry.completed = true
			entry.ok = *details.OK
		}
	}
	result := make([]taskHandoffExternalEffect, 0)
	for _, callID := range order {
		entry := byCall[callID]
		if entry.effect == "read" || entry.effect == "ephemeral" || entry.effect == "external_read" {
			continue
		}
		effect := entry.effect
		if effect == "" {
			effect = "unknown"
		}
		tool := entry.tool
		if tool == "" {
			tool = "unknown"
		}
		state := "result_unknown"
		if entry.completed && entry.ok {
			state = "completed"
		} else if entry.completed {
			state = "reported_error"
		}
		result = append(result, taskHandoffExternalEffect{ToolCallID: callID, Tool: tool, Effect: effect, State: state})
	}
	return result, nil
}

// validateTaskHandoffEffectResolutions binds a user's per-effect review to
// the exact effects derived from the durable trace. A global acknowledgement
// cannot stand in for resolving individual possibly-mutating calls.
func validateTaskHandoffEffectResolutions(effects []taskHandoffExternalEffect, resolutions []taskHandoffEffectResolution) ([]taskHandoffEffectResolution, error) {
	if len(effects) == 0 {
		if len(resolutions) != 0 {
			return nil, fmt.Errorf("handoff supplied resolutions for effects absent from the durable trace: %w", domain.ErrConflict)
		}
		return []taskHandoffEffectResolution{}, nil
	}
	if len(resolutions) != len(effects) {
		return nil, fmt.Errorf("every external effect must have an explicit handoff resolution: %w", domain.ErrInvalid)
	}
	expected := make(map[string]struct{}, len(effects))
	for _, effect := range effects {
		if effect.ToolCallID == "" {
			return nil, fmt.Errorf("handoff trace has an empty external effect identity: %w", domain.ErrConflict)
		}
		expected[effect.ToolCallID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(resolutions))
	validated := make([]taskHandoffEffectResolution, 0, len(resolutions))
	for _, resolution := range resolutions {
		if _, ok := expected[resolution.ToolCallID]; !ok {
			return nil, fmt.Errorf("handoff resolution does not match a durable external effect: %w", domain.ErrConflict)
		}
		if _, duplicate := seen[resolution.ToolCallID]; duplicate {
			return nil, fmt.Errorf("handoff repeats an external effect resolution: %w", domain.ErrInvalid)
		}
		seen[resolution.ToolCallID] = struct{}{}
		switch resolution.Outcome {
		case "confirmed_applied", "confirmed_not_applied", "unknown":
		default:
			return nil, fmt.Errorf("handoff effect outcome is invalid: %w", domain.ErrInvalid)
		}
		validated = append(validated, resolution)
	}
	for id := range expected {
		if _, ok := seen[id]; !ok {
			return nil, fmt.Errorf("handoff omitted an external effect resolution: %w", domain.ErrInvalid)
		}
	}
	return validated, nil
}
