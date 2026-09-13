package pluginforge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/pluginmanifest"
)

type Runtime interface {
	Activate(context.Context, string, Release) error
	Deactivate(context.Context, string, string) error
	Invoke(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	InvokePinned(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error)
	UICall(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error)
	UIServiceCall(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error)
	Capabilities(string) []CapabilityBinding
	SurfaceStates(string, string) []SurfaceState
	UI(string, string) (UIBinding, bool)
	UIs(string) []UIBinding
	Skills(string) []SkillBinding
	BeginTurn(string) TurnLease
	SetObserver(func(RuntimeEvent))
}

type Service struct {
	repo          *Repository
	runtime       Runtime
	dataDir       string
	workspaceRoot string
	sourceLocks   sync.Map
}

type CreateInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Shape       string `json:"shape"`
}

type SourceFileInput struct {
	Path             string `json:"path"`
	Content          string `json:"content"`
	ExpectedRevision string `json:"expectedRevision"`
}

func NewService(repo *Repository, runtime Runtime, dataDir, workspaceRoot string) *Service {
	service := &Service{repo: repo, runtime: runtime, dataDir: dataDir, workspaceRoot: workspaceRoot}
	runtime.SetObserver(service.handleRuntimeEvent)
	return service
}

func (s *Service) lockSourceProject(projectID string) func() {
	value, _ := s.sourceLocks.LoadOrStore(projectID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (s *Service) handleRuntimeEvent(event RuntimeEvent) {
	_ = s.repo.MarkRuntimeFailure(context.Background(), event.UserID, event.PluginID, event.ReleaseID, event.Error)
	raw, _ := json.Marshal(map[string]any{"releaseId": event.ReleaseID, "kind": event.Kind, "error": event.Error})
	_ = s.repo.Audit(context.Background(), AuditEvent{ID: newID("evt"), UserID: event.UserID, PluginID: event.PluginID, Action: "runtime.crashed", Details: raw, CreatedAt: event.At})
}

func (s *Service) ListProjects(ctx context.Context, userID string) ([]ProjectView, error) {
	projects, err := s.repo.ListProjects(ctx, userID)
	if err != nil {
		return nil, err
	}
	result := make([]ProjectView, 0, len(projects))
	for _, project := range projects {
		releases, releaseErr := s.repo.Releases(ctx, project.ID)
		if releaseErr != nil {
			return nil, releaseErr
		}
		view := ProjectView{Project: project, Releases: releases}
		if len(releases) > 0 {
			view.LatestRelease = &releases[0]
		}
		result = append(result, view)
	}
	return result, nil
}

func (s *Service) BeginRevision(ctx context.Context, userID, projectID string) (Project, error) {
	project, err := s.repo.Project(ctx, userID, projectID)
	if err != nil {
		return Project{}, err
	}
	if project.State != StateActive && project.State != StateInactive {
		return Project{}, errors.New("only an installed plugin can begin a new revision")
	}
	project, err = s.repo.Transition(ctx, userID, projectID, StateGenerated, "")
	if err == nil {
		s.audit(ctx, project, "revision.started", map[string]any{"activeReleasePreserved": true})
	}
	return project, err
}
func (s *Service) ListInstallations(ctx context.Context, userID string) ([]Installation, error) {
	return s.repo.ListInstallations(ctx, userID)
}
func (s *Service) Capabilities(userID string) []CapabilityBinding {
	return s.runtime.Capabilities(userID)
}
func (s *Service) SurfaceStates(ctx context.Context, userID string) ([]SurfaceState, error) {
	return s.repo.SurfaceStates(ctx, userID)
}
func (s *Service) MigrationStatus(ctx context.Context) (map[string]any, error) {
	activeV1, err := s.repo.ActiveV1Installations(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"generatedSpec":         pluginmanifest.SpecV2,
		"activeV1Installations": activeV1,
		"v1Compatibility":       true,
		"v1RouteRemovalReady":   activeV1 == 0,
		"v1CreationEnabled":     false,
	}, nil
}
func (s *Service) UIBinding(userID, pluginID string) (UIBinding, bool) {
	return s.runtime.UI(userID, pluginID)
}
func (s *Service) UIBindings(userID string) []UIBinding { return s.runtime.UIs(userID) }
func (s *Service) Skills(userID string) []SkillBinding  { return s.runtime.Skills(userID) }
func (s *Service) BeginTurn(userID string) TurnLease    { return s.runtime.BeginTurn(userID) }

func (s *Service) Create(ctx context.Context, userID string, in CreateInput) (Project, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.Shape = normalizeShape(in.Shape)
	if in.Name == "" || in.Description == "" {
		return Project{}, domain.ErrInvalid
	}
	id := newID("prj")
	slug := Slug(in.Name)
	if slug == "" {
		slug = "plugin-" + id[len(id)-8:]
	}
	if slug[0] < 'a' || slug[0] > 'z' {
		slug = "plugin-" + slug
	}
	spec, _ := json.Marshal(map[string]any{"goal": in.Description, "shape": in.Shape, "acceptance": acceptanceForShape(in.Shape), "requestedBy": "user"})
	now := time.Now().UTC()
	sourceDir := filepath.Join(s.dataDir, "plugin-workspaces", userID, slug)
	p := Project{ID: id, UserID: userID, Name: in.Name, Slug: slug, Description: in.Description, State: StateProposed, SourceDir: sourceDir, Spec: spec, CreatedAt: now, UpdatedAt: now}
	if err := s.repo.CreateProject(ctx, p); err != nil {
		return Project{}, err
	}
	s.audit(ctx, p, "project.proposed", nil)
	return p, nil
}

func (s *Service) Generate(ctx context.Context, userID, projectID string) (Project, error) {
	unlock := s.lockSourceProject(projectID)
	defer unlock()
	p, err := s.repo.Transition(ctx, userID, projectID, StateGenerating, "")
	if err != nil {
		return Project{}, err
	}
	shape := projectShape(p.Spec)
	manifest := referenceManifestV2(p, shape)
	if err = writeProjectV2(p, manifest, shape); err != nil {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateGenerationFailed, err.Error())
		return failed, err
	}
	if err = commitGeneratedProject(ctx, p.SourceDir); err != nil {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateGenerationFailed, err.Error())
		return failed, err
	}
	p, err = s.repo.Transition(ctx, userID, projectID, StateGenerated, "")
	if err == nil {
		s.audit(ctx, p, "project.generated", map[string]any{"sourceDir": p.SourceDir, "specVersion": pluginmanifest.SpecV2, "shape": shape})
	}
	return p, err
}

