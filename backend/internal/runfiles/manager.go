package runfiles

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	markerName       = ".axiom-agent-run.json"
	markerVersion    = 1
	staleAfter       = 24 * time.Hour
	maxArtifactBytes = 32 << 20
)

type ownershipMarker struct {
	Version   int       `json:"version"`
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"createdAt"`
}

// Manager owns the dedicated temporary root used by Agent turns.
type Manager struct {
	root string
}

func (m *Manager) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}

// NewManager creates the private temporary root and removes only stale run
// directories carrying this package's ownership marker.
func NewManager(root, workspaceRoot string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("agent temp directory is empty")
	}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("resolve agent temp directory: %w", err)
	}
	workspace, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		return nil, fmt.Errorf("resolve workspace directory: %w", err)
	}
	if resolvedWorkspace, resolveErr := filepath.EvalSymlinks(workspace); resolveErr == nil {
		workspace = resolvedWorkspace
	} else if !errors.Is(resolveErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("resolve workspace directory: %w", resolveErr)
	}
	prospectiveRoot, err := resolveProspectivePath(absRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve agent temp directory: %w", err)
	}
	if pathWithin(workspace, prospectiveRoot) {
		return nil, fmt.Errorf("agent temp directory must be outside the workspace")
	}
	if err := os.MkdirAll(absRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create agent temp directory: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve agent temp directory: %w", err)
	}
	if pathWithin(workspace, resolvedRoot) {
		return nil, fmt.Errorf("agent temp directory must be outside the workspace")
	}
	if err := os.Chmod(resolvedRoot, 0o700); err != nil {
		return nil, fmt.Errorf("restrict agent temp directory permissions: %w", err)
	}
	m := &Manager{root: resolvedRoot}
	if err := m.cleanupStale(time.Now()); err != nil {
		return nil, err
	}
	return m, nil
}

func resolveProspectivePath(path string) (string, error) {
	current := path
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Abs(filepath.Clean(resolved))
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent directory")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if relative == "." {
		return true
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (m *Manager) NewScope() (*Scope, error) {
	if m == nil || m.root == "" {
		return nil, fmt.Errorf("agent temp manager is unavailable")
	}
	if err := m.cleanupStale(time.Now()); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(m.root, "axiom-run-")
	if err != nil {
		return nil, fmt.Errorf("create agent run directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("restrict agent run directory permissions: %w", err), os.RemoveAll(dir))
	}
	markerData, err := json.Marshal(ownershipMarker{Version: markerVersion, PID: os.Getpid(), CreatedAt: time.Now().UTC()})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("encode agent run ownership marker: %w", err), os.RemoveAll(dir))
	}
	marker := filepath.Join(dir, markerName)
	if err := os.WriteFile(marker, markerData, 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("mark agent run directory: %w", err), os.RemoveAll(dir))
	}
	return &Scope{dir: dir, artifacts: map[string]string{}}, nil
}

// NewExecutionDir allocates a private scratch directory for one command inside
// a run scope. Agent artifacts and the run ownership marker remain in the
// parent scope and are not granted to the child process.
func (s *Scope) NewExecutionDir() (string, error) {
	if s == nil {
		return "", fmt.Errorf("agent run scope is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", fmt.Errorf("agent run scope is closed")
	}
	dir, err := os.MkdirTemp(s.dir, "exec-")
	if err != nil {
		return "", fmt.Errorf("create agent command scratch directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", errors.Join(fmt.Errorf("restrict agent command scratch directory permissions: %w", err), os.RemoveAll(dir))
	}
	return dir, nil
}

// NewSandboxJournalPath allocates a durable recovery-record path alongside
// run scopes. It remains outside Scope.Close cleanup so an interrupted host
// can restore process ACL changes before the next run.
func (s *Scope) NewSandboxJournalPath() (string, error) {
	if s == nil {
		return "", fmt.Errorf("agent run scope is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", fmt.Errorf("agent run scope is closed")
	}
	id, err := randomID()
	if err != nil {
		return "", fmt.Errorf("generate sandbox journal ID: %w", err)
	}
	return filepath.Join(filepath.Dir(s.dir), ".axiom-sandbox-"+id+".json"), nil
}

func (m *Manager) cleanupStale(now time.Time) error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return fmt.Errorf("inspect agent temp directory: %w", err)
	}
	cutoff := now.Add(-staleAfter)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "axiom-run-") {
			continue
		}
		path := filepath.Join(m.root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspect old agent run %q: %w", entry.Name(), err)
		}
		if !info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		markerInfo, err := os.Lstat(filepath.Join(path, markerName))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspect ownership marker for %q: %w", entry.Name(), err)
		}
		if !markerInfo.Mode().IsRegular() {
			continue
		}
		markerData, err := os.ReadFile(filepath.Join(path, markerName))
		if err != nil {
			return fmt.Errorf("read ownership marker for %q: %w", entry.Name(), err)
		}
		var marker ownershipMarker
		if err := json.Unmarshal(markerData, &marker); err != nil || marker.Version != markerVersion || marker.PID <= 0 {
			continue
		}
		alive, err := processAlive(marker.PID)
		if err != nil {
			return fmt.Errorf("check owner process for %q: %w", entry.Name(), err)
		}
		if marker.CreatedAt.After(cutoff) || alive {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove stale agent run %q: %w", entry.Name(), err)
		}
	}
	return nil
}

// Scope owns files created for one turn. Artifact IDs are opaque and valid only
// while this scope remains open.
type Scope struct {
	mu        sync.RWMutex
	dir       string
	artifacts map[string]string
	closed    bool
	cleaned   bool
}

type Artifact struct {
	ID   string
	Path string
}

func (s *Scope) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *Scope) WriteArtifact(prefix, extension string, data []byte) (Artifact, error) {
	if s == nil {
		return Artifact{}, fmt.Errorf("agent run scope is unavailable")
	}
	if len(data) > maxArtifactBytes {
		return Artifact{}, fmt.Errorf("agent artifact exceeds the %d MiB limit", maxArtifactBytes>>20)
	}
	if prefix == "" {
		prefix = "artifact"
	}
	prefix = safeName(prefix)
	extension = strings.TrimPrefix(safeName(extension), ".")
	for attempt := 0; attempt < 4; attempt++ {
		id, err := randomID()
		if err != nil {
			return Artifact{}, fmt.Errorf("generate agent artifact ID: %w", err)
		}
		name := prefix + "-" + id
		if extension != "" {
			name += "." + extension
		}
		path := filepath.Join(s.dir, name)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return Artifact{}, fmt.Errorf("agent run scope is closed")
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			written, writeErr := file.Write(data)
			if writeErr == nil && written != len(data) {
				writeErr = io.ErrShortWrite
			}
			closeErr := file.Close()
			if writeErr == nil && closeErr == nil {
				s.artifacts[id] = path
				s.mu.Unlock()
				return Artifact{ID: id, Path: path}, nil
			}
			removeErr := os.Remove(path)
			s.mu.Unlock()
			return Artifact{}, errors.Join(fmt.Errorf("write agent artifact: %w", errors.Join(writeErr, closeErr)), removeErr)
		}
		s.mu.Unlock()
		if !errors.Is(err, fs.ErrExist) {
			return Artifact{}, fmt.Errorf("create agent artifact: %w", err)
		}
	}
	return Artifact{}, fmt.Errorf("could not allocate a unique agent artifact name")
}

func (s *Scope) ReadArtifact(id string) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("agent run scope is unavailable")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	path, ok := s.artifacts[id]
	closed := s.closed
	if closed {
		return nil, fmt.Errorf("agent run scope is closed")
	}
	if !ok {
		return nil, fmt.Errorf("unknown artifact ID")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve agent artifact: %w", err)
	}
	if !pathWithin(s.dir, resolved) {
		return nil, fmt.Errorf("agent artifact resolves outside its run directory")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("inspect agent artifact: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxArtifactBytes {
		return nil, fmt.Errorf("agent artifact is not a regular file within the size limit")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read agent artifact: %w", err)
	}
	return data, nil
}

func (s *Scope) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.cleaned {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	dir := s.dir
	s.artifacts = nil
	if err := os.RemoveAll(dir); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("remove agent run directory: %w", err)
	}
	if _, err := os.Lstat(dir); err == nil {
		s.mu.Unlock()
		return fmt.Errorf("agent run directory still exists after cleanup")
	} else if !errors.Is(err, fs.ErrNotExist) {
		s.mu.Unlock()
		return fmt.Errorf("verify agent run cleanup: %w", err)
	}
	s.cleaned = true
	s.mu.Unlock()
	return nil
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func safeName(value string) string {
	value = strings.TrimSpace(value)
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out.WriteRune(r)
		}
	}
	return out.String()
}
