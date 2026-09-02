package pluginruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"axiom.local/agent/internal/pluginforge"
)

// Supervisor owns out-of-process plugin runtimes. A release is immutable and a
// capability binding always points at one concrete process/release pair.
type Supervisor struct {
	mu            sync.RWMutex
	workspaceRoot string
	plugins       map[string]*mountedRelease
	capabilities  map[string]*binding
	services      map[string]*serviceBinding
	skills        map[string]pluginforge.SkillBinding
	uis           map[string]pluginforge.UIBinding
	hooks         map[string]surfaceBinding
	jobs          map[string]surfaceBinding
	epoch         atomic.Uint64
	observer      func(pluginforge.RuntimeEvent)
	broker        *ResourceBroker
	hostTools     map[string]BrokerCommand
}

type mountedRelease struct {
	release pluginforge.Release
	process *process
	epoch   uint64
}

type surfaceBinding struct {
	PluginID  string
	ReleaseID string
	ID        string
}

type turnLease struct {
	supervisor *Supervisor
	tools      map[string]*binding
	skills     []pluginforge.SkillBinding
	processes  []*process
	closed     atomic.Bool
}

type binding struct {
	capability pluginforge.CapabilityBinding
	process    *process
	host       BrokerCommand
	method     string
	release    pluginforge.Release
}

type serviceBinding struct {
	binding pluginforge.ServiceBinding
	process *process
	release pluginforge.Release
}

type process struct {
	release     pluginforge.Release
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      *bufio.Reader
	stderr      cappedBuffer
	containment *processContainment

	callMu   sync.Mutex
	request  atomic.Uint64
	inFlight sync.WaitGroup
	draining atomic.Bool
	stopped  atomic.Bool
	stopping atomic.Bool
	done     chan struct{}
	waitMu   sync.Mutex
	waitErr  error
	userID   string
	broker   *ResourceBroker
}

const (
	maxRPCFrameBytes     = 1 << 20
	maxPluginInputBytes  = 256 << 10
	maxPluginStderrBytes = 64 << 10
)

type cappedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	limit  int
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit == 0 {
		b.limit = maxPluginStderrBytes
	}
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if remaining > len(value) {
			remaining = len(value)
		}
		_, _ = b.buffer.Write(value[:remaining])
	}
	return len(value), nil
}

func (b *cappedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buffer.String() }

type rpcRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

func New(workspaceRoot string) (*Supervisor, error) {
	return NewWithData(workspaceRoot, filepath.Join(workspaceRoot, "data"))
}

