package pluginmanifest

import "encoding/json"

const (
	SpecV2       = "axiom.plugin/v2"
	LegacySpecV1 = "axiom.plugin/v1"
)

type Manifest struct {
	SpecVersion  string       `json:"specVersion"`
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Version      string       `json:"version"`
	Description  string       `json:"description"`
	Runtime      *Runtime     `json:"runtime,omitempty"`
	UI           *UI          `json:"ui,omitempty"`
	Exports      Exports      `json:"exports,omitempty"`
	Dependencies Dependencies `json:"dependencies,omitempty"`
	Permissions  Permissions  `json:"permissions,omitempty"`
	Upgrade      Upgrade      `json:"upgrade,omitempty"`
}

type Runtime struct {
	Backend *Backend `json:"backend,omitempty"`
}

type Backend struct {
	Artifact       string `json:"artifact"`
	Protocol       string `json:"protocol"`
	ShutdownMillis int    `json:"shutdownMillis,omitempty"`
}

type UI struct {
	Entry   string   `json:"entry"`
	Assets  string   `json:"assets,omitempty"`
	Slots   []string `json:"slots,omitempty"`
	Sandbox string   `json:"sandbox,omitempty"`
}

type Exports struct {
	Services []ServiceExport `json:"services,omitempty"`
	Tools    []ToolExport    `json:"tools,omitempty"`
	Skills   []SkillExport   `json:"skills,omitempty"`
	Hooks    []HookExport    `json:"hooks,omitempty"`
	Jobs     []JobExport     `json:"jobs,omitempty"`
}

type ServiceExport struct {
	ID       string `json:"id"`
	Contract string `json:"contract"`
	Handler  string `json:"handler,omitempty"`
}

type ToolExport struct {
	ID           string          `json:"id"`
	Summary      string          `json:"summary"`
	Tags         []string        `json:"tags,omitempty"`
	Visibility   string          `json:"visibility"`
	Risk         string          `json:"risk"`
	Executor     Executor        `json:"executor"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

type SkillExport struct {
	ID         string `json:"id"`
	Summary    string `json:"summary"`
	Visibility string `json:"visibility"`
	Entry      string `json:"entry"`
}

type HookExport struct {
	ID      string   `json:"id"`
	Events  []string `json:"events"`
	Handler string   `json:"handler"`
}

type JobExport struct {
	ID      string   `json:"id"`
	Handler Executor `json:"handler"`
}

type Executor struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

type Dependencies struct {
	Plugins  []PluginDependency  `json:"plugins,omitempty"`
	Services []ServiceDependency `json:"services,omitempty"`
}

type PluginDependency struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type ServiceDependency struct {
	ID       string `json:"id"`
	Contract string `json:"contract"`
}

type Permissions struct {
	Filesystem FilesystemPermissions `json:"filesystem,omitempty"`
	Network    []string              `json:"network,omitempty"`
	Secrets    []string              `json:"secrets,omitempty"`
	Process    bool                  `json:"process,omitempty"`
	Background bool                  `json:"background,omitempty"`
}

type FilesystemPermissions struct {
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
}

type Upgrade struct {
	Strategy       string `json:"strategy,omitempty"`
	PinActiveCalls bool   `json:"pinActiveCalls,omitempty"`
	StateVersion   int    `json:"stateVersion,omitempty"`
}

type SourceVersion string

const (
	SourceV1 SourceVersion = "v1"
	SourceV2 SourceVersion = "v2"
)

type Document struct {
	Manifest      Manifest
	SourceVersion SourceVersion
	Legacy        *LegacyManifest
}

// LegacyManifest is the immutable wire shape emitted by axiom.plugin/v1.
// Runtime code may keep using this value while package metadata is projected
// into Manifest V2.
type LegacyManifest struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Version      string             `json:"version"`
	APIVersion   string             `json:"apiVersion"`
	Description  string             `json:"description"`
	Backend      LegacyBackend      `json:"backend"`
	Frontend     LegacyFrontend     `json:"frontend"`
	Capabilities []LegacyCapability `json:"capabilities"`
	Permissions  LegacyPermissions  `json:"permissions"`
	Upgrade      LegacyUpgrade      `json:"upgrade"`
}

type LegacyBackend struct {
	Artifact       string `json:"artifact"`
	Protocol       string `json:"protocol"`
	ShutdownMillis int    `json:"shutdownMillis"`
}

type LegacyFrontend struct {
	Entry string   `json:"entry"`
	Slots []string `json:"slots"`
}

type LegacyCapability struct {
	ID           string          `json:"id"`
	Summary      string          `json:"summary"`
	Risk         string          `json:"risk"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

type LegacyPermissions struct {
	WorkspaceRead   bool     `json:"workspaceRead"`
	PluginDataWrite bool     `json:"pluginDataWrite"`
	Network         []string `json:"network"`
	Secrets         []string `json:"secrets"`
	Background      bool     `json:"background"`
}

type LegacyUpgrade struct {
	Strategy      string `json:"strategy"`
	PinActiveRuns bool   `json:"pinActiveRuns"`
	StateVersion  int    `json:"stateVersion"`
}
