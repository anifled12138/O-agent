package permissions

import (
	"testing"

	"axiom.local/agent/internal/domain"
)

func TestExpandedShellCapabilitiesRequireApprovalOrAreDenied(t *testing.T) {
	request := Request{ToolName: "exec_command", Effect: EffectShell}

	decision := Evaluate(domain.PermissionProfileWorkspaceAutonomy, request)
	if decision.Outcome != OutcomeAllow {
		t.Fatalf("workspace-autonomous shell capability request should run automatically, got %#v", decision)
	}

	decision = Evaluate(domain.PermissionProfileRequestApproval, request)
	if decision.Outcome != OutcomeAsk {
		t.Fatalf("request-approval shell capability request must ask, got %#v", decision)
	}

	decision = Evaluate(domain.PermissionProfileReadOnly, request)
	if decision.Outcome != OutcomeDeny {
		t.Fatalf("read-only shell capability request must be denied, got %#v", decision)
	}

	decision = Evaluate(domain.PermissionProfileFullyAutonomous, request)
	if decision.Outcome != OutcomeAllow {
		t.Fatalf("fully-autonomous shell capability request should follow the selected profile, got %#v", decision)
	}
}

func TestPermissionProfilesApplyFourDistinctPolicies(t *testing.T) {
	for _, effect := range []Effect{EffectWorkspaceWrite, EffectExternalRead, EffectExternalWrite, EffectUnknown} {
		request := Request{ToolName: "operation", Effect: effect}
		if decision := Evaluate(domain.PermissionProfileWorkspaceAutonomy, request); decision.Outcome != OutcomeAllow {
			t.Errorf("workspace-auto should allow %s, got %#v", effect, decision)
		}
		if decision := Evaluate(domain.PermissionProfileRequestApproval, request); decision.Outcome != OutcomeAsk {
			t.Errorf("request-approval should ask for %s, got %#v", effect, decision)
		}
		if decision := Evaluate(domain.PermissionProfileReadOnly, request); decision.Outcome != OutcomeDeny {
			t.Errorf("read-only should deny %s, got %#v", effect, decision)
		}
		if decision := Evaluate(domain.PermissionProfileFullyAutonomous, request); decision.Outcome != OutcomeAllow {
			t.Errorf("fully-auto should allow %s, got %#v", effect, decision)
		}
	}

	for _, effect := range []Effect{EffectDestructive, EffectSensitive} {
		request := Request{ToolName: "risky_operation", Effect: effect}
		for _, profile := range []domain.PermissionProfile{domain.PermissionProfileWorkspaceAutonomy, domain.PermissionProfileRequestApproval} {
			if decision := Evaluate(profile, request); decision.Outcome != OutcomeAsk {
				t.Errorf("profile %s should ask for risky effect %s, got %#v", profile, effect, decision)
			}
		}
	}
}
