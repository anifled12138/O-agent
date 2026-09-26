// Package permissions evaluates the host's session permission profile against
// trusted operation metadata. Plugin and MCP declarations are inputs to the
// host's classification, never permission grants by themselves.
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

// Evaluate intentionally fails closed for invalid profiles and unknown
// effects. Only a narrowly defined read or workspace write can run without
// prompting under the corresponding profile.
func Evaluate(profile domain.PermissionProfile, request Request) Decision {
	request.Profile = profile
	if !profile.Valid() {
		return Decision{Outcome: OutcomeDeny, Reason: "会话权限等级无效，已阻止工具调用"}
	}

	if request.Effect == EffectRead {
		return Decision{Outcome: OutcomeAllow, Reason: "只读操作符合当前会话权限"}
	}

	if profile == domain.PermissionProfileReadOnly {
		return Decision{Outcome: OutcomeDeny, Reason: "当前会话为只读权限；请切换会话权限后重试"}
	}

	if profile == domain.PermissionProfileWorkspaceAutonomy && request.Effect == EffectWorkspaceWrite {
		return Decision{Outcome: OutcomeAllow, Reason: "工作区内写入符合当前会话权限"}
	}

	if request.Effect == EffectShell {
		return Decision{Outcome: OutcomeAsk, Reason: "Shell 命令的实际影响无法仅凭工作目录限制，需要用户确认"}
	}
	if request.Effect == EffectExternalRead || request.Effect == EffectExternalWrite {
		return Decision{Outcome: OutcomeAsk, Reason: "该工具会访问外部服务，需要用户确认"}
	}
	if request.Effect == EffectDestructive {
		return Decision{Outcome: OutcomeAsk, Reason: "该工具可能删除或覆盖数据，需要用户确认"}
	}
	if request.Effect == EffectSensitive {
		return Decision{Outcome: OutcomeAsk, Reason: "该操作会改变 Agent 或插件的持久状态，需要用户确认"}
	}
	if request.Effect == EffectUnknown {
		return Decision{Outcome: OutcomeAsk, Reason: "工具没有可信的宿主风险分类，默认需要用户确认"}
	}

	if profile == domain.PermissionProfileAskOnSensitive {
		return Decision{Outcome: OutcomeAsk, Reason: "当前会话要求在任何写入前询问"}
	}
	return Decision{Outcome: OutcomeDeny, Reason: "该操作不属于当前会话允许的工作区操作"}
}
