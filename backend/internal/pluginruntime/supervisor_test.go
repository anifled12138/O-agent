package pluginruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginmanifest"
)

func TestWithinReleaseBoundary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins", "release")
	if !within(root, filepath.Join(root, "backend", "plugin.exe")) {
		t.Fatal("release artifact should be accepted")
	}
	if within(root, filepath.Join(root, "..", "other", "plugin.exe")) {
		t.Fatal("artifact path must not escape release root")
	}
}

func TestConcurrentFrontendActivationAndDeactivationRemainsConsistent(t *testing.T) {
	supervisor, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := pluginforge.Release{ID: "rel_race", PluginID: "example.race", Version: "1.0.0", Manifest: pluginmanifest.Manifest{SpecVersion: pluginmanifest.SpecV2, ID: "example.race", Name: "Race", Version: "1.0.0", Description: "Race UI", UI: &pluginmanifest.UI{Entry: "frontend/index.html", Sandbox: "strict"}}}
	var group sync.WaitGroup
	for index := 0; index < 32; index++ {
		group.Add(1)
		go func(deactivate bool) {
			defer group.Done()
			if deactivate {
				_ = supervisor.Deactivate(context.Background(), "user", release.PluginID)
			} else {
				_ = supervisor.Activate(context.Background(), "user", release)
			}
		}(index%2 == 0)
	}
	group.Wait()
	states := supervisor.SurfaceStates("user", release.PluginID)
	if len(states) != 0 && (len(states) != 1 || states[0].Kind != "ui") {
		t.Fatalf("partial registry state: %#v", states)
	}
}

func TestExportCollisionLeavesOriginalRegistryUntouched(t *testing.T) {
	supervisor, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := func(pluginID string) pluginmanifest.Manifest {
		return pluginmanifest.Manifest{SpecVersion: pluginmanifest.SpecV2, ID: pluginID, Name: pluginID, Version: "1.0.0", Description: "Skill", Exports: pluginmanifest.Exports{Skills: []pluginmanifest.SkillExport{{ID: "shared.guide", Summary: "Shared", Visibility: "discoverable", Entry: "skills/guide/SKILL.md"}}}}
	}
	first := pluginforge.Release{ID: "rel_first", PluginID: "example.first", Version: "1.0.0", Manifest: manifest("example.first")}
	second := pluginforge.Release{ID: "rel_second", PluginID: "example.second", Version: "1.0.0", Manifest: manifest("example.second")}
	if err := supervisor.Activate(context.Background(), "user", first); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Activate(context.Background(), "user", second); err == nil {
		t.Fatal("colliding export was activated")
	}
	items := supervisor.Skills("user")
	if len(items) != 1 || items[0].PluginID != first.PluginID {
		t.Fatalf("original registry was disturbed: %#v", items)
	}
}