func (s *Service) WriteSourceFile(ctx context.Context, userID, projectID string, in SourceFileInput) (Project, error) {
	unlock := s.lockSourceProject(projectID)
	defer unlock()
	project, err := s.repo.Project(ctx, userID, projectID)
	if err != nil {
		return Project{}, err
	}
	if project.State != StateGenerated && project.State != StateBuildFailed {
		return Project{}, errors.New("source can only be edited before a successful release build")
	}
	if err = ensureSourceRepositoryClean(ctx, project.SourceDir); err != nil {
		return Project{}, err
	}
	currentRevision, err := gitText(ctx, project.SourceDir, "rev-parse", "HEAD")
	if err != nil {
		return Project{}, err
	}
	if strings.TrimSpace(in.ExpectedRevision) == "" || strings.TrimSpace(in.ExpectedRevision) != currentRevision {
		return Project{}, errors.New("source revision changed; inspect the source tree again before writing a file")
	}
	if len(in.Content) > maxSourceFileBytes {
		return Project{}, errors.New("plugin source file exceeds 512 KiB")
	}
	if strings.ContainsRune(in.Content, '\x00') {
		return Project{}, errors.New("plugin source must be text")
	}
	relative, target, err := resolveSourcePath(project.SourceDir, in.Path)
	if err != nil {
		return Project{}, err
	}
	if err = ensureNoSymlinkPath(project.SourceDir, target); err != nil {
		return Project{}, err
	}
	if err = os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return Project{}, err
	}
	if err = os.WriteFile(target, []byte(in.Content), 0o600); err != nil {
		return Project{}, err
	}
	if err = commitSourceChange(ctx, project.SourceDir, filepath.ToSlash(relative)); err != nil {
		return Project{}, err
	}
	s.audit(ctx, project, "source.updated", map[string]any{"path": filepath.ToSlash(relative), "bytes": len(in.Content)})
	return project, nil
}

