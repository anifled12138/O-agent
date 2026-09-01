package pluginmanifest

import (
	"encoding/json"
	"testing"
)

var objectSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

func TestManifestShapeMatrix(t *testing.T) {
	tests := []struct {
		name     string
		manifest Manifest
	}{
		{name: "pure UI", manifest: baseManifest("example.ui", UIOnly())},
		{name: "pure backend", manifest: baseManifest("example.backend", BackendOnly())},
		{name: "skill only", manifest: baseManifest("example.skills", SkillOnly())},
		{name: "host tool only", manifest: baseManifest("example.host-tool", HostToolOnly())},
		{name: "full stack", manifest: fullStackManifest()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.manifest.Validate(); err != nil {
				t.Fatalf("valid manifest rejected: %v", err)
			}
		})
	}
}

func TestManifestRejectsMissingAndCrossSurfaceContracts(t *testing.T) {
	empty := baseManifest("example.empty")
	if empty.Validate() == nil {
		t.Fatal("empty plugin should be rejected")
	}
	toolWithoutBackend := baseManifest("example.invalid")
	toolWithoutBackend.Exports.Tools = []ToolExport{{
		ID: "example.invalid.run", Summary: "Run", Visibility: "discoverable", Risk: "read-only",
		Executor: Executor{Kind: "backend", Target: "run"}, InputSchema: objectSchema, OutputSchema: objectSchema,
	}}
	if toolWithoutBackend.Validate() == nil {
		t.Fatal("backend executor without backend should be rejected")
	}
	jobWithoutPermission := baseManifest("example.job", BackendOnly())
	jobWithoutPermission.Exports.Jobs = []JobExport{{ID: "example.job.run", Handler: Executor{Kind: "backend", Target: "run"}}}
	if jobWithoutPermission.Validate() == nil {
		t.Fatal("background job without permission should be rejected")
	}
}

func TestDecodeV1ProjectsIntoV2WithoutRewriting(t *testing.T) {
	legacy := LegacyManifest{
		ID: "workspace.legacy", Name: "Legacy", Version: "0.1.20260901", APIVersion: LegacySpecV1, Description: "Legacy full-stack plugin",
		Backend:  LegacyBackend{Artifact: "backend/plugin.exe", Protocol: "axiom.rpc/v1", ShutdownMillis: 9000},
		Frontend: LegacyFrontend{Entry: "frontend/index.html", Slots: []string{"workspace.main"}},
		Capabilities: []LegacyCapability{{
			ID: "workspace.legacy.scan", Summary: "Scan", Risk: "workspace-read", InputSchema: objectSchema, OutputSchema: objectSchema,
		}},
		Permissions: LegacyPermissions{WorkspaceRead: true, PluginDataWrite: true},
		Upgrade:     LegacyUpgrade{Strategy: "drain", PinActiveRuns: true, StateVersion: 1},
	}
	raw, _ := json.Marshal(legacy)
	document, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode legacy manifest: %v", err)
	}
	if document.SourceVersion != SourceV1 || document.Legacy == nil {
		t.Fatalf("legacy source identity lost: %#v", document)
	}
	manifest := document.Manifest
	if manifest.SpecVersion != SpecV2 || manifest.Runtime == nil || manifest.UI == nil || len(manifest.Exports.Tools) != 1 {
		t.Fatalf("legacy surfaces were not projected: %#v", manifest)
	}
	if manifest.Exports.Tools[0].Visibility != "discoverable" || manifest.Exports.Tools[0].Executor.Kind != "backend" {
		t.Fatalf("legacy tool projection is incorrect: %#v", manifest.Exports.Tools[0])
	}
	if got := manifest.Permissions.Filesystem.Read; len(got) != 1 || got[0] != "${workspace}" {
		t.Fatalf("legacy workspace permission lost: %v", got)
	}
	var unchanged LegacyManifest
	if err := json.Unmarshal(raw, &unchanged); err != nil || unchanged.APIVersion != LegacySpecV1 {
		t.Fatal("legacy release bytes were mutated")
	}
}

