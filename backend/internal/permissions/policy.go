// Package permissions applies one session policy to every tool call. The
// caller classifies effects where the host can verify them; an enabled tool is
// otherwise available without a per-call approval in workspace-autonomous
// mode, unless the effect is classified as risky.
package permissions

import "axiom.local/agent/internal/domain"

type Outcome string

const (
	OutcomeAllow Outcome = "allow"
	OutcomeAsk   Outcome = "ask"
	OutcomeDeny  Outcome = "deny"
)

type Effect string

const (
	EffectRead           Effect = "read"
	EffectEphemeral      Effect = "ephemeral"
	EffectWorkspaceWrite Effect = "workspace_write"
	EffectShell          Effect = "shell"
	EffectExternalRead   Effect = "external_read"
	EffectExternalWrite  Effect = "external_write"
	EffectDestructive    Effect = "destructive"
	EffectSensitive      Effect = "sensitive"
	EffectUnknown        Effect = "unknown"
)

type Request struct {
	ToolName  string                   `json:"toolName"`
	Source    string                   `json:"source"`
	PluginID  string                   `json:"pluginId,omitempty"`
	ReleaseID string                   `json:"releaseId,omitempty"`
	Effect    Effect                   `json:"effect"`
	Resource  string                   `json:"resource,omitempty"`
	Reason    string                   `json:"reason,omitempty"`
	Impact    string                   `json:"impact,omitempty"`
	Profile   domain.PermissionProfile `json:"profile"`
}

type Decision struct {
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason"`
}

// Evaluate applies the session's permission policy independently of whether a
// tool is built in or provided by an enabled extension. Unknown effects are
// not an implicit approval prompt: workspace-autonomous mode only asks for
// high-impact actions the host has positively classified as destructive or
// sensitive. Request-approval mode additionally asks before workspace writes
// and external access. The process sandbox remains an independent boundary.
func Evaluate(profile domain.PermissionProfile, request Request) Decision {
	request.Profile = profile
	if !profile.Valid() {
		return Decision{Outcome: OutcomeDeny, Reason: "会话权限等级无效，已阻止工具调用"}
	}

	if profile == domain.PermissionProfileFullyAutonomous {
		return Decision{Outcome: OutcomeAllow, Reason: "完全自动模式不进行逐项权限拦截"}
	}

	if request.Effect == EffectRead || request.Effect == EffectEphemeral {
		return Decision{Outcome: OutcomeAllow, Reason: "只读或临时操作允许执行"}
	}

	if profile == domain.PermissionProfileReadOnly {
		return Decision{Outcome: OutcomeDeny, Reason: "只读模式不允许写入或产生外部影响"}
	}

	if request.Effect == EffectDestructive {
		return Decision{Outcome: OutcomeAsk, Reason: "操作会删除或覆盖已有数据"}
	}
	if request.Effect == EffectSensitive {
		return Decision{Outcome: OutcomeAsk, Reason: "操作会执行或启用新的插件代码"}
	}
	if profile == domain.PermissionProfileRequestApproval {
		switch request.Effect {
		case EffectWorkspaceWrite, EffectShell, EffectExternalRead, EffectExternalWrite, EffectUnknown:
			return Decision{Outcome: OutcomeAsk, Reason: "此模式要求在工作区写入或访问网络前获得批准"}
		}
	}

	// workspace_autonomous is the default: normal workspace writes, command
	// execution and network access run without per-call prompts. Only effects
	// positively classified as destructive or sensitive require approval.
	return Decision{Outcome: OutcomeAllow, Reason: "工作区自动模式允许常规操作"}
}