func (s *Service) BuildAndTest(ctx context.Context, userID, projectID string) (Project, Release, error) {
	unlock := s.lockSourceProject(projectID)
	defer unlock()
	project, err := s.repo.Project(ctx, userID, projectID)
	if err != nil {
		return Project{}, Release{}, err
	}
	if err = ensureSourceRepositoryClean(ctx, project.SourceDir); err != nil {
		return project, Release{}, err
	}
	p, err := s.repo.Transition(ctx, userID, projectID, StateBuilding, "")
	if err != nil {
		return Project{}, Release{}, err
	}
	fail := func(buildErr error) (Project, Release, error) {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateBuildFailed, buildErr.Error())
		return failed, Release{}, buildErr
	}
	manifestRaw, err := os.ReadFile(filepath.Join(p.SourceDir, "plugin.json"))
	if err != nil {
		return fail(err)
	}
	document, err := pluginmanifest.Decode(manifestRaw)
	if err != nil {
		return fail(fmt.Errorf("manifest compatibility validation failed: %w", err))
	}
	result, err := s.executeBuildPlan(ctx, p, document, manifestRaw)
	if err != nil {
		return fail(err)
	}
	release := Release{
		ID:             "rel_" + result.digest[:24],
		ProjectID:      p.ID,
		PluginID:       document.Manifest.ID,
		Version:        document.Manifest.Version,
		Digest:         result.digest,
		BundleDir:      result.bundleDir,
		Manifest:       document.Manifest,
		TestReport:     result.report,
		PermissionHash: pluginmanifest.GrantDigest(document.Manifest),
		CreatedAt:      time.Now().UTC(),
		SourceVersion:  document.SourceVersion,
	}
	if err = s.repo.CreateRelease(ctx, release); err != nil && !strings.Contains(strings.ToLower(err.Error()), "unique") {
		return fail(err)
	}
	p, err = s.repo.Transition(ctx, userID, projectID, StateTested, "")
	if err != nil {
		return p, release, err
	}
	s.audit(ctx, p, "release.tested", map[string]any{"releaseId": release.ID, "digest": result.digest, "sourceVersion": document.SourceVersion})
	return p, release, nil
}

func (s *Service) RequestApproval(ctx context.Context, userID, projectID string) (Project, Release, error) {
	p, err := s.repo.Transition(ctx, userID, projectID, StateAwaitingApproval, "")
	if err != nil {
		return Project{}, Release{}, err
	}
	release, err := s.repo.LatestRelease(ctx, projectID)
	if err == nil {
		s.audit(ctx, p, "permission.requested", map[string]any{"permissionHash": release.PermissionHash, "permissions": release.Manifest.Permissions})
	}
	return p, release, err
}
func (s *Service) Approve(ctx context.Context, userID, projectID string) (Project, error) {
	p, err := s.repo.Project(ctx, userID, projectID)
	if err != nil {
		return Project{}, err
	}
	if p.State != StateAwaitingApproval {
		return Project{}, fmt.Errorf("project is not awaiting approval")
	}
	release, err := s.repo.LatestRelease(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if err = s.repo.Grant(ctx, userID, release); err != nil {
		return Project{}, err
	}
	p, err = s.repo.Transition(ctx, userID, projectID, StateApproved, "")
	if err == nil {
		s.audit(ctx, p, "permission.approved", map[string]any{"releaseId": release.ID, "permissionHash": release.PermissionHash})
	}
	return p, err
}

func (s *Service) Install(ctx context.Context, userID, projectID string) (Project, Installation, error) {
	p, err := s.repo.Project(ctx, userID, projectID)
	if err != nil {
		return Project{}, Installation{}, err
	}
	if p.State == StateActivationFailed {
		p, err = s.repo.Transition(ctx, userID, projectID, StateApproved, "")
		if err != nil {
			return Project{}, Installation{}, err
		}
	}
	if p.State != StateApproved && p.State != StateInactive {
		return Project{}, Installation{}, fmt.Errorf("project must be approved before installation")
	}
	release, err := s.repo.LatestRelease(ctx, projectID)
	if err != nil {
		return Project{}, Installation{}, err
	}
	granted, err := s.repo.HasGrant(ctx, userID, release)
	if err != nil || !granted {
		if err == nil {
			err = errors.New("release permissions have not been approved")
		}
		return Project{}, Installation{}, err
	}
	installation, err := s.activateRelease(ctx, userID, release)
	if err != nil {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateActivationFailed, err.Error())
		return failed, Installation{}, err
	}
	if p.State == StateApproved {
		p, err = s.repo.Transition(ctx, userID, projectID, StateInstalled, "")
		if err != nil {
			return p, Installation{}, err
		}
	}
	p, err = s.repo.Transition(ctx, userID, projectID, StateActive, "")
	if err == nil {
		s.audit(ctx, p, "plugin.activated", map[string]any{"releaseId": release.ID})
	}
	return p, installation, err
}