func TestDecodeV2IsStrictAndNormalizesDefaults(t *testing.T) {
	manifest := baseManifest("example.ui", UIOnly())
	manifest.UI.Assets = ""
	manifest.UI.Sandbox = ""
	raw, _ := json.Marshal(manifest)
	document, err := Decode(raw)
	if err != nil {
		t.Fatalf("decode v2 manifest: %v", err)
	}
	if document.SourceVersion != SourceV2 || document.Manifest.UI.Assets != "frontend/**" || document.Manifest.UI.Sandbox != "strict" {
		t.Fatalf("v2 defaults not normalized: %#v", document.Manifest.UI)
	}
	bad := append(raw[:len(raw)-1], []byte(`,"unexpected":true}`)...)
	if _, err := Decode(bad); err == nil {
		t.Fatal("unknown manifest fields should be rejected")
	}
}

func TestGrantDigestIsCanonicalAndSurfaceBound(t *testing.T) {
	a := fullStackManifest()
	a.Permissions.Network = []string{"b.example", "a.example", "a.example"}
	a.Permissions.Secrets = []string{"secret.b", "secret.a"}
	b := a
	b.Permissions.Network = []string{"a.example", "b.example"}
	b.Permissions.Secrets = []string{"secret.a", "secret.b"}
	if GrantDigest(a) != GrantDigest(b) {
		t.Fatal("grant digest should ignore set declaration order and duplicates")
	}
	uiCopy := *a.UI
	uiCopy.Slots = append(append([]string(nil), a.UI.Slots...), "workspace.sidebar")
	b.UI = &uiCopy
	if GrantDigest(a) == GrantDigest(b) {
		t.Fatal("adding a UI surface must change the grant digest")
	}
	c := a
	c.Exports.Tools = append([]ToolExport(nil), a.Exports.Tools...)
	c.Exports.Tools[0].Summary = "Changed prose only"
	if SurfaceDigest(a) != SurfaceDigest(c) {
		t.Fatal("surface identity digest should not depend on descriptive prose")
	}
	c.Exports.Tools[0].Visibility = "always"
	if GrantDigest(a) == GrantDigest(c) {
		t.Fatal("expanding model visibility must change the grant digest")
	}
}

func baseManifest(id string, options ...func(*Manifest)) Manifest {
	manifest := Manifest{SpecVersion: SpecV2, ID: id, Name: "Example", Version: "1.0.0", Description: "Example plugin", Upgrade: Upgrade{Strategy: "drain", StateVersion: 1}}
	for _, option := range options {
		option(&manifest)
	}
	return manifest
}

func UIOnly() func(*Manifest) {
	return func(manifest *Manifest) {
		manifest.UI = &UI{Entry: "frontend/index.html", Assets: "frontend/**", Slots: []string{"workspace.main"}, Sandbox: "strict"}
	}
}

func BackendOnly() func(*Manifest) {
	return func(manifest *Manifest) {
		manifest.Runtime = &Runtime{Backend: &Backend{Artifact: "backend/plugin.exe", Protocol: "axiom.rpc/v2", ShutdownMillis: 10000}}
	}
}

func SkillOnly() func(*Manifest) {
	return func(manifest *Manifest) {
		manifest.Exports.Skills = []SkillExport{{ID: "example.skills.review", Summary: "Review a workspace", Visibility: "discoverable", Entry: "skills/review/SKILL.md"}}
	}
}

func HostToolOnly() func(*Manifest) {
	return func(manifest *Manifest) {
		manifest.Exports.Tools = []ToolExport{{
			ID: "example.host-tool.run", Summary: "Run a trusted Host operation", Visibility: "creator-only", Risk: "host-controlled",
			Executor: Executor{Kind: "host", Target: "host.example.run"}, InputSchema: objectSchema, OutputSchema: objectSchema,
		}}
	}
}

func fullStackManifest() Manifest {
	manifest := baseManifest("workspace.inspector", UIOnly(), BackendOnly())
	manifest.Exports.Services = []ServiceExport{{ID: "workspace.inspector.query", Contract: "axiom.service/v1", Handler: "query"}}
	manifest.Exports.Tools = []ToolExport{{
		ID: "workspace.inspector.scan", Summary: "Scan workspace", Tags: []string{"workspace", "scan"}, Visibility: "discoverable", Risk: "workspace-read",
		Executor: Executor{Kind: "service", Target: "workspace.inspector.query"}, InputSchema: objectSchema, OutputSchema: objectSchema,
	}}
	manifest.Permissions.Filesystem.Read = []string{"${workspace}"}
	return manifest
}