func NewWithData(workspaceRoot, dataRoot string) (*Supervisor, error) {
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("plugin workspace root is unavailable: %s", root)
	}
	broker, err := NewResourceBroker(root, dataRoot, nil, nil)
	if err != nil {
		return nil, err
	}
	return &Supervisor{
		workspaceRoot: root, plugins: map[string]*mountedRelease{}, capabilities: map[string]*binding{},
		services: map[string]*serviceBinding{}, skills: map[string]pluginforge.SkillBinding{},
		uis: map[string]pluginforge.UIBinding{}, hooks: map[string]surfaceBinding{}, jobs: map[string]surfaceBinding{}, broker: broker,
		hostTools: map[string]BrokerCommand{"host.workspace.describe": func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"mounted":true,"resourceAccess":"brokered"}`), nil
		}},
	}, nil
}

func (s *Supervisor) Activate(ctx context.Context, userID string, release pluginforge.Release) error {
	if err := release.Manifest.Validate(); err != nil {
		return err
	}
	var candidate *process
	if release.Manifest.Runtime != nil && release.Manifest.Runtime.Backend != nil {
		var startErr error
		candidate, startErr = startProcess(ctx, userID, release, s.broker, func(current *process, exitErr error) { s.processExited(userID, release, current, exitErr) })
		if startErr != nil {
			return startErr
		}
	}

	pluginKey := key(userID, release.PluginID)
	mount := &mountedRelease{release: release, process: candidate}
	newBindings := make(map[string]*binding, len(release.Manifest.Exports.Tools))
	for _, capability := range release.Manifest.Exports.Tools {
		next := &binding{process: candidate, method: capability.Executor.Target, release: release}
		switch capability.Executor.Kind {
		case "backend":
			if candidate == nil {
				return fmt.Errorf("tool %q requires a backend process", capability.ID)
			}
		case "service":
			if candidate == nil {
				return fmt.Errorf("tool %q requires its service backend", capability.ID)
			}
			next.method = "service.call"
		case "host":
			next.process = nil
			next.host = s.hostTools[capability.Executor.Target]
			if next.host == nil {
				if candidate != nil {
					_ = candidate.forceStop()
				}
				return fmt.Errorf("tool %q references unregistered Host target %q", capability.ID, capability.Executor.Target)
			}
		default:
			if candidate != nil {
				_ = candidate.forceStop()
			}
			return fmt.Errorf("tool %q uses unsupported executor kind %q", capability.ID, capability.Executor.Kind)
		}
		capabilityKey := key(userID, capability.ID)
		next.capability = pluginforge.CapabilityBinding{
			ToolExport: capability,
			PluginID:   release.PluginID,
			ReleaseID:  release.ID,
			Version:    release.Version,
		}
		newBindings[capabilityKey] = next
	}
	newServices := make(map[string]*serviceBinding, len(release.Manifest.Exports.Services))
	for _, service := range release.Manifest.Exports.Services {
		newServices[key(userID, service.ID)] = &serviceBinding{
			binding: pluginforge.ServiceBinding{PluginID: release.PluginID, ReleaseID: release.ID, Version: release.Version, Service: service},
			process: candidate,
			release: release,
		}
	}
	newSkills := make(map[string]pluginforge.SkillBinding, len(release.Manifest.Exports.Skills))
	for _, skill := range release.Manifest.Exports.Skills {
		newSkills[key(userID, skill.ID)] = pluginforge.SkillBinding{PluginID: release.PluginID, ReleaseID: release.ID, Version: release.Version, BundleDir: release.BundleDir, Skill: skill}
	}
	var nextUI *pluginforge.UIBinding
	if release.Manifest.UI != nil {
		binding := pluginforge.UIBinding{PluginID: release.PluginID, ReleaseID: release.ID, Version: release.Version, UI: *release.Manifest.UI}
		nextUI = &binding
	}
	newHooks := make(map[string]surfaceBinding, len(release.Manifest.Exports.Hooks))
	for _, hook := range release.Manifest.Exports.Hooks {
		newHooks[key(userID, hook.ID)] = surfaceBinding{PluginID: release.PluginID, ReleaseID: release.ID, ID: hook.ID}
	}
	newJobs := make(map[string]surfaceBinding, len(release.Manifest.Exports.Jobs))
	for _, job := range release.Manifest.Exports.Jobs {
		newJobs[key(userID, job.ID)] = surfaceBinding{PluginID: release.PluginID, ReleaseID: release.ID, ID: job.ID}
	}

	s.mu.Lock()
	old := s.plugins[pluginKey]
	if dependencyErr := s.activationDependencyError(userID, release); dependencyErr != "" {
		s.mu.Unlock()
		if candidate != nil {
			_ = candidate.forceStop()
		}
		return errors.New(dependencyErr)
	}
	if candidate != nil && candidate.stopped.Load() {
		s.mu.Unlock()
		return errors.New("plugin backend exited during activation preparation")
	}
	if conflict := s.registrationConflict(userID, release.PluginID, newBindings, newServices, newSkills, newHooks, newJobs); conflict != "" {
		s.mu.Unlock()
		if candidate != nil {
			_ = candidate.forceStop()
		}
		return errors.New(conflict)
	}
	nextEpoch := s.epoch.Add(1)
	mount.epoch = nextEpoch
	for _, item := range newBindings {
		item.capability.RegistryEpoch = nextEpoch
	}
	for id, item := range newServices {
		item.binding.RegistryEpoch = nextEpoch
		newServices[id] = item
	}
	for id, item := range newSkills {
		item.RegistryEpoch = nextEpoch
		newSkills[id] = item
	}
	if nextUI != nil {
		nextUI.RegistryEpoch = nextEpoch
	}
	s.removePluginBindings(userID, release.PluginID)
	s.plugins[pluginKey] = mount
	for capabilityKey, next := range newBindings {
		s.capabilities[capabilityKey] = next
	}
	for id, service := range newServices {
		s.services[id] = service
	}
	for id, skill := range newSkills {
		s.skills[id] = skill
	}
	for id, hook := range newHooks {
		s.hooks[id] = hook
	}
	for id, job := range newJobs {
		s.jobs[id] = job
	}
	if nextUI != nil {
		s.uis[pluginKey] = *nextUI
	}
	s.mu.Unlock()

	if old != nil && old.process != nil {
		old.process.draining.Store(true)
		go old.process.stop(shutdownDuration(backendShutdownMillis(old.release)))
	}
	return nil
}

func (s *Supervisor) SetObserver(observer func(pluginforge.RuntimeEvent)) {
	s.mu.Lock()
	s.observer = observer
	s.mu.Unlock()
}

func (s *Supervisor) processExited(userID string, release pluginforge.Release, exited *process, exitErr error) {
	if exited.stopping.Load() {
		return
	}
	s.mu.Lock()
	mounted := s.plugins[key(userID, release.PluginID)]
	if mounted == nil || mounted.process != exited || mounted.release.ID != release.ID {
		s.mu.Unlock()
		return
	}
	mounted.process = nil
	prefix := userID + "\x00"
	for id, item := range s.capabilities {
		if strings.HasPrefix(id, prefix) && item.process == exited {
			delete(s.capabilities, id)
		}
	}
	for id, item := range s.services {
		if strings.HasPrefix(id, prefix) && item.process == exited {
			delete(s.services, id)
		}
	}
	for id, item := range s.hooks {
		if strings.HasPrefix(id, prefix) && item.PluginID == release.PluginID {
			delete(s.hooks, id)
		}
	}
	for id, item := range s.jobs {
		if strings.HasPrefix(id, prefix) && item.PluginID == release.PluginID {
			delete(s.jobs, id)
		}
	}
	observer := s.observer
	s.mu.Unlock()
	message := "plugin backend exited"
	if exitErr != nil {
		message += ": " + exitErr.Error()
	}
	if observer != nil {
		observer(pluginforge.RuntimeEvent{UserID: userID, PluginID: release.PluginID, ReleaseID: release.ID, Kind: "backend.crashed", Error: message, At: time.Now().UTC()})
	}
}

func (s *Supervisor) Deactivate(_ context.Context, userID, pluginID string) error {
	pluginKey := key(userID, pluginID)
	s.mu.Lock()
	current := s.plugins[pluginKey]
	if current == nil {
		s.mu.Unlock()
		return nil
	}
	s.removePluginBindings(userID, pluginID)
	s.mu.Unlock()
	if current.process != nil {
		current.process.draining.Store(true)
		go current.process.stop(shutdownDuration(backendShutdownMillis(current.release)))
	}
	return nil
}

func (s *Supervisor) Invoke(ctx context.Context, userID, capabilityID string, input json.RawMessage) (json.RawMessage, error) {
	return s.invoke(ctx, userID, capabilityID, "", input)
}

func (s *Supervisor) InvokePinned(ctx context.Context, userID, capabilityID, releaseID string, input json.RawMessage) (json.RawMessage, error) {
	return s.invoke(ctx, userID, capabilityID, releaseID, input)
}

func (s *Supervisor) invoke(ctx context.Context, userID, capabilityID, releaseID string, input json.RawMessage) (json.RawMessage, error) {
	s.mu.RLock()
	selected := s.capabilities[key(userID, capabilityID)]
	if selected != nil && (releaseID == "" || selected.capability.ReleaseID == releaseID) && (selected.process == nil || !selected.process.draining.Load()) {
		if selected.process != nil {
			selected.process.inFlight.Add(1)
		}
	} else {
		selected = nil
	}
	s.mu.RUnlock()
	if selected == nil {
		return nil, fmt.Errorf("capability %q is not active", capabilityID)
	}
	if selected.process != nil {
		defer selected.process.inFlight.Done()
	}
	return s.invokeBinding(ctx, selected, capabilityID, input)
}

func (s *Supervisor) invokeBinding(ctx context.Context, selected *binding, capabilityID string, input json.RawMessage) (json.RawMessage, error) {
	if len(input) > maxPluginInputBytes {
		return nil, errors.New("capability input exceeds 256 KiB")
	}
	params := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &params); err != nil {
			return nil, fmt.Errorf("invalid capability input: %w", err)
		}
	}
	if err := validateSchema(input, selected.capability.InputSchema); err != nil {
		return nil, fmt.Errorf("capability input contract: %w", err)
	}
	params["_axiomCapabilityId"] = capabilityID
	if selected.capability.Executor.Kind == "service" {
		params["_axiomServiceId"] = selected.capability.Executor.Target
	}
	if selected.host != nil {
		raw, _ := json.Marshal(params)
		result, err := selected.host(ctx, raw)
		if err != nil {
			return nil, err
		}
		if !json.Valid(result) {
			return nil, errors.New("Host tool returned invalid JSON")
		}
		if err := validateSchema(result, selected.capability.OutputSchema); err != nil {
			return nil, fmt.Errorf("capability output contract: %w", err)
		}
		return result, nil
	}
	// The host, not plugin UI or model output, chooses the filesystem boundary.
	if hasPermission(selected.release.Manifest.Permissions.Filesystem.Read, "${workspace}") {
		params["root"] = s.workspaceRoot
	} else {
		delete(params, "root")
	}
	raw, _ := json.Marshal(params)
	result, err := selected.process.call(ctx, selected.method, raw)
	if err != nil {
		return nil, err
	}
	if !json.Valid(result) {
		return nil, errors.New("plugin returned invalid JSON")
	}
	if err := validateSchema(result, selected.capability.OutputSchema); err != nil {
		return nil, fmt.Errorf("capability output contract: %w", err)
	}
	return result, nil
}

// UICall is the Presentation-plane bridge. It deliberately resolves a mounted
// UI/backend pair by plugin ID and never consults the Agent tool registry.
func (s *Supervisor) UICall(ctx context.Context, userID, pluginID, operation string, input json.RawMessage) (json.RawMessage, error) {
	if operation == "" || len(operation) > 128 || strings.ContainsAny(operation, " \t\r\n") {
		return nil, errors.New("invalid UI operation")
	}
	if len(input) > maxPluginInputBytes {
		return nil, errors.New("UI call input exceeds 256 KiB")
	}
	s.mu.RLock()
	mounted := s.plugins[key(userID, pluginID)]
	_, hasUI := s.uis[key(userID, pluginID)]
	if mounted != nil && mounted.process != nil && hasUI && !mounted.process.draining.Load() {
		mounted.process.inFlight.Add(1)
	} else {
		mounted = nil
	}
	s.mu.RUnlock()
	if mounted == nil {
		return nil, fmt.Errorf("plugin UI %q has no active backend bridge", pluginID)
	}
	defer mounted.process.inFlight.Done()
	var value any = map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &value); err != nil {
			return nil, fmt.Errorf("invalid UI call input: %w", err)
		}
	}
	params := map[string]any{"operation": operation, "input": value}
	if hasPermission(mounted.release.Manifest.Permissions.Filesystem.Read, "${workspace}") {
		params["root"] = s.workspaceRoot
	}
	raw, _ := json.Marshal(params)
	result, err := mounted.process.call(ctx, "ui.call", raw)
	if err != nil {
		return nil, err
	}
	if !json.Valid(result) {
		return nil, errors.New("plugin returned invalid JSON")
	}
	return result, nil
}

// UIServiceCall lets a sandboxed UI consume only services declared in the
// calling release's dependency contract. The service remains outside the
// Agent capability index and executes under the UI principal.
func (s *Supervisor) UIServiceCall(ctx context.Context, userID, callerPluginID, serviceID string, input json.RawMessage) (json.RawMessage, error) {
	if len(input) > maxPluginInputBytes {
		return nil, errors.New("UI service input exceeds 256 KiB")
	}
	var value any = map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &value); err != nil {
			return nil, fmt.Errorf("invalid UI service input: %w", err)
		}
	}

	s.mu.RLock()
	caller := s.plugins[key(userID, callerPluginID)]
	_, hasUI := s.uis[key(userID, callerPluginID)]
	selected := s.services[key(userID, serviceID)]
	contract := ""
	if caller != nil && hasUI {
		for _, dependency := range caller.release.Manifest.Dependencies.Services {
			if dependency.ID == serviceID {
				contract = dependency.Contract
				break
			}
		}
	}
	if caller == nil || !hasUI || selected == nil || selected.process == nil || selected.process.draining.Load() || selected.process.stopped.Load() || contract == "" || contract != selected.binding.Service.Contract {
		selected = nil
	} else {
		selected.process.inFlight.Add(1)
	}
	s.mu.RUnlock()
	if selected == nil {
		return nil, fmt.Errorf("service %q is not an active dependency of UI plugin %q", serviceID, callerPluginID)
	}
	defer selected.process.inFlight.Done()

	method := selected.binding.Service.Handler
	if method == "" {
		method = "service.call"
	}
	params, _ := json.Marshal(map[string]any{
		"_axiomServiceId": serviceID,
		"principal":       "ui",
		"callerPluginId":  callerPluginID,
		"input":           value,
	})
	result, err := selected.process.call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	if !json.Valid(result) {
		return nil, errors.New("service returned invalid JSON")
	}
	return result, nil
}

func (s *Supervisor) Capabilities(userID string) []pluginforge.CapabilityBinding {
	prefix := userID + "\x00"
	s.mu.RLock()
	result := make([]pluginforge.CapabilityBinding, 0)
	for capabilityKey, item := range s.capabilities {
		if strings.HasPrefix(capabilityKey, prefix) {
			result = append(result, item.capability)
		}
	}
	s.mu.RUnlock()
	return result
}

func (s *Supervisor) SurfaceStates(userID, pluginID string) []pluginforge.SurfaceState {
	pluginKey := key(userID, pluginID)
	prefix := userID + "\x00"
	s.mu.RLock()
	mounted := s.plugins[pluginKey]
	if mounted == nil {
		s.mu.RUnlock()
		return nil
	}
	now := time.Now().UTC()
	result := make([]pluginforge.SurfaceState, 0)
	add := func(kind, id string) {
		result = append(result, pluginforge.SurfaceState{UserID: userID, PluginID: pluginID, ReleaseID: mounted.release.ID, Kind: kind, SurfaceID: id, Status: "active", RegistryEpoch: mounted.epoch, UpdatedAt: now})
	}
	if mounted.process != nil {
		add("runtime", "backend")
	}
	if ui, ok := s.uis[pluginKey]; ok {
		add("ui", ui.UI.Entry)
	}
	for id, item := range s.services {
		if strings.HasPrefix(id, prefix) && item.binding.PluginID == pluginID {
			add("service", item.binding.Service.ID)
		}
	}
	for id, item := range s.capabilities {
		if strings.HasPrefix(id, prefix) && item.capability.PluginID == pluginID {
			add("tool", item.capability.ID)
		}
	}
	for id, item := range s.skills {
		if strings.HasPrefix(id, prefix) && item.PluginID == pluginID {
			add("skill", item.Skill.ID)
		}
	}
	for id, item := range s.hooks {
		if strings.HasPrefix(id, prefix) && item.PluginID == pluginID {
			add("hook", item.ID)
		}
	}
	for id, item := range s.jobs {
		if strings.HasPrefix(id, prefix) && item.PluginID == pluginID {
			add("job", item.ID)
		}
	}
	s.mu.RUnlock()
	return result
}

func (s *Supervisor) UI(userID, pluginID string) (pluginforge.UIBinding, bool) {
	s.mu.RLock()
	value, ok := s.uis[key(userID, pluginID)]
	s.mu.RUnlock()
	return value, ok
}

func (s *Supervisor) UIs(userID string) []pluginforge.UIBinding {
	prefix := userID + "\x00"
	s.mu.RLock()
	result := []pluginforge.UIBinding{}
	for id, item := range s.uis {
		if strings.HasPrefix(id, prefix) {
			result = append(result, item)
		}
	}
	s.mu.RUnlock()
	return result
}

func (s *Supervisor) Skills(userID string) []pluginforge.SkillBinding {
	prefix := userID + "\x00"
	s.mu.RLock()
	result := make([]pluginforge.SkillBinding, 0)
	for id, item := range s.skills {
		if strings.HasPrefix(id, prefix) {
			result = append(result, item)
		}
	}
	s.mu.RUnlock()
	return result
}

func (s *Supervisor) BeginTurn(userID string) pluginforge.TurnLease {
	prefix := userID + "\x00"
	s.mu.RLock()
	lease := &turnLease{supervisor: s, tools: map[string]*binding{}}
	seen := map[*process]bool{}
	for id, item := range s.capabilities {
		if !strings.HasPrefix(id, prefix) || item.process != nil && item.process.draining.Load() {
			continue
		}
		lease.tools[item.capability.ID] = item
		if item.process != nil && !seen[item.process] {
			item.process.inFlight.Add(1)
			seen[item.process] = true
			lease.processes = append(lease.processes, item.process)
		}
	}
	for id, item := range s.skills {
		if strings.HasPrefix(id, prefix) {
			lease.skills = append(lease.skills, item)
		}
	}
	s.mu.RUnlock()
	return lease
}

func (l *turnLease) Capabilities() []pluginforge.CapabilityBinding {
	result := make([]pluginforge.CapabilityBinding, 0, len(l.tools))
	for _, item := range l.tools {
		result = append(result, item.capability)
	}
	return result
}

func (l *turnLease) Skills() []pluginforge.SkillBinding {
	return append([]pluginforge.SkillBinding(nil), l.skills...)
}

func (l *turnLease) Invoke(ctx context.Context, capabilityID string, input json.RawMessage) (json.RawMessage, error) {
	if l.closed.Load() {
		return nil, errors.New("Agent turn capability lease is closed")
	}
	selected := l.tools[capabilityID]
	if selected == nil {
		return nil, fmt.Errorf("capability %q was not pinned for this Agent turn", capabilityID)
	}
	return l.supervisor.invokeBinding(ctx, selected, capabilityID, input)
}

func (l *turnLease) Close() {
	if l.closed.Swap(true) {
		return
	}
	for _, current := range l.processes {
		current.inFlight.Done()
	}
}

func (s *Supervisor) Close() error {
	s.mu.Lock()
	processes := make([]*process, 0, len(s.plugins))
	for _, current := range s.plugins {
		if current.process != nil {
			current.process.draining.Store(true)
			processes = append(processes, current.process)
		}
	}
	s.plugins = map[string]*mountedRelease{}
	s.capabilities = map[string]*binding{}
	s.services = map[string]*serviceBinding{}
	s.skills = map[string]pluginforge.SkillBinding{}
	s.uis = map[string]pluginforge.UIBinding{}
	s.hooks = map[string]surfaceBinding{}
	s.jobs = map[string]surfaceBinding{}
	s.mu.Unlock()
	for _, current := range processes {
		current.stop(shutdownDuration(backendShutdownMillis(current.release)))
	}
	return nil
}

func (s *Supervisor) registrationConflict(userID, pluginID string, tools map[string]*binding, services map[string]*serviceBinding, skills map[string]pluginforge.SkillBinding, hooks, jobs map[string]surfaceBinding) string {
	for id := range tools {
		if current := s.capabilities[id]; current != nil && current.capability.PluginID != pluginID {
			return fmt.Sprintf("tool export %q is already registered", strings.TrimPrefix(id, userID+"\x00"))
		}
	}
	for id := range services {
		if current, ok := s.services[id]; ok && current.binding.PluginID != pluginID {
			return fmt.Sprintf("service export %q is already registered", strings.TrimPrefix(id, userID+"\x00"))
		}
	}
	for id := range skills {
		if current, ok := s.skills[id]; ok && current.PluginID != pluginID {
			return fmt.Sprintf("skill export %q is already registered", strings.TrimPrefix(id, userID+"\x00"))
		}
	}
	for id := range hooks {
		if current, ok := s.hooks[id]; ok && current.PluginID != pluginID {
			return fmt.Sprintf("hook export %q is already registered", strings.TrimPrefix(id, userID+"\x00"))
		}
	}
	for id := range jobs {
		if current, ok := s.jobs[id]; ok && current.PluginID != pluginID {
			return fmt.Sprintf("job export %q is already registered", strings.TrimPrefix(id, userID+"\x00"))
		}
	}
	return ""
}

func (s *Supervisor) activationDependencyError(userID string, release pluginforge.Release) string {
	for _, dependency := range release.Manifest.Dependencies.Plugins {
		mounted := s.plugins[key(userID, dependency.ID)]
		if mounted == nil || !versionSatisfies(mounted.release.Version, dependency.Version) {
			return fmt.Sprintf("plugin dependency %s %s is not active", dependency.ID, dependency.Version)
		}
	}
	for _, dependency := range release.Manifest.Dependencies.Services {
		service, ok := s.services[key(userID, dependency.ID)]
		if !ok || service.binding.Service.Contract != dependency.Contract {
			return fmt.Sprintf("service dependency %s (%s) is not active", dependency.ID, dependency.Contract)
		}
	}
	return ""
}

func versionSatisfies(current, constraint string) bool {
	if constraint == "*" || current == constraint {
		return true
	}
	prefix := ""
	for _, candidate := range []string{"^", "~", ">="} {
		if strings.HasPrefix(constraint, candidate) {
			prefix = candidate
			constraint = strings.TrimPrefix(constraint, candidate)
			break
		}
	}
	currentParts, currentOK := numericVersion(current)
	wantedParts, wantedOK := numericVersion(constraint)
	if !currentOK || !wantedOK {
		return false
	}
	compare := 0
	for index := 0; index < 3; index++ {
		if currentParts[index] < wantedParts[index] {
			compare = -1
			break
		}
		if currentParts[index] > wantedParts[index] {
			compare = 1
			break
		}
	}
	switch prefix {
	case ">=":
		return compare >= 0
	case "^":
		if compare < 0 || currentParts[0] != wantedParts[0] {
			return false
		}
		if wantedParts[0] > 0 {
			return true
		}
		if currentParts[1] != wantedParts[1] {
			return false
		}
		return wantedParts[1] > 0 || currentParts[2] == wantedParts[2]
	case "~":
		return currentParts[0] == wantedParts[0] && currentParts[1] == wantedParts[1] && compare >= 0
	}
	return false
}

func numericVersion(value string) ([3]int, bool) {
	var result [3]int
	core := strings.SplitN(value, "-", 2)[0]
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return result, false
	}
	for index, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil {
			return result, false
		}
		result[index] = parsed
	}
	return result, true
}

func (s *Supervisor) removePluginBindings(userID, pluginID string) {
	pluginKey := key(userID, pluginID)
	delete(s.plugins, pluginKey)
	delete(s.uis, pluginKey)
	for id, item := range s.capabilities {
		if item.capability.PluginID == pluginID && strings.HasPrefix(id, userID+"\x00") {
			delete(s.capabilities, id)
		}
	}
	for id, item := range s.services {
		if item.binding.PluginID == pluginID && strings.HasPrefix(id, userID+"\x00") {
			delete(s.services, id)
		}
	}
	for id, item := range s.skills {
		if item.PluginID == pluginID && strings.HasPrefix(id, userID+"\x00") {
			delete(s.skills, id)
		}
	}
	for id, item := range s.hooks {
		if item.PluginID == pluginID && strings.HasPrefix(id, userID+"\x00") {
			delete(s.hooks, id)
		}
	}
	for id, item := range s.jobs {
		if item.PluginID == pluginID && strings.HasPrefix(id, userID+"\x00") {
			delete(s.jobs, id)
		}
	}
}

func startProcess(ctx context.Context, userID string, release pluginforge.Release, broker *ResourceBroker, onExit func(*process, error)) (*process, error) {
	bundleRoot, err := filepath.Abs(release.BundleDir)
	if err != nil {
		return nil, err
	}
	if release.Manifest.Runtime == nil || release.Manifest.Runtime.Backend == nil {
		return nil, errors.New("plugin release has no backend runtime")
	}
	artifact := filepath.Clean(filepath.FromSlash(release.Manifest.Runtime.Backend.Artifact))
	if runtime.GOOS != "windows" {
		artifact = strings.TrimSuffix(artifact, ".exe")
	}
	if filepath.IsAbs(artifact) || strings.HasPrefix(artifact, "..") {
		return nil, errors.New("plugin backend artifact escapes its release")
	}
	executable := filepath.Join(bundleRoot, artifact)
	executable, err = filepath.Abs(executable)
	if err != nil || !within(bundleRoot, executable) {
		return nil, errors.New("plugin backend artifact escapes its release")
	}
	command := exec.Command(executable)
	command.Dir = bundleRoot
	command.Env = []string{"AXIOM_PLUGIN_ID=" + release.PluginID, "AXIOM_RELEASE_ID=" + release.ID}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	current := &process{release: release, userID: userID, broker: broker, cmd: command, stdin: stdin, stdout: bufio.NewReaderSize(stdout, maxRPCFrameBytes), done: make(chan struct{})}
	command.Stderr = &current.stderr
	if err = command.Start(); err != nil {
		return nil, err
	}
	current.containment, err = containProcess(command)
	if err != nil {
		_ = current.forceStop()
		return nil, fmt.Errorf("apply plugin process containment: %w", err)
	}
	go func() {
		waitErr := command.Wait()
		current.waitMu.Lock()
		current.waitErr = waitErr
		current.waitMu.Unlock()
		current.stopped.Store(true)
		_ = current.containment.Close()
		close(current.done)
		if onExit != nil {
			onExit(current, waitErr)
		}
	}()
	protocol := release.Manifest.Runtime.Backend.Protocol
	helloInput, _ := json.Marshal(map[string]any{"host": "axiom", "protocol": protocol, "brokers": []string{"filesystem", "network", "secret", "process"}})
	hello, err := current.call(ctx, "plugin.hello", helloInput)
	if err == nil {
		var status struct {
			Protocol string `json:"protocol"`
			Status   string `json:"status"`
		}
		err = json.Unmarshal(hello, &status)
		if err == nil && (status.Protocol != protocol || status.Status != "ready") {
			err = fmt.Errorf("plugin rejected protocol handshake")
		}
	}
	if err == nil {
		_, err = current.call(ctx, "plugin.health", json.RawMessage(`{}`))
	}
	if err != nil {
		_ = current.forceStop()
		stderr := strings.TrimSpace(current.stderr.String())
		if stderr != "" {
			return nil, fmt.Errorf("plugin startup failed: %w: %s", err, stderr)
		}
		return nil, fmt.Errorf("plugin startup failed: %w", err)
	}
	return current, nil
}

func (p *process) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if p.stopped.Load() {
		return nil, errors.New("plugin process is stopped")
	}
	p.callMu.Lock()
	defer p.callMu.Unlock()
	requestID := fmt.Sprintf("req_%d", p.request.Add(1))
	raw, _ := json.Marshal(rpcRequest{ID: requestID, Method: method, Params: params})
	if len(raw) > maxRPCFrameBytes {
		return nil, errors.New("plugin RPC request exceeds 1 MiB")
	}
	if _, err := p.stdin.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	return p.awaitResponse(ctx, requestID)
}

func (p *process) awaitResponse(ctx context.Context, requestID string) (json.RawMessage, error) {
	for {
		type outcome struct {
			line []byte
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			line, err := p.stdout.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				err = errors.New("plugin RPC response exceeds 1 MiB")
			}
			done <- outcome{line: line, err: err}
		}()
		select {
		case <-ctx.Done():
			_ = p.forceStop()
			return nil, ctx.Err()
		case received := <-done:
			if received.err != nil {
				_ = p.forceStop()
				return nil, received.err
			}
			var brokerRequest rpcRequest
			if json.Unmarshal(received.line, &brokerRequest) == nil && brokerRequest.Method != "" {
				if brokerRequest.ID == "" || !strings.HasPrefix(brokerRequest.Method, "host.") {
					_ = p.forceStop()
					return nil, errors.New("plugin emitted an invalid Host broker request")
				}
				result, brokerErr := p.handleBroker(ctx, brokerRequest.Method, brokerRequest.Params)
				response := rpcResponse{ID: brokerRequest.ID, Result: result}
				if brokerErr != nil {
					response.Result = nil
					response.Error = brokerErr.Error()
				}
				raw, _ := json.Marshal(response)
				if len(raw) > maxRPCFrameBytes {
					return nil, errors.New("Host broker response exceeds 1 MiB")
				}
				if _, err := p.stdin.Write(append(raw, '\n')); err != nil {
					return nil, err
				}
				continue
			}
			var response rpcResponse
			if err := json.Unmarshal(received.line, &response); err != nil {
				return nil, fmt.Errorf("invalid plugin response: %w", err)
			}
			if response.ID != requestID {
				return nil, fmt.Errorf("plugin response id mismatch")
			}
			if response.Error != "" {
				return nil, errors.New(response.Error)
			}
			return response.Result, nil
		}
	}
}

func (p *process) handleBroker(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if p.broker == nil || p.release.Manifest.Runtime == nil || p.release.Manifest.Runtime.Backend == nil || p.release.Manifest.Runtime.Backend.Protocol != "axiom.rpc/v2" {
		return nil, errors.New("Host broker requires axiom.rpc/v2")
	}
	switch method {
	case "host.fs.read":
		var input struct{ Scope, Path string }
		if json.Unmarshal(params, &input) != nil {
			return nil, errors.New("invalid filesystem broker request")
		}
		value, err := p.broker.ReadFile(p.release, input.Scope, input.Path)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"content": string(value)})
	case "host.fs.list":
		var input struct{ Scope, Path string }
		if json.Unmarshal(params, &input) != nil {
			return nil, errors.New("invalid filesystem broker request")
		}
		value, err := p.broker.ListDir(p.release, input.Scope, input.Path)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"entries": value})
	case "host.fs.write":
		var input struct{ Scope, Path, Content string }
		if json.Unmarshal(params, &input) != nil {
			return nil, errors.New("invalid filesystem broker request")
		}
		return json.RawMessage(`{"written":true}`), p.broker.WriteFile(p.release, input.Scope, input.Path, []byte(input.Content))
	case "host.secret.get":
		var input struct{ Name string }
		if json.Unmarshal(params, &input) != nil {
			return nil, errors.New("invalid secret broker request")
		}
		value, err := p.broker.Secret(ctx, p.userID, p.release, input.Name)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"valueBase64": base64.StdEncoding.EncodeToString(value)})
	case "host.process.run":
		var input struct {
			Name  string
			Input json.RawMessage
		}
		if json.Unmarshal(params, &input) != nil {
			return nil, errors.New("invalid process broker request")
		}
		return p.broker.Run(ctx, p.release, input.Name, input.Input)
	case "host.net.fetch":
		var input struct{ Method, URL, Body string }
		if json.Unmarshal(params, &input) != nil {
			return nil, errors.New("invalid network broker request")
		}
		response, err := p.broker.Fetch(ctx, p.release, input.Method, input.URL, strings.NewReader(input.Body))
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || len(body) > maxBrokerHTTPBytes {
			return nil, errors.New("brokered HTTP response exceeds 2 MiB")
		}
		return json.Marshal(map[string]any{"status": response.StatusCode, "bodyBase64": base64.StdEncoding.EncodeToString(body), "contentType": response.Header.Get("Content-Type")})
	default:
		return nil, fmt.Errorf("unknown Host broker method %q", method)
	}
}

func (p *process) stop(timeout time.Duration) {
	p.stopping.Store(true)
	drained := make(chan struct{})
	go func() { p.inFlight.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(timeout):
		_ = p.forceStop()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _ = p.call(ctx, "plugin.shutdown", json.RawMessage(`{}`))
	select {
	case <-p.done:
		p.stopped.Store(true)
		_ = p.containment.Close()
	case <-ctx.Done():
		_ = p.forceStop()
	}
}

func (p *process) forceStop() error {
	if p.stopped.Swap(true) {
		return nil
	}
	_ = p.stdin.Close()
	_ = p.containment.Close()
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

func key(userID, value string) string { return userID + "\x00" + value }

func within(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

func shutdownDuration(milliseconds int) time.Duration {
	if milliseconds < 1000 || milliseconds > 30000 {
		milliseconds = 10000
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func backendShutdownMillis(release pluginforge.Release) int {
	if release.Manifest.Runtime == nil || release.Manifest.Runtime.Backend == nil {
		return 10000
	}
	return release.Manifest.Runtime.Backend.ShutdownMillis
}

func hasPermission(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
