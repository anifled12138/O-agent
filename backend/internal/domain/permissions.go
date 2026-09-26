package domain

// PermissionProfile selects a named, user-controlled policy for one
// conversation. The effective rules are enforced by the Agent runtime; this
// value is persisted so each turn can record the policy it was created under.
type PermissionProfile string

const (
	PermissionProfileReadOnly          PermissionProfile = "read_only"
	PermissionProfileWorkspaceAutonomy PermissionProfile = "workspace_autonomous"
	PermissionProfileAskOnSensitive    PermissionProfile = "ask_on_sensitive"
)

func (p PermissionProfile) Valid() bool {
	switch p {
	case PermissionProfileReadOnly, PermissionProfileWorkspaceAutonomy, PermissionProfileAskOnSensitive:
		return true
	default:
		return false
	}
}

func DefaultPermissionProfile() PermissionProfile {
	return PermissionProfileWorkspaceAutonomy
}
