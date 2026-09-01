package pluginforge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

type Runtime interface {
	Activate(context.Context, string, Release) error
	Deactivate(context.Context, string, string) error
	Invoke(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	Capabilities(string) []CapabilityBinding
}

type Service struct {
	repo          *Repository
	runtime       Runtime
	dataDir       string
	workspaceRoot string
}

type CreateInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type SourceFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func NewService(repo *Repository, runtime Runtime, dataDir, workspaceRoot string) *Service {
	return &Service{repo: repo, runtime: runtime, dataDir: dataDir, workspaceRoot: workspaceRoot}
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

func (s *Service) Create(ctx context.Context, userID string, in CreateInput) (Project, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
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
	spec, _ := json.Marshal(map[string]any{"goal": in.Description, "acceptance": []string{"backend compiles and passes tests", "frontend loads in a sandbox", "capability output matches its schema", "permissions remain workspace-readonly"}, "requestedBy": "user"})
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
	p, err := s.repo.Transition(ctx, userID, projectID, StateGenerating, "")
	if err != nil {
		return Project{}, err
	}
	manifest := referenceManifest(p)
	if err = writeProject(p, manifest); err != nil {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateGenerationFailed, err.Error())
		return failed, err
	}
	commitGeneratedProject(p.SourceDir)
	p, err = s.repo.Transition(ctx, userID, projectID, StateGenerated, "")
	if err == nil {
		s.audit(ctx, p, "project.generated", map[string]any{"sourceDir": p.SourceDir})
	}
	return p, err
}

func (s *Service) WriteSourceFile(ctx context.Context, userID, projectID string, in SourceFileInput) (Project, error) {
	project, err := s.repo.Project(ctx, userID, projectID)
	if err != nil {
		return Project{}, err
	}
	if project.State != StateGenerated && project.State != StateBuildFailed {
		return Project{}, errors.New("source can only be edited before a successful release build")
	}
	if len(in.Content) > 512<<10 {
		return Project{}, errors.New("plugin source file exceeds 512 KiB")
	}
	relative := filepath.Clean(filepath.FromSlash(strings.TrimSpace(in.Path)))
	if !allowedSourcePath(relative) {
		return Project{}, errors.New("source path is outside the plugin contract")
	}
	root, err := filepath.Abs(project.SourceDir)
	if err != nil {
		return Project{}, err
	}
	target, err := filepath.Abs(filepath.Join(root, relative))
	if err != nil || !pathWithin(root, target) {
		return Project{}, errors.New("source path escapes the plugin workspace")
	}
	if err = os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return Project{}, err
	}
	if err = os.WriteFile(target, []byte(in.Content), 0o600); err != nil {
		return Project{}, err
	}
	commitSourceChange(project.SourceDir, filepath.ToSlash(relative))
	s.audit(ctx, project, "source.updated", map[string]any{"path": filepath.ToSlash(relative), "bytes": len(in.Content)})
	return project, nil
}

func (s *Service) BuildAndTest(ctx context.Context, userID, projectID string) (Project, Release, error) {
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
	var manifest Manifest
	if err = json.Unmarshal(manifestRaw, &manifest); err != nil {
		return fail(err)
	}
	if err = manifest.Validate(); err != nil {
		return fail(err)
	}
	goExe, err := findGo()
	if err != nil {
		return fail(err)
	}
	backendDir := filepath.Join(p.SourceDir, "backend")
	if err = validateBackendPolicy(backendDir); err != nil {
		return fail(err)
	}
	testStarted := time.Now()
	testOutput, err := runCommand(ctx, backendDir, goExe, "test", "./...")
	if err != nil {
		return fail(fmt.Errorf("plugin tests failed: %s", testOutput))
	}
	buildDir := filepath.Join(p.SourceDir, "build", time.Now().UTC().Format("20060102T150405.000000000"))
	if err = os.MkdirAll(buildDir, 0o700); err != nil {
		return fail(err)
	}
	exeName := "plugin"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	builtExe := filepath.Join(buildDir, exeName)
	buildOutput, err := runCommand(ctx, backendDir, goExe, "build", "-trimpath", "-o", builtExe, ".")
	if err != nil {
		return fail(fmt.Errorf("plugin build failed: %s", buildOutput))
	}
	frontendSource := filepath.Join(p.SourceDir, "frontend", "index.html")
	digest, err := artifactDigest(manifestRaw, builtExe, frontendSource)
	if err != nil {
		return fail(err)
	}
	bundleDir := filepath.Join(s.dataDir, "plugin-store", "sha256", digest)
	if err = packageRelease(bundleDir, builtExe, frontendSource, manifestRaw); err != nil {
		return fail(err)
	}
	report, _ := json.Marshal(map[string]any{"passed": true, "goTest": strings.TrimSpace(testOutput), "build": strings.TrimSpace(buildOutput), "durationMillis": time.Since(testStarted).Milliseconds(), "checkedAt": time.Now().UTC()})
	release := Release{ID: "rel_" + digest[:24], ProjectID: p.ID, PluginID: manifest.ID, Version: manifest.Version, Digest: digest, BundleDir: bundleDir, Manifest: manifest, TestReport: report, PermissionHash: PermissionHash(manifest.Permissions), CreatedAt: time.Now().UTC()}
	if err = s.repo.CreateRelease(ctx, release); err != nil && !strings.Contains(strings.ToLower(err.Error()), "unique") {
		return fail(err)
	}
	p, err = s.repo.Transition(ctx, userID, projectID, StateTested, "")
	if err != nil {
		return p, release, err
	}
	s.audit(ctx, p, "release.tested", map[string]any{"releaseId": release.ID, "digest": digest})
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
	if err = s.runtime.Activate(ctx, userID, release); err != nil {
		failed, _ := s.repo.Transition(ctx, userID, projectID, StateActivationFailed, err.Error())
		return failed, Installation{}, err
	}
	if p.State == StateApproved {
		p, err = s.repo.Transition(ctx, userID, projectID, StateInstalled, "")
		if err != nil {
			return p, Installation{}, err
		}
	}
	installation, err := s.repo.Activate(ctx, userID, release)
	if err != nil {
		return p, Installation{}, err
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
	if err = s.runtime.Activate(ctx, userID, release); err != nil {
		return Project{}, Installation{}, err
	}
	installation, err := s.repo.Activate(ctx, userID, release)
	if err != nil {
		return Project{}, Installation{}, err
	}
	s.audit(ctx, project, "plugin.rolled_back", map[string]any{"releaseId": release.ID})
	return project, installation, nil
}

func (s *Service) Invoke(ctx context.Context, userID, capabilityID string, input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return s.runtime.Invoke(ctx, userID, capabilityID, input)
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
	for _, i := range installations {
		if i.ActiveReleaseID == releaseID && i.Status == "active" {
			allowed = true
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
	clean := filepath.Clean(filepath.FromSlash(asset))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", domain.ErrInvalid
	}
	path := filepath.Join(release.BundleDir, "frontend", clean)
	root, rootErr := filepath.Abs(filepath.Join(release.BundleDir, "frontend"))
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
	if path == "plugin.json" {
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
