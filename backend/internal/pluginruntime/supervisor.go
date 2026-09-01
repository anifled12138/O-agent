package pluginruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	services      map[string]pluginforge.ServiceBinding
	skills        map[string]pluginforge.SkillBinding
	uis           map[string]pluginforge.UIBinding
	hooks         map[string]surfaceBinding
	jobs          map[string]surfaceBinding
}

type mountedRelease struct {
	release pluginforge.Release
	process *process
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
}

type process struct {
	release pluginforge.Release
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  bytes.Buffer

	callMu   sync.Mutex
	request  atomic.Uint64
	inFlight sync.WaitGroup
	draining atomic.Bool
	stopped  atomic.Bool
}

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
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("plugin workspace root is unavailable: %s", root)
	}
	return &Supervisor{
		workspaceRoot: root, plugins: map[string]*mountedRelease{}, capabilities: map[string]*binding{},
		services: map[string]pluginforge.ServiceBinding{}, skills: map[string]pluginforge.SkillBinding{},
		uis: map[string]pluginforge.UIBinding{}, hooks: map[string]surfaceBinding{}, jobs: map[string]surfaceBinding{},
	}, nil
}

func (s *Supervisor) Activate(ctx context.Context, userID string, release pluginforge.Release) error {
	if err := release.Manifest.Validate(); err != nil {
		return err
	}
	var candidate *process
	if release.Manifest.Runtime != nil && release.Manifest.Runtime.Backend != nil {
		var startErr error
		candidate, startErr = startProcess(ctx, release)
		if startErr != nil {
			return startErr
		}
	}

	pluginKey := key(userID, release.PluginID)
	mount := &mountedRelease{release: release, process: candidate}
	newBindings := make(map[string]*binding, len(release.Manifest.Exports.Tools))
	for _, capability := range release.Manifest.Exports.Tools {
		if capability.Executor.Kind != "backend" {
			if candidate != nil {
				_ = candidate.forceStop()
			}
			return fmt.Errorf("tool %q uses unsupported executor kind %q", capability.ID, capability.Executor.Kind)
		}
		capabilityKey := key(userID, capability.ID)
		newBindings[capabilityKey] = &binding{process: candidate, capability: pluginforge.CapabilityBinding{
			ToolExport: capability,
			PluginID:   release.PluginID,
			ReleaseID:  release.ID,
			Version:    release.Version,
		}}
	}
	newServices := make(map[string]pluginforge.ServiceBinding, len(release.Manifest.Exports.Services))
	for _, service := range release.Manifest.Exports.Services {
		newServices[key(userID, service.ID)] = pluginforge.ServiceBinding{PluginID: release.PluginID, ReleaseID: release.ID, Version: release.Version, Service: service}
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
	if conflict := s.registrationConflict(userID, release.PluginID, newBindings, newServices, newSkills, newHooks, newJobs); conflict != "" {
		s.mu.Unlock()
		if candidate != nil {
			_ = candidate.forceStop()
		}
		return errors.New(conflict)
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
	if selected != nil && (releaseID == "" || selected.capability.ReleaseID == releaseID) && !selected.process.draining.Load() {
		selected.process.inFlight.Add(1)
	} else {
		selected = nil
	}
	s.mu.RUnlock()
	if selected == nil {
		return nil, fmt.Errorf("capability %q is not active", capabilityID)
	}
	defer selected.process.inFlight.Done()
	return s.invokeBinding(ctx, selected, capabilityID, input)
}

func (s *Supervisor) invokeBinding(ctx context.Context, selected *binding, capabilityID string, input json.RawMessage) (json.RawMessage, error) {
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
	// The host, not plugin UI or model output, chooses the filesystem boundary.
	if hasPermission(selected.process.release.Manifest.Permissions.Filesystem.Read, "${workspace}") {
		params["root"] = s.workspaceRoot
	} else {
		delete(params, "root")
	}
	raw, _ := json.Marshal(params)
	result, err := selected.process.call(ctx, "capability.invoke", raw)
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
	s.mu.RLock()
	mounted := s.plugins[key(userID, pluginID)]
	s.mu.RUnlock()
	if mounted == nil {
		return nil
	}
	now := time.Now().UTC()
	result := make([]pluginforge.SurfaceState, 0)
	add := func(kind, id string) {
		result = append(result, pluginforge.SurfaceState{UserID: userID, PluginID: pluginID, ReleaseID: mounted.release.ID, Kind: kind, SurfaceID: id, Status: "active", UpdatedAt: now})
	}
	if mounted.release.Manifest.Runtime != nil {
		add("runtime", "backend")
	}
	if mounted.release.Manifest.UI != nil {
		add("ui", mounted.release.Manifest.UI.Entry)
	}
	for _, item := range mounted.release.Manifest.Exports.Services {
		add("service", item.ID)
	}
	for _, item := range mounted.release.Manifest.Exports.Tools {
		add("tool", item.ID)
	}
	for _, item := range mounted.release.Manifest.Exports.Skills {
		add("skill", item.ID)
	}
	for _, item := range mounted.release.Manifest.Exports.Hooks {
		add("hook", item.ID)
	}
	for _, item := range mounted.release.Manifest.Exports.Jobs {
		add("job", item.ID)
	}
	return result
}

func (s *Supervisor) UI(userID, pluginID string) (pluginforge.UIBinding, bool) {
	s.mu.RLock()
	value, ok := s.uis[key(userID, pluginID)]
	s.mu.RUnlock()
	return value, ok
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
		if !strings.HasPrefix(id, prefix) || item.process.draining.Load() {
			continue
		}
		lease.tools[item.capability.ID] = item
		if !seen[item.process] {
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
	s.services = map[string]pluginforge.ServiceBinding{}
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

func (s *Supervisor) registrationConflict(userID, pluginID string, tools map[string]*binding, services map[string]pluginforge.ServiceBinding, skills map[string]pluginforge.SkillBinding, hooks, jobs map[string]surfaceBinding) string {
	for id := range tools {
		if current := s.capabilities[id]; current != nil && current.capability.PluginID != pluginID {
			return fmt.Sprintf("tool export %q is already registered", strings.TrimPrefix(id, userID+"\x00"))
		}
	}
	for id := range services {
		if current, ok := s.services[id]; ok && current.PluginID != pluginID {
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
		if item.PluginID == pluginID && strings.HasPrefix(id, userID+"\x00") {
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

func startProcess(ctx context.Context, release pluginforge.Release) (*process, error) {
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
	current := &process{release: release, cmd: command, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 1<<20)}
	command.Stderr = &current.stderr
	if err = command.Start(); err != nil {
		return nil, err
	}
	hello, err := current.call(ctx, "plugin.hello", json.RawMessage(`{"host":"axiom","protocol":"axiom.rpc/v1"}`))
	if err == nil {
		var status struct {
			Protocol string `json:"protocol"`
			Status   string `json:"status"`
		}
		err = json.Unmarshal(hello, &status)
		if err == nil && (status.Protocol != "axiom.rpc/v1" || status.Status != "ready") {
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
	if _, err := p.stdin.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	type outcome struct {
		line []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		line, err := p.stdout.ReadBytes('\n')
		done <- outcome{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = p.forceStop()
		return nil, ctx.Err()
	case received := <-done:
		if received.err != nil {
			return nil, received.err
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

func (p *process) stop(timeout time.Duration) {
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
	finished := make(chan error, 1)
	go func() { finished <- p.cmd.Wait() }()
	select {
	case <-finished:
		p.stopped.Store(true)
	case <-ctx.Done():
		_ = p.forceStop()
	}
}

func (p *process) forceStop() error {
	if p.stopped.Swap(true) {
		return nil
	}
	_ = p.stdin.Close()
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