func (s *Service) Deactivate(ctx context.Context, userID, projectID string) (Project, error) {
	p, err := s.repo.Project(ctx, userID, projectID)
	if err != nil {
		return Project{}, err
	}
	release, err := s.repo.LatestRelease(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if err = s.runtime.Deactivate(ctx, userID, release.PluginID); err != nil {
		return Project{}, err
	}
	if err = s.repo.SetInstallationStatus(ctx, userID, release.PluginID, "inactive"); err != nil {
		return Project{}, err
	}
	p, err = s.repo.Transition(ctx, userID, projectID, StateInactive, "")
	if err == nil {
		s.audit(ctx, p, "plugin.deactivated", nil)
	}
	return p, err
}

func (s *Service) Rollback(ctx context.Context, userID, projectID, releaseID string) (Project, Installation, error) {
	project, err := s.repo.Project(ctx, userID, projectID)
	if err != nil {
		return Project{}, Installation{}, err
	}
	if project.State != StateActive {
		return Project{}, Installation{}, errors.New("rollback requires an active plugin")
	}
	release, err := s.repo.Release(ctx, releaseID)
	if err != nil || release.ProjectID != project.ID {
		if err == nil {
			err = domain.ErrNotFound
		}
		return Project{}, Installation{}, err
	}
	granted, err := s.repo.HasGrant(ctx, userID, release)
	if err != nil || !granted {
		if err == nil {
			err = errors.New("rollback release permissions are not approved")
		}
		return Project{}, Installation{}, err
	}
	installation, err := s.activateRelease(ctx, userID, release)
	if err != nil {
		return Project{}, Installation{}, err
	}
	s.audit(ctx, project, "plugin.rolled_back", map[string]any{"releaseId": release.ID})
	return project, installation, nil
}

// activateRelease coordinates the in-memory registry swap with the durable
// installation/surface transaction. If persistence fails, it restores the
// previous mounted release (or detaches the candidate) before returning.
func (s *Service) activateRelease(ctx context.Context, userID string, release Release) (Installation, error) {
	var previous *Release
	installations, listErr := s.repo.ListInstallations(ctx, userID)
	if listErr != nil {
		return Installation{}, listErr
	}
	for _, item := range installations {
		if item.PluginID == release.PluginID && item.Status == "active" {
			loaded, loadErr := s.repo.Release(ctx, item.ActiveReleaseID)
			if loadErr != nil {
				return Installation{}, loadErr
			}
			previous = &loaded
			break
		}
	}
	if err := s.runtime.Activate(ctx, userID, release); err != nil {
		return Installation{}, err
	}
	surfaces := s.runtime.SurfaceStates(userID, release.PluginID)
	installation, err := s.repo.ActivateWithSurfaces(ctx, userID, release, surfaces)
	if err == nil {
		return installation, nil
	}
	if previous != nil {
		_ = s.runtime.Activate(context.Background(), userID, *previous)
	} else {
		_ = s.runtime.Deactivate(context.Background(), userID, release.PluginID)
	}
	return Installation{}, fmt.Errorf("persist plugin activation: %w", err)
}

func (s *Service) Invoke(ctx context.Context, userID, capabilityID string, input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return s.runtime.Invoke(ctx, userID, capabilityID, input)
}

func (s *Service) InvokePinned(ctx context.Context, userID, capabilityID, releaseID string, input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return s.runtime.InvokePinned(ctx, userID, capabilityID, releaseID, input)
}

func (s *Service) LoadSkill(userID, skillID, releaseID string) (string, error) {
	for _, binding := range s.runtime.Skills(userID) {
		if binding.Skill.ID == skillID && binding.ReleaseID == releaseID {
			return s.LoadPinnedSkill(binding)
		}
	}
	return "", domain.ErrNotFound
}

func (s *Service) LoadPinnedSkill(binding SkillBinding) (string, error) {
	root, err := filepath.Abs(binding.BundleDir)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(binding.Skill.Entry)))
	if err != nil || !pathWithin(root, target) {
		return "", errors.New("skill entry escapes its release")
	}
	file, err := os.Open(target)
	if err != nil {
		return "", err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *Service) UICall(ctx context.Context, userID, pluginID, operation string, input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return s.runtime.UICall(ctx, userID, pluginID, operation, input)
}

