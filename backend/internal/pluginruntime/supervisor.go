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
	plugins       map[string]*process
	capabilities  map[string]*binding
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
	return &Supervisor{workspaceRoot: root, plugins: map[string]*process{}, capabilities: map[string]*binding{}}, nil
}

func (s *Supervisor) Activate(ctx context.Context, userID string, release pluginforge.Release) error {
	if err := release.Manifest.Validate(); err != nil {
		return err
	}
	candidate, err := startProcess(ctx, release)
	if err != nil {
		return err
	}

	pluginKey := key(userID, release.PluginID)
	newBindings := make(map[string]*binding, len(release.Manifest.Exports.Tools))
	for _, capability := range release.Manifest.Exports.Tools {
		capabilityKey := key(userID, capability.ID)
		newBindings[capabilityKey] = &binding{process: candidate, capability: pluginforge.CapabilityBinding{
			ToolExport: capability,
			PluginID:   release.PluginID,
			ReleaseID:  release.ID,
			Version:    release.Version,
		}}
	}

	s.mu.Lock()
	old := s.plugins[pluginKey]
	if old != nil {
		for capabilityKey, current := range s.capabilities {
			if current.process == old {
				delete(s.capabilities, capabilityKey)
			}
		}
	}
	s.plugins[pluginKey] = candidate
	for capabilityKey, next := range newBindings {
		s.capabilities[capabilityKey] = next
	}
	s.mu.Unlock()

	if old != nil {
		old.draining.Store(true)
		go old.stop(shutdownDuration(backendShutdownMillis(old.release)))
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
	delete(s.plugins, pluginKey)
	for capabilityKey, value := range s.capabilities {
		if value.process == current {
			delete(s.capabilities, capabilityKey)
		}
	}
	s.mu.Unlock()
	current.draining.Store(true)
	go current.stop(shutdownDuration(backendShutdownMillis(current.release)))
	return nil
}

func (s *Supervisor) Invoke(ctx context.Context, userID, capabilityID string, input json.RawMessage) (json.RawMessage, error) {
	s.mu.RLock()
	selected := s.capabilities[key(userID, capabilityID)]
	if selected != nil && !selected.process.draining.Load() {
		selected.process.inFlight.Add(1)
	} else {
		selected = nil
	}
	s.mu.RUnlock()
	if selected == nil {
		return nil, fmt.Errorf("capability %q is not active", capabilityID)
	}
	defer selected.process.inFlight.Done()

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

func (s *Supervisor) Close() error {
	s.mu.Lock()
	processes := make([]*process, 0, len(s.plugins))
	for _, current := range s.plugins {
		current.draining.Store(true)
		processes = append(processes, current)
	}
	s.plugins = map[string]*process{}
	s.capabilities = map[string]*binding{}
	s.mu.Unlock()
	for _, current := range processes {
		current.stop(shutdownDuration(backendShutdownMillis(current.release)))
	}
	return nil
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
