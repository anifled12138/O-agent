package pluginforge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

type State string

const (
	StateProposed         State = "proposed"
	StateGenerating       State = "generating"
	StateGenerated        State = "generated"
	StateBuilding         State = "building"
	StateTested           State = "tested"
	StateAwaitingApproval State = "awaiting_approval"
	StateApproved         State = "approved"
	StateInstalled        State = "installed"
	StateActive           State = "active"
	StateInactive         State = "inactive"
	StateGenerationFailed State = "generation_failed"
	StateBuildFailed      State = "build_failed"
	StateActivationFailed State = "activation_failed"
)

var transitions = map[State]map[State]bool{
	StateProposed:         {StateGenerating: true},
	StateGenerating:       {StateGenerated: true, StateGenerationFailed: true},
	StateGenerationFailed: {StateGenerating: true},
	StateGenerated:        {StateBuilding: true},
	StateBuilding:         {StateTested: true, StateBuildFailed: true},
	StateBuildFailed:      {StateBuilding: true, StateGenerating: true},
	StateTested:           {StateAwaitingApproval: true},
	StateAwaitingApproval: {StateApproved: true},
	StateApproved:         {StateInstalled: true, StateActivationFailed: true},
	StateInstalled:        {StateActive: true, StateActivationFailed: true},
	StateActive:           {StateInactive: true, StateGenerated: true, StateActivationFailed: true},
	StateInactive:         {StateActive: true, StateGenerated: true},
	StateActivationFailed: {StateApproved: true, StateInactive: true},
}

func CanTransition(from, to State) bool { return transitions[from][to] }

type PermissionSet struct {
	WorkspaceRead   bool     `json:"workspaceRead"`
	PluginDataWrite bool     `json:"pluginDataWrite"`
	Network         []string `json:"network"`
	Secrets         []string `json:"secrets"`
	Background      bool     `json:"background"`
}

type BackendEntry struct {
	Artifact       string `json:"artifact"`
	Protocol       string `json:"protocol"`
	ShutdownMillis int    `json:"shutdownMillis"`
}

type FrontendEntry struct {
	Entry string   `json:"entry"`
	Slots []string `json:"slots"`
}

type Capability struct {
	ID           string          `json:"id"`
	Summary      string          `json:"summary"`
	Risk         string          `json:"risk"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

type Manifest struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	APIVersion   string        `json:"apiVersion"`
	Description  string        `json:"description"`
	Backend      BackendEntry  `json:"backend"`
	Frontend     FrontendEntry `json:"frontend"`
	Capabilities []Capability  `json:"capabilities"`
	Permissions  PermissionSet `json:"permissions"`
	Upgrade      UpgradePolicy `json:"upgrade"`
}

type UpgradePolicy struct {
	Strategy      string `json:"strategy"`
	PinActiveRuns bool   `json:"pinActiveRuns"`
	StateVersion  int    `json:"stateVersion"`
}

type Project struct {
	ID          string          `json:"id"`
	UserID      string          `json:"-"`
	Name        string          `json:"name"`
	Slug        string          `json:"slug"`
	Description string          `json:"description"`
	State       State           `json:"state"`
	SourceDir   string          `json:"sourceDir"`
	Spec        json.RawMessage `json:"spec"`
	LastError   string          `json:"lastError,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

type ProjectView struct {
	Project
	LatestRelease *Release  `json:"latestRelease,omitempty"`
	Releases      []Release `json:"releases"`
}

type Release struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"projectId"`
	PluginID       string          `json:"pluginId"`
	Version        string          `json:"version"`
	Digest         string          `json:"digest"`
	BundleDir      string          `json:"-"`
	Manifest       Manifest        `json:"manifest"`
	TestReport     json.RawMessage `json:"testReport"`
	PermissionHash string          `json:"permissionHash"`
	CreatedAt      time.Time       `json:"createdAt"`
}

type Installation struct {
	ID              string    `json:"id"`
	UserID          string    `json:"-"`
	PluginID        string    `json:"pluginId"`
	ProjectID       string    `json:"projectId"`
	ActiveReleaseID string    `json:"activeReleaseId"`
	Status          string    `json:"status"`
	InstalledAt     time.Time `json:"installedAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type CapabilityBinding struct {
	Capability
	PluginID  string `json:"pluginId"`
	ReleaseID string `json:"releaseId"`
	Version   string `json:"version"`
}

type AuditEvent struct {
	ID        string          `json:"id"`
	UserID    string          `json:"userId"`
	ProjectID string          `json:"projectId,omitempty"`
	PluginID  string          `json:"pluginId,omitempty"`
	Action    string          `json:"action"`
	Details   json.RawMessage `json:"details"`
	CreatedAt time.Time       `json:"createdAt"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)

func (m Manifest) Validate() error {
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid plugin id %q", m.ID)
	}
	if m.Name == "" || m.Version == "" || m.APIVersion != "axiom.plugin/v1" {
		return errors.New("name, version, and apiVersion axiom.plugin/v1 are required")
	}
	if m.Backend.Artifact == "" || m.Backend.Protocol != "axiom.rpc/v1" {
		return errors.New("backend artifact and axiom.rpc/v1 are required")
	}
	if m.Frontend.Entry == "" {
		return errors.New("frontend entry is required")
	}
	seen := map[string]bool{}
	for _, capability := range m.Capabilities {
		if !idPattern.MatchString(capability.ID) || seen[capability.ID] {
			return fmt.Errorf("invalid or duplicate capability %q", capability.ID)
		}
		seen[capability.ID] = true
		if !json.Valid(capability.InputSchema) || !json.Valid(capability.OutputSchema) {
			return fmt.Errorf("capability %q has invalid schema", capability.ID)
		}
		for _, schema := range []json.RawMessage{capability.InputSchema, capability.OutputSchema} {
			var root struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(schema, &root) != nil || root.Type != "object" {
				return fmt.Errorf("capability %q schemas must use the axiom.plugin/v1 object subset", capability.ID)
			}
		}
	}
	if len(m.Capabilities) == 0 {
		return errors.New("at least one capability is required")
	}
	return nil
}

func PermissionHash(p PermissionSet) string {
	copy := p
	sort.Strings(copy.Network)
	sort.Strings(copy.Secrets)
	raw, _ := json.Marshal(copy)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func Slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && out.Len() > 0 {
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}