func (s *Service) UIServiceCall(ctx context.Context, userID, pluginID, serviceID string, input json.RawMessage) (json.RawMessage, error) {
	if s.runtime == nil {
		return nil, errors.New("plugin runtime is unavailable")
	}
	return s.runtime.UIServiceCall(ctx, userID, pluginID, serviceID, input)
}

func (s *Service) LegacyUICall(ctx context.Context, userID, pluginID, suffix string, input json.RawMessage) (json.RawMessage, error) {
	ui, ok := s.runtime.UI(userID, pluginID)
	if !ok {
		return nil, domain.ErrNotFound
	}
	release, err := s.repo.Release(ctx, ui.ReleaseID)
	if err != nil {
		return nil, err
	}
	if release.SourceVersion != pluginmanifest.SourceV1 {
		return nil, errors.New("legacy UI invocation is restricted to V1 releases")
	}
	var selected string
	for _, capability := range s.runtime.Capabilities(userID) {
		if capability.PluginID == pluginID && strings.HasSuffix(capability.ID, suffix) {
			if selected != "" {
				return nil, errors.New("legacy capability suffix is ambiguous")
			}
			selected = capability.ID
		}
	}
	if selected == "" {
		return nil, errors.New("legacy capability is not active")
	}
	return s.runtime.InvokePinned(ctx, userID, selected, release.ID, input)
}

func (s *Service) Restore(ctx context.Context) error {
	installations, err := s.repo.ActiveInstallations(ctx)
	if err != nil {
		return err
	}
	var joined error
	for _, installation := range installations {
		release, loadErr := s.repo.Release(ctx, installation.ActiveReleaseID)
		if loadErr == nil {
			loadErr = s.runtime.Activate(ctx, installation.UserID, release)
			if loadErr == nil {
				_, loadErr = s.repo.ActivateWithSurfaces(ctx, installation.UserID, release, s.runtime.SurfaceStates(installation.UserID, release.PluginID))
			}
		}
		if loadErr != nil {
			joined = errors.Join(joined, fmt.Errorf("restore %s: %w", installation.PluginID, loadErr))
		}
	}
	return joined
}

func (s *Service) Asset(ctx context.Context, userID, releaseID, asset string) (string, error) {
	installations, err := s.repo.ListInstallations(ctx, userID)
	if err != nil {
		return "", err
	}
	allowed := false
	pluginID := ""
	for _, i := range installations {
		if i.ActiveReleaseID == releaseID && i.Status == "active" {
			allowed = true
			pluginID = i.PluginID
			break
		}
	}
	if !allowed {
		return "", domain.ErrUnauthorized
	}
	release, err := s.repo.Release(ctx, releaseID)
	if err != nil {
		return "", err
	}
	ui, ok := s.runtime.UI(userID, pluginID)
	if !ok || ui.ReleaseID != releaseID {
		return "", domain.ErrNotFound
	}
	clean := filepath.Clean(filepath.FromSlash(asset))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", domain.ErrInvalid
	}
	uiRoot := filepath.Join(release.BundleDir, filepath.Dir(filepath.FromSlash(ui.UI.Entry)))
	path := filepath.Join(uiRoot, clean)
	root, rootErr := filepath.Abs(uiRoot)
	absolute, absoluteErr := filepath.Abs(path)
	if rootErr != nil || absoluteErr != nil {
		return "", domain.ErrInvalid
	}
	relative, relativeErr := filepath.Rel(root, absolute)
	if relativeErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return "", domain.ErrInvalid
	}
	return absolute, nil
}

func (s *Service) audit(ctx context.Context, p Project, action string, details any) {
	raw, _ := json.Marshal(details)
	_ = s.repo.Audit(ctx, AuditEvent{ID: newID("evt"), UserID: p.UserID, ProjectID: p.ID, Action: action, Details: raw, CreatedAt: time.Now().UTC()})
}

