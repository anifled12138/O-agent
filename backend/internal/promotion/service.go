package promotion

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/capsule"
	"axiom.local/agent/internal/pluginforge"
)

type Status string

const (
	StatusQueued             Status = "queued"
	StatusVerifying          Status = "verifying"
	StatusGenerating         Status = "generating_reference"
	StatusBuilding           Status = "building_reference"
	StatusAwaitingApproval   Status = "awaiting_user_approval"
	StatusVerificationFailed Status = "verification_failed"
	StatusNeedsRepair        Status = "needs_repair"
	StatusFailed             Status = "failed"
)

type Job struct {
	ID            string                      `json:"id"`
	UserID        string                      `json:"userId"`
	CapsuleID     string                      `json:"capsuleId"`
	CapsuleDigest string                      `json:"capsuleDigest"`
	Status        Status                      `json:"status"`
	Verification  *capsule.VerificationReport `json:"verification,omitempty"`
	ProjectID     string                      `json:"projectId,omitempty"`
	ReleaseID     string                      `json:"releaseId,omitempty"`
	Error         string                      `json:"error,omitempty"`
	Attempt       int                         `json:"attempt"`
	CreatedAt     time.Time                   `json:"createdAt"`
	UpdatedAt     time.Time                   `json:"updatedAt"`
}

type Service struct {
	hostCtx  context.Context
	root     string
	capsules *capsule.Repository
	forge    *pluginforge.Service
	mu       sync.RWMutex
	jobs     map[string]Job
	running  map[string]bool
}

func Open(hostCtx context.Context, dataDir string, capsules *capsule.Repository, forge *pluginforge.Service) (*Service, error) {
	root, err := filepath.Abs(filepath.Join(dataDir, "capability-promotions"))
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(root, "jobs"), 0o700); err != nil {
		return nil, err
	}
	service := &Service{hostCtx: hostCtx, root: root, capsules: capsules, forge: forge, jobs: map[string]Job{}, running: map[string]bool{}}
	if err = service.load(); err != nil {
		return nil, err
	}
	for _, job := range service.List("") {
		if job.Status == StatusQueued || job.Status == StatusVerifying || job.Status == StatusGenerating || job.Status == StatusBuilding {
			service.start(job.ID)
		}
	}
	return service, nil
}

func (s *Service) Request(userID, capsuleID string) (Job, error) {
	manifest, err := s.capsules.Get(strings.TrimSpace(capsuleID))
	if err != nil {
		return Job{}, err
	}
	s.mu.Lock()
	for _, existing := range s.jobs {
		if existing.UserID == userID && existing.CapsuleDigest == manifest.Digest && existing.Status != StatusFailed && existing.Status != StatusVerificationFailed {
			s.mu.Unlock()
			return existing, nil
		}
	}
	now := time.Now().UTC()
	job := Job{ID: newID("promotion"), UserID: userID, CapsuleID: manifest.ID, CapsuleDigest: manifest.Digest, Status: StatusQueued, CreatedAt: now, UpdatedAt: now}
	if err = s.persist(job); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	s.jobs[job.ID] = job
	s.mu.Unlock()
	s.start(job.ID)
	return job, nil
}

func (s *Service) Retry(userID, id string) (Job, error) {
	s.mu.Lock()
	job, ok := s.jobs[id]
	if !ok || job.UserID != userID {
		s.mu.Unlock()
		return Job{}, errors.New("promotion job not found")
	}
	if job.Status != StatusNeedsRepair && job.Status != StatusFailed && job.Status != StatusVerificationFailed {
		s.mu.Unlock()
		return Job{}, errors.New("promotion job is not retryable")
	}
	job.Status, job.Error, job.UpdatedAt = StatusQueued, "", time.Now().UTC()
	if err := s.persist(job); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	s.jobs[id] = job
	s.mu.Unlock()
	s.start(id)
	return job, nil
}