func TestTrustedHostExecutorNeedsNoSidecar(t *testing.T) {
	supervisor, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := pluginforge.Release{ID: "rel_host", PluginID: "example.host", Version: "1.0.0", Manifest: pluginmanifest.Manifest{
		SpecVersion: pluginmanifest.SpecV2, ID: "example.host", Name: "Host Tool", Version: "1.0.0", Description: "Trusted Host tool",
		Exports: pluginmanifest.Exports{Tools: []pluginmanifest.ToolExport{{ID: "example.host.describe", Summary: "Describe mounted workspace", Visibility: "discoverable", Risk: "read-only", Executor: pluginmanifest.Executor{Kind: "host", Target: "host.workspace.describe"}, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`), OutputSchema: json.RawMessage(`{"type":"object","required":["mounted","resourceAccess"],"properties":{"mounted":{"type":"boolean"},"resourceAccess":{"type":"string"}}}`)}}},
	}}
	if err := supervisor.Activate(context.Background(), "user", release); err != nil {
		t.Fatal(err)
	}
	result, err := supervisor.Invoke(context.Background(), "user", "example.host.describe", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(result), `"mounted":true`) {
		t.Fatalf("Host tool failed: %s, %v", result, err)
	}
	if states := supervisor.SurfaceStates("user", release.PluginID); len(states) != 1 || states[0].Kind != "tool" {
		t.Fatalf("Host tool mounted unexpected surfaces: %#v", states)
	}
}

func TestActivationRejectsMissingDependenciesWithoutPublishingSurfaces(t *testing.T) {
	supervisor, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := pluginforge.Release{ID: "rel_dependent", PluginID: "example.dependent", Version: "1.0.0", Manifest: pluginmanifest.Manifest{
		SpecVersion: pluginmanifest.SpecV2, ID: "example.dependent", Name: "Dependent", Version: "1.0.0", Description: "Requires service",
		UI:           &pluginmanifest.UI{Entry: "frontend/index.html", Sandbox: "strict"},
		Dependencies: pluginmanifest.Dependencies{Services: []pluginmanifest.ServiceDependency{{ID: "missing.service", Contract: "example/v1"}}},
	}}
	if err := supervisor.Activate(context.Background(), "user", release); err == nil {
		t.Fatal("missing dependency was accepted")
	}
	if len(supervisor.SurfaceStates("user", release.PluginID)) != 0 {
		t.Fatal("failed preparation published partial surfaces")
	}
}

func TestUIServiceCallRequiresDeclaredContractAndCarriesUIPrincipal(t *testing.T) {
	supervisor, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pluginInput, hostInput := io.Pipe()
	pluginOutput, hostOutput := io.Pipe()
	providerProcess := &process{stdin: hostInput, stdout: bufio.NewReader(pluginOutput), done: make(chan struct{})}
	go func() {
		defer pluginInput.Close()
		defer hostOutput.Close()
		line, err := bufio.NewReader(pluginInput).ReadBytes('\n')
		if err != nil {
			return
		}
		var request rpcRequest
		if json.Unmarshal(line, &request) != nil || request.Method != "service.call" {
			return
		}
		var params map[string]any
		if json.Unmarshal(request.Params, &params) != nil || params["principal"] != "ui" || params["callerPluginId"] != "example.ui" {
			return
		}
		_ = json.NewEncoder(hostOutput).Encode(rpcResponse{ID: request.ID, Result: json.RawMessage(`{"ready":true}`)})
	}()

	service := pluginmanifest.ServiceExport{ID: "example.index", Contract: "example.index/v1", Handler: "service.call"}
	supervisor.services[key("user", service.ID)] = &serviceBinding{
		binding: pluginforge.ServiceBinding{PluginID: "example.service", ReleaseID: "rel_service", Version: "1.0.0", Service: service},
		process: providerProcess,
	}
	callerRelease := pluginforge.Release{ID: "rel_ui", PluginID: "example.ui", Version: "1.0.0", Manifest: pluginmanifest.Manifest{
		SpecVersion: pluginmanifest.SpecV2, ID: "example.ui", Name: "UI", Version: "1.0.0", Description: "UI client",
		UI:           &pluginmanifest.UI{Entry: "frontend/index.html", Sandbox: "strict"},
		Dependencies: pluginmanifest.Dependencies{Services: []pluginmanifest.ServiceDependency{{ID: service.ID, Contract: service.Contract}}},
	}}
	supervisor.plugins[key("user", callerRelease.PluginID)] = &mountedRelease{release: callerRelease}
	supervisor.uis[key("user", callerRelease.PluginID)] = pluginforge.UIBinding{PluginID: callerRelease.PluginID, ReleaseID: callerRelease.ID, UI: *callerRelease.Manifest.UI}

	result, err := supervisor.UIServiceCall(context.Background(), "user", callerRelease.PluginID, service.ID, json.RawMessage(`{"query":"status"}`))
	if err != nil || !strings.Contains(string(result), `"ready":true`) {
		t.Fatalf("declared UI service call failed: %s, %v", result, err)
	}
	if _, err = supervisor.UIServiceCall(context.Background(), "user", "undeclared.ui", service.ID, json.RawMessage(`{}`)); err == nil {
		t.Fatal("undeclared UI service access was accepted")
	}
}

func TestVersionConstraints(t *testing.T) {
	for _, test := range []struct {
		current, constraint string
		want                bool
	}{{"1.4.2", "^1.2.0", true}, {"2.0.0", "^1.2.0", false}, {"0.2.5", "^0.2.0", true}, {"0.3.0", "^0.2.0", false}, {"0.0.4", "^0.0.4", true}, {"0.0.5", "^0.0.4", false}, {"1.4.2", "~1.4.0", true}, {"1.5.0", "~1.4.0", false}, {"1.4.2", ">=1.4.1", true}, {"1.4.2", "1.4.2", true}} {
		if got := versionSatisfies(test.current, test.constraint); got != test.want {
			t.Fatalf("versionSatisfies(%s,%s)=%v", test.current, test.constraint, got)
		}
	}
}

func TestPureUISurfaceActivatesWithoutBackend(t *testing.T) {
	workspace := t.TempDir()
	supervisor, err := New(workspace)
	if err != nil {
		t.Fatal(err)
	}
	release := pluginforge.Release{ID: "rel_ui", PluginID: "example.ui", Version: "1.0.0", BundleDir: filepath.Join(workspace, "work", "ui"), Manifest: pluginmanifest.Manifest{
		SpecVersion: pluginmanifest.SpecV2, ID: "example.ui", Name: "UI", Version: "1.0.0", Description: "Pure UI",
		UI: &pluginmanifest.UI{Entry: "frontend/index.html", Assets: "frontend/**", Slots: []string{"workspace.main"}, Sandbox: "strict"},
	}}
	if err := supervisor.Activate(context.Background(), "user", release); err != nil {
		t.Fatal(err)
	}
	if _, ok := supervisor.UI("user", "example.ui"); !ok {
		t.Fatal("UI registry did not publish the active surface")
	}
	states := supervisor.SurfaceStates("user", "example.ui")
	if len(states) != 1 || states[0].Kind != "ui" {
		t.Fatalf("unexpected surface states: %#v", states)
	}
	if err := supervisor.Deactivate(context.Background(), "user", "example.ui"); err != nil {
		t.Fatal(err)
	}
	if _, ok := supervisor.UI("user", "example.ui"); ok {
		t.Fatal("UI surface remained registered after deactivation")
	}
}

func TestSkillSurfaceIsRegisteredWithoutEnteringToolCatalog(t *testing.T) {
	workspace := t.TempDir()
	supervisor, err := New(workspace)
	if err != nil {
		t.Fatal(err)
	}
	release := pluginforge.Release{ID: "rel_skill", PluginID: "example.skill", Version: "1.0.0", BundleDir: filepath.Join(workspace, "work", "skill"), Manifest: pluginmanifest.Manifest{
		SpecVersion: pluginmanifest.SpecV2, ID: "example.skill", Name: "Skill", Version: "1.0.0", Description: "Lazy skill",
		Exports: pluginmanifest.Exports{Skills: []pluginmanifest.SkillExport{{ID: "example.skill.guide", Summary: "Guide work", Visibility: "discoverable", Entry: "skills/guide/SKILL.md"}}},
	}}
	if err := supervisor.Activate(context.Background(), "user", release); err != nil {
		t.Fatal(err)
	}
	if len(supervisor.Skills("user")) != 1 || len(supervisor.Capabilities("user")) != 0 {
		t.Fatal("skill must stay in the lazy skill registry, outside the tool catalog")
	}
	lease := supervisor.BeginTurn("user")
	if err := supervisor.Deactivate(context.Background(), "user", "example.skill"); err != nil {
		t.Fatal(err)
	}
	if len(lease.Skills()) != 1 {
		t.Fatal("turn snapshot lost its pinned skill after hot deactivation")
	}
	lease.Close()
}

func TestShutdownDurationIsBounded(t *testing.T) {
	if got := shutdownDuration(20); got != 10*time.Second {
		t.Fatalf("unsafe short timeout was not normalized: %s", got)
	}
	if got := shutdownDuration(5000); got != 5*time.Second {
		t.Fatalf("valid timeout changed: %s", got)
	}
}