func newID(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + "_" + hex.EncodeToString(raw)
}
func findGo() (string, error) {
	candidates := []string{"go"}
	if runtime.GOOS == "windows" {
		candidates = []string{`D:\DevTools\go\bin\go.exe`, "go.exe"}
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Go toolchain is unavailable")
}
func runCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	output, err := command.CombinedOutput()
	return string(output), err
}
func artifactDigest(manifest []byte, paths ...string) (string, error) {
	hash := sha256.New()
	hash.Write(manifest)
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		if _, err = io.Copy(hash, file); err != nil {
			file.Close()
			return "", err
		}
		file.Close()
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func packageRelease(bundleDir, builtExe, frontendSource string, manifest []byte) error {
	backendDir := filepath.Join(bundleDir, "backend")
	frontendDir := filepath.Join(bundleDir, "frontend")
	if err := os.MkdirAll(backendDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(frontendDir, 0o700); err != nil {
		return err
	}
	exeName := "plugin"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	if err := copyIfMissing(builtExe, filepath.Join(backendDir, exeName), 0o700); err != nil {
		return err
	}
	if err := copyIfMissing(frontendSource, filepath.Join(frontendDir, "index.html"), 0o600); err != nil {
		return err
	}
	path := filepath.Join(bundleDir, "manifest.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return os.WriteFile(path, manifest, 0o600)
	}
	return nil
}
func copyIfMissing(source, target string, mode os.FileMode) error {
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func allowedSourcePath(path string) bool {
	if path == "plugin.json" || path == ".gitignore" {
		return true
	}
	if filepath.IsAbs(path) || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(os.PathSeparator)) {
		return false
	}
	forward := filepath.ToSlash(path)
	extension := strings.ToLower(filepath.Ext(path))
	if strings.HasPrefix(forward, "backend/") {
		return extension == ".go" || extension == ".mod" || extension == ".sum"
	}
	if strings.HasPrefix(forward, "frontend/") {
		return extension == ".html" || extension == ".css" || extension == ".js" || extension == ".json"
	}
	if strings.HasPrefix(forward, "skills/") {
		return extension == ".md" || extension == ".json" || extension == ".txt"
	}
	return path == "README.md"
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

func validateBackendPolicy(backendDir string) error {
	set := token.NewFileSet()
	packages, err := parser.ParseDir(set, backendDir, func(info os.FileInfo) bool {
		return strings.HasSuffix(info.Name(), ".go")
	}, parser.ImportsOnly)
	if err != nil {
		return fmt.Errorf("parse plugin source: %w", err)
	}
	banned := map[string]string{
		"os/exec":  "starting child processes",
		"syscall":  "direct system calls",
		"unsafe":   "unsafe memory access",
		"plugin":   "loading native Go plugins",
		"net":      "direct network access",
		"net/http": "direct network access",
		"net/url":  "direct network access",
		"net/rpc":  "direct network access",
	}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, imported := range file.Imports {
				path, _ := strconv.Unquote(imported.Path.Value)
				if reason, blocked := banned[path]; blocked || strings.HasPrefix(path, "net/") {
					if !blocked {
						reason = "direct network access"
					}
					return fmt.Errorf("plugin policy rejects import %q (%s); use a host-brokered capability", path, reason)
				}
			}
		}
	}
	return nil
}

func validateBackendPolicyV2(backendDir string) error {
	if err := validateBackendPolicy(backendDir); err != nil {
		return err
	}
	set := token.NewFileSet()
	packages, err := parser.ParseDir(set, backendDir, func(info os.FileInfo) bool { return strings.HasSuffix(info.Name(), ".go") }, 0)
	if err != nil {
		return fmt.Errorf("parse plugin source: %w", err)
	}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, imported := range file.Imports {
				path, _ := strconv.Unquote(imported.Path.Value)
				if path == "path/filepath" {
					return errors.New("plugin policy rejects direct filesystem traversal; use host.fs brokers")
				}
			}
			var violation error
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				identifier, ok := selector.X.(*ast.Ident)
				if ok && identifier.Name == "os" && selector.Sel.Name != "Stdin" && selector.Sel.Name != "Stdout" {
					violation = fmt.Errorf("plugin policy rejects os.%s; use a Host resource broker", selector.Sel.Name)
					return false
				}
				return true
			})
			if violation != nil {
				return violation
			}
		}
	}
	return nil
}