func (s *Service) List(userID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		if userID == "" || job.UserID == userID {
			result = append(result, job)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result
}

func (s *Service) start(id string) {
	s.mu.Lock()
	if s.running[id] {
		s.mu.Unlock()
		return
	}
	s.running[id] = true
	s.mu.Unlock()
	go func() {
		defer func() { s.mu.Lock(); delete(s.running, id); s.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(s.hostCtx, 15*time.Minute)
		defer cancel()
		s.run(ctx, id)
	}()
}

func (s *Service) run(ctx context.Context, id string) {
	job, ok := s.job(id)
	if !ok {
		return
	}
	manifest, err := s.capsules.Get(job.CapsuleID)
	if err != nil || manifest.Digest != job.CapsuleDigest {
		s.fail(id, StatusFailed, "the pinned capsule revision is unavailable")
		return
	}
	job = s.update(id, func(next *Job) { next.Status = StatusVerifying; next.Attempt++; next.Error = "" })
	report := s.capsules.Verify(ctx, job.CapsuleID)
	job = s.update(id, func(next *Job) { next.Verification = &report })
	if !report.Passed {
		s.fail(id, StatusVerificationFailed, report.Failure)
		return
	}
	project, err := s.project(ctx, job)
	if err != nil {
		s.fail(id, StatusFailed, err.Error())
		return
	}
	if project.ID == "" {
		job = s.update(id, func(next *Job) { next.Status = StatusGenerating })
		project, err = s.forge.Create(ctx, job.UserID, pluginforge.CreateInput{
			Name: "Capsule · " + manifest.Contract.Name, Shape: pluginforge.ShapeAgentTool,
			Description: fmt.Sprintf("Verified Capsule %s at %s. Intent: %s. Fallback: %s. Preserve %d pinned replay cases and request no host permissions.", manifest.ID, manifest.Digest, manifest.Contract.Intent, manifest.Contract.Fallback, len(manifest.Evidence)),
		})
		if err == nil {
			job = s.update(id, func(next *Job) { next.ProjectID = project.ID })
			project, err = s.forge.GenerateFromCapsule(ctx, job.UserID, project.ID, manifest)
		}
		if err != nil {
			s.fail(id, StatusNeedsRepair, err.Error())
			return
		}
	}
	if project.State == pluginforge.StateProposed || project.State == pluginforge.StateGenerationFailed {
		job = s.update(id, func(next *Job) { next.Status = StatusGenerating })
		project, err = s.forge.GenerateFromCapsule(ctx, job.UserID, project.ID, manifest)
		if err != nil {
			s.fail(id, StatusNeedsRepair, err.Error())
			return
		}
	}
	if project.State == pluginforge.StateGenerated || project.State == pluginforge.StateBuildFailed {
		job = s.update(id, func(next *Job) { next.Status = StatusBuilding })
		var release pluginforge.Release
		project, release, err = s.forge.BuildAndTest(ctx, job.UserID, project.ID)
		if err != nil {
			s.fail(id, StatusNeedsRepair, err.Error())
			return
		}
		job = s.update(id, func(next *Job) { next.ReleaseID = release.ID })
	}
	if project.State == pluginforge.StateTested {
		project, _, err = s.forge.RequestApproval(ctx, job.UserID, project.ID)
		if err != nil {
			s.fail(id, StatusFailed, err.Error())
			return
		}
	}
	if project.State != pluginforge.StateAwaitingApproval {
		s.fail(id, StatusFailed, "forge candidate stopped in unexpected state "+string(project.State))
		return
	}
	s.update(id, func(next *Job) { next.Status = StatusAwaitingApproval; next.Error = "" })
}

func (s *Service) project(ctx context.Context, job Job) (pluginforge.Project, error) {
	if job.ProjectID == "" {
		return pluginforge.Project{}, nil
	}
	projects, err := s.forge.ListProjects(ctx, job.UserID)
	if err != nil {
		return pluginforge.Project{}, err
	}
	for _, project := range projects {
		if project.ID == job.ProjectID {
			return project.Project, nil
		}
	}
	return pluginforge.Project{}, errors.New("forge project is unavailable")
}

func (s *Service) job(id string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *Service) update(id string, mutate func(*Job)) Job {
	s.mu.Lock()
	job := s.jobs[id]
	mutate(&job)
	job.UpdatedAt = time.Now().UTC()
	if err := s.persist(job); err != nil {
		job.Status = StatusFailed
		job.Error = "persist promotion state: " + err.Error()
		job.UpdatedAt = time.Now().UTC()
	}
	s.jobs[id] = job
	s.mu.Unlock()
	return job
}

func (s *Service) fail(id string, status Status, message string) {
	s.update(id, func(job *Job) { job.Status = status; job.Error = strings.TrimSpace(message) })
}

func (s *Service) persist(job Job) error {
	directory := filepath.Join(s.root, "jobs", job.ID, "events")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(job, "", "  ")
	name := time.Now().UTC().Format("20060102T150405.000000000") + "-" + string(job.Status) + "-" + newID("event") + ".json"
	return os.WriteFile(filepath.Join(directory, name), raw, 0o600)
}

func (s *Service) load() error {
	root := filepath.Join(s.root, "jobs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "promotion_") {
			continue
		}
		events, readErr := os.ReadDir(filepath.Join(root, entry.Name(), "events"))
		if readErr != nil || len(events) == 0 {
			continue
		}
		names := make([]string, 0, len(events))
		for _, event := range events {
			if !event.IsDir() && strings.HasSuffix(event.Name(), ".json") {
				names = append(names, event.Name())
			}
		}
		if len(names) == 0 {
			continue
		}
		sort.Strings(names)
		raw, readErr := os.ReadFile(filepath.Join(root, entry.Name(), "events", names[len(names)-1]))
		var job Job
		if readErr != nil || json.Unmarshal(raw, &job) != nil || job.ID != entry.Name() {
			continue
		}
		s.jobs[job.ID] = job
	}
	return nil
}

func newID(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + "_" + hex.EncodeToString(raw)
}
