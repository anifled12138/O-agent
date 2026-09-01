package pluginmanifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9A-Za-z][0-9A-Za-z.-]*$`)
)

var allowedHookEvents = map[string]bool{
	"agent.run.started":        true,
	"agent.run.completed":      true,
	"plugin.activated":         true,
	"plugin.deactivated":       true,
	"tool.before_execute":      true,
	"tool.after_execute":       true,
	"workspace.session_opened": true,
}

func (m Manifest) Validate() error {
	if m.SpecVersion != SpecV2 {
		return fmt.Errorf("specVersion must be %q", SpecV2)
	}
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("invalid plugin id %q", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Description) == "" {
		return errors.New("plugin name and description are required")
	}
	if !versionPattern.MatchString(m.Version) {
		return fmt.Errorf("invalid plugin version %q", m.Version)
	}
	if !m.hasSurface() {
		return errors.New("plugin must declare at least one runtime, UI, or export surface")
	}
	if err := m.validateRuntime(); err != nil {
		return err
	}
	if err := m.validateUI(); err != nil {
		return err
	}
	if err := m.validateExports(); err != nil {
		return err
	}
	if err := m.validateDependencies(); err != nil {
		return err
	}
	return m.validatePermissions()
}

func (m Manifest) hasSurface() bool {
	return m.Runtime != nil && m.Runtime.Backend != nil || m.UI != nil ||
		len(m.Exports.Services)+len(m.Exports.Tools)+len(m.Exports.Skills)+len(m.Exports.Hooks)+len(m.Exports.Jobs) > 0
}

func (m Manifest) validateRuntime() error {
	if m.Runtime == nil {
		return nil
	}
	if m.Runtime.Backend == nil {
		return errors.New("runtime must contain a backend")
	}
	backend := m.Runtime.Backend
	if !validPackagePath(backend.Artifact) {
		return fmt.Errorf("invalid backend artifact %q", backend.Artifact)
	}
	if backend.Protocol != "axiom.rpc/v1" && backend.Protocol != "axiom.rpc/v2" {
		return fmt.Errorf("unsupported backend protocol %q", backend.Protocol)
	}
	if backend.ShutdownMillis != 0 && (backend.ShutdownMillis < 1000 || backend.ShutdownMillis > 30000) {
		return errors.New("backend shutdownMillis must be between 1000 and 30000")
	}
	return nil
}

func (m Manifest) validateUI() error {
	if m.UI == nil {
		return nil
	}
	if !validPackagePath(m.UI.Entry) {
		return fmt.Errorf("invalid UI entry %q", m.UI.Entry)
	}
	if m.UI.Assets != "" && !validPackagePattern(m.UI.Assets) {
		return fmt.Errorf("invalid UI assets pattern %q", m.UI.Assets)
	}
	if m.UI.Sandbox != "" && m.UI.Sandbox != "strict" {
		return fmt.Errorf("unsupported UI sandbox %q", m.UI.Sandbox)
	}
	seen := map[string]bool{}
	for _, slot := range m.UI.Slots {
		if !idPattern.MatchString(slot) || seen[slot] {
			return fmt.Errorf("invalid or duplicate UI slot %q", slot)
		}
		seen[slot] = true
	}
	return nil
}

func (m Manifest) validateExports() error {
	seen := map[string]string{}
	serviceIDs := map[string]bool{}
	claim := func(kind, id string) error {
		if !idPattern.MatchString(id) {
			return fmt.Errorf("invalid %s export id %q", kind, id)
		}
		if previous := seen[id]; previous != "" {
			return fmt.Errorf("export id %q is shared by %s and %s", id, previous, kind)
		}
		seen[id] = kind
		return nil
	}
	for _, service := range m.Exports.Services {
		if err := claim("service", service.ID); err != nil {
			return err
		}
		if service.Contract == "" {
			return fmt.Errorf("service %q requires a contract", service.ID)
		}
		if m.Runtime == nil || m.Runtime.Backend == nil {
			return fmt.Errorf("service %q requires a backend runtime", service.ID)
		}
		serviceIDs[service.ID] = true
	}
	for _, tool := range m.Exports.Tools {
		if err := claim("tool", tool.ID); err != nil {
			return err
		}
		if strings.TrimSpace(tool.Summary) == "" || strings.TrimSpace(tool.Risk) == "" {
			return fmt.Errorf("tool %q requires summary and risk", tool.ID)
		}
		if !validVisibility(tool.Visibility) {
			return fmt.Errorf("tool %q has invalid visibility %q", tool.ID, tool.Visibility)
		}
		if err := validateObjectSchema(tool.ID, "input", tool.InputSchema); err != nil {
			return err
		}
		if err := validateObjectSchema(tool.ID, "output", tool.OutputSchema); err != nil {
			return err
		}
		if err := m.validateExecutor("tool "+tool.ID, tool.Executor, serviceIDs); err != nil {
			return err
		}
	}
	for _, skill := range m.Exports.Skills {
		if err := claim("skill", skill.ID); err != nil {
			return err
		}
		if strings.TrimSpace(skill.Summary) == "" || !validVisibility(skill.Visibility) {
			return fmt.Errorf("skill %q requires summary and valid visibility", skill.ID)
		}
		if !validPackagePath(skill.Entry) {
			return fmt.Errorf("skill %q has invalid entry %q", skill.ID, skill.Entry)
		}
	}
	for _, hook := range m.Exports.Hooks {
		if err := claim("hook", hook.ID); err != nil {
			return err
		}
		if m.Runtime == nil || m.Runtime.Backend == nil || hook.Handler == "" || len(hook.Events) == 0 {
			return fmt.Errorf("hook %q requires backend, handler, and events", hook.ID)
		}
		for _, event := range hook.Events {
			if !allowedHookEvents[event] {
				return fmt.Errorf("hook %q subscribes to unsupported event %q", hook.ID, event)
			}
		}
	}
	for _, job := range m.Exports.Jobs {
		if err := claim("job", job.ID); err != nil {
			return err
		}
		if !m.Permissions.Background {
			return fmt.Errorf("job %q requires background permission", job.ID)
		}
		if err := m.validateExecutor("job "+job.ID, job.Handler, serviceIDs); err != nil {
			return err
		}
	}
	return nil
}

func (m Manifest) validateExecutor(owner string, executor Executor, serviceIDs map[string]bool) error {
	if executor.Target == "" {
		return fmt.Errorf("%s requires an executor target", owner)
	}
	switch executor.Kind {
	case "backend":
		if m.Runtime == nil || m.Runtime.Backend == nil {
			return fmt.Errorf("%s references a backend executor without a backend", owner)
		}
	case "service":
		if !serviceIDs[executor.Target] {
			return fmt.Errorf("%s references unknown service %q", owner, executor.Target)
		}
	case "host":
		// Host executors are resolved from the trusted Host registry at activation.
	default:
		return fmt.Errorf("%s has invalid executor kind %q", owner, executor.Kind)
	}
	return nil
}

func (m Manifest) validateDependencies() error {
	seenPlugins := map[string]bool{}
	for _, dependency := range m.Dependencies.Plugins {
		if !idPattern.MatchString(dependency.ID) || dependency.ID == m.ID || dependency.Version == "" || seenPlugins[dependency.ID] {
			return fmt.Errorf("invalid plugin dependency %q", dependency.ID)
		}
		seenPlugins[dependency.ID] = true
	}
	seenServices := map[string]bool{}
	for _, dependency := range m.Dependencies.Services {
		if !idPattern.MatchString(dependency.ID) || dependency.Contract == "" || seenServices[dependency.ID] {
			return fmt.Errorf("invalid service dependency %q", dependency.ID)
		}
		seenServices[dependency.ID] = true
	}
	return nil
}

func (m Manifest) validatePermissions() error {
	for _, scope := range append(append([]string{}, m.Permissions.Filesystem.Read...), m.Permissions.Filesystem.Write...) {
		if scope != "${workspace}" && scope != "${pluginData}" && scope != "${temp}" {
			return fmt.Errorf("unsupported filesystem scope %q", scope)
		}
	}
	for _, host := range m.Permissions.Network {
		if strings.TrimSpace(host) == "" || strings.ContainsAny(host, "/:@") {
			return fmt.Errorf("network permission must be a hostname, got %q", host)
		}
	}
	for _, secret := range m.Permissions.Secrets {
		if !idPattern.MatchString(secret) {
			return fmt.Errorf("invalid secret permission %q", secret)
		}
	}
	return nil
}

func validateObjectSchema(id, direction string, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("tool %q has invalid %s schema", id, direction)
	}
	var schema struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &schema) != nil || schema.Type != "object" {
		return fmt.Errorf("tool %q %s schema must use the object subset", id, direction)
	}
	return nil
}

func validVisibility(value string) bool {
	return value == "none" || value == "discoverable" || value == "always" || value == "creator-only"
}

func validPackagePath(value string) bool {
	return value != "" && !strings.Contains(value, "\\") && !strings.HasPrefix(value, "/") && path.Clean(value) == value && value != "." && value != ".." && !strings.HasPrefix(value, "../")
}

func validPackagePattern(value string) bool {
	return value != "" && !strings.Contains(value, "\\") && !strings.HasPrefix(value, "/") && value != ".." && !strings.HasPrefix(value, "../")
}
