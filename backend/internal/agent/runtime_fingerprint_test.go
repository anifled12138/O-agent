package agent

import (
	"testing"

	"axiom.local/agent/internal/domain"
)

func TestRuntimeFingerprintBindsRepositoryIdentityButNotHostPath(t *testing.T) {
	provider := domain.Provider{ID: "provider_1", Kind: "openai", BaseURL: "https://api.example.test", Model: "model-x", ContextWindow: 32768}
	generation := domain.AgentGeneration{ID: "generation_1", DefinitionDigest: "definition-digest"}
	project := domain.Project{ID: "project_1", Name: "O", Workdir: `C:\agent\O`, RemoteRepoURL: "https://github.com/example/o.git", RemoteBranch: "main", ResolvedCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	first, err := turnRuntimeFingerprint(provider, generation, domain.PermissionProfileWorkspaceAutonomy, project, true, &turnScope{})
	if err != nil {
		t.Fatal(err)
	}
	project.Workdir = "/srv/o-agent/workspaces/project_1"
	second, err := turnRuntimeFingerprint(provider, generation, domain.PermissionProfileWorkspaceAutonomy, project, true, &turnScope{})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("the same pinned project on another host produced an incompatible runtime fingerprint")
	}
	project.ResolvedCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	third, err := turnRuntimeFingerprint(provider, generation, domain.PermissionProfileWorkspaceAutonomy, project, true, &turnScope{})
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("runtime fingerprint did not change when the pinned project commit changed")
	}
	project.ResolvedCommit = ""
	fourth, err := turnRuntimeFingerprint(provider, generation, domain.PermissionProfileWorkspaceAutonomy, project, true, &turnScope{})
	if err != nil {
		t.Fatal(err)
	}
	project.Workdir = `D:\other\O`
	fifth, err := turnRuntimeFingerprint(provider, generation, domain.PermissionProfileWorkspaceAutonomy, project, true, &turnScope{})
	if err != nil {
		t.Fatal(err)
	}
	if fourth == fifth {
		t.Fatal("a local project without a pinned commit was not bound to its host work directory")
	}
}
