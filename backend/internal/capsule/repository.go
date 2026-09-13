package capsule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/capability"
)

type Repository struct {
	mu       sync.RWMutex
	root     string
	executor capability.ScriptExecutor
	active   map[string]Manifest
}

func Open(workspaceRoot string, executor capability.ScriptExecutor) (*Repository, error) {
	root, err := filepath.Abs(filepath.Join(workspaceRoot, ".axiom", "capsules"))
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	repository := &Repository{root: root, executor: executor, active: map[string]Manifest{}}
	if err = repository.reload(); err != nil {
		return nil, err
	}
	return repository, nil
}

func (r *Repository) Root() string { return r.root }

func (r *Repository) Save(manifest Manifest) (Manifest, error) {
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	pluginRoot := filepath.Join(r.root, manifest.ID)
	revisionRoot := filepath.Join(pluginRoot, "revisions", manifest.Digest)
	if err := ensureWithin(r.root, revisionRoot); err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(revisionRoot, 0o700); err != nil {
		return Manifest{}, err
	}
	manifestPath := filepath.Join(revisionRoot, "capsule.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		raw, _ := json.MarshalIndent(manifest, "", "  ")
		if err = os.WriteFile(manifestPath, raw, 0o600); err != nil {
			return Manifest{}, err
		}
	} else if err != nil {
		return Manifest{}, err
	}
	activationRoot := filepath.Join(pluginRoot, "activations")
	if err := os.MkdirAll(activationRoot, 0o700); err != nil {
		return Manifest{}, err
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	activation := map[string]any{"digest": manifest.Digest, "activatedAt": time.Now().UTC()}
	raw, _ := json.Marshal(activation)
	if err := os.WriteFile(filepath.Join(activationRoot, stamp+"-"+manifest.Digest[:12]+".json"), raw, 0o600); err != nil {
		return Manifest{}, err
	}
	r.mu.Lock()
	r.active[manifest.ID] = manifest
	r.mu.Unlock()
	return manifest, nil
}

func (r *Repository) Get(id string) (Manifest, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	manifest, ok := r.active[id]
	if !ok {
		return Manifest{}, errors.New("capsule not found")
	}
	return cloneManifest(manifest), nil
}

func (r *Repository) List() []Summary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Summary, 0, len(r.active))
	for _, manifest := range r.active {
		result = append(result, manifest.Summary())
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func (r *Repository) Invoke(ctx context.Context, id string, input json.RawMessage) (json.RawMessage, time.Duration, error) {
	manifest, err := r.Get(id)
	if err != nil {
		return nil, 0, err
	}
	if err = capability.ValidateValue(input, manifest.Contract.InputSchema); err != nil {
		return nil, 0, fmt.Errorf("capsule input: %w", err)
	}
	output, duration, err := r.executor.Execute(ctx, manifest.Program, input, manifest.Contract.Limits)
	if err != nil {
		return nil, duration, fmt.Errorf("%s: %w", manifest.Contract.Fallback, err)
	}
	if err = capability.ValidateValue(output, manifest.Contract.OutputSchema); err != nil {
		return nil, duration, fmt.Errorf("%s: output contract: %w", manifest.Contract.Fallback, err)
	}
	return output, duration, nil
}

func (r *Repository) Verify(ctx context.Context, id string) VerificationReport {
	started := time.Now()
	report := VerificationReport{CapsuleID: id, VerifiedAt: time.Now().UTC()}
	manifest, err := r.Get(id)
	if err != nil {
		report.Failure = err.Error()
		return report
	}
	report.Digest = manifest.Digest
	report.Cases = len(manifest.Evidence)
	for _, evidence := range manifest.Evidence {
		output, _, invokeErr := r.Invoke(ctx, id, evidence.Input)
		if invokeErr != nil {
			report.Failure = "case " + evidence.ID + ": " + invokeErr.Error()
			break
		}
		actual := capability.NewEvidence("actual", evidence.Input, output, 0)
		if actual.OutputDigest != evidence.OutputDigest {
			report.Failure = "case " + evidence.ID + ": output digest changed"
			break
		}
		report.PassedCases++
	}
	report.Passed = report.Cases > 0 && report.PassedCases == report.Cases
	report.DurationMillis = time.Since(started).Milliseconds()
	return report
}

func (r *Repository) reload() error {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return err
	}
	loaded := map[string]Manifest{}
	for _, entry := range entries {
		if !entry.IsDir() || !capsuleID.MatchString(entry.Name()) {
			continue
		}
		manifest, loadErr := r.loadActive(entry.Name())
		if loadErr != nil {
			return fmt.Errorf("load capsule %s: %w", entry.Name(), loadErr)
		}
		loaded[manifest.ID] = manifest
	}
	r.mu.Lock()
	r.active = loaded
	r.mu.Unlock()
	return nil
}

func (r *Repository) loadActive(id string) (Manifest, error) {
	activationRoot := filepath.Join(r.root, id, "activations")
	entries, err := os.ReadDir(activationRoot)
	if err != nil {
		return Manifest{}, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return Manifest{}, errors.New("capsule has no activation record")
	}
	sort.Strings(names)
	raw, err := os.ReadFile(filepath.Join(activationRoot, names[len(names)-1]))
	if err != nil {
		return Manifest{}, err
	}
	var activation struct {
		Digest string `json:"digest"`
	}
	if json.Unmarshal(raw, &activation) != nil || len(activation.Digest) != 64 {
		return Manifest{}, errors.New("invalid capsule activation record")
	}
	raw, err = os.ReadFile(filepath.Join(r.root, id, "revisions", activation.Digest, "capsule.json"))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, err
	}
	if err = manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ensureWithin(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("capsule path escapes repository")
	}
	return nil
}

func cloneManifest(manifest Manifest) Manifest {
	manifest.Contract.InputSchema = append(json.RawMessage(nil), manifest.Contract.InputSchema...)
	manifest.Contract.OutputSchema = append(json.RawMessage(nil), manifest.Contract.OutputSchema...)
	manifest.Evidence = append([]capability.EvidenceCase(nil), manifest.Evidence...)
	return manifest
}
