package pluginforge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"axiom.local/agent/internal/pluginmanifest"
)

type buildResult struct {
	digest    string
	bundleDir string
	report    json.RawMessage
}

type buildStep struct {
	Surface string `json:"surface"`
	Action  string `json:"action"`
	Passed  bool   `json:"passed"`
	Output  string `json:"output,omitempty"`
}

func (s *Service) executeBuildPlan(ctx context.Context, project Project, document pluginmanifest.Document, manifestRaw []byte) (buildResult, error) {
	if document.SourceVersion == pluginmanifest.SourceV1 {
		return s.executeLegacyBuild(ctx, project, document, manifestRaw)
	}
	return s.executeV2Build(ctx, project, document.Manifest)
}

func (s *Service) executeLegacyBuild(ctx context.Context, project Project, document pluginmanifest.Document, manifestRaw []byte) (buildResult, error) {
	var manifest Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return buildResult{}, err
	}
	if err := manifest.Validate(); err != nil {
		return buildResult{}, err
	}
	goExe, err := findGo()
	if err != nil {
		return buildResult{}, err
	}
	backendDir := filepath.Join(project.SourceDir, "backend")
	if err = validateBackendPolicy(backendDir); err != nil {
		return buildResult{}, err
	}
	started := time.Now()
	testOutput, err := runCommand(ctx, backendDir, goExe, "test", "./...")
	if err != nil {
		return buildResult{}, fmt.Errorf("plugin tests failed: %s", testOutput)
	}
	buildDir := filepath.Join(project.SourceDir, "build", time.Now().UTC().Format("20060102T150405.000000000"))
	if err = os.MkdirAll(buildDir, 0o700); err != nil {
		return buildResult{}, err
	}
	exeName := "plugin"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	builtExe := filepath.Join(buildDir, exeName)
	buildOutput, err := runCommand(ctx, backendDir, goExe, "build", "-trimpath", "-o", builtExe, ".")
	if err != nil {
		return buildResult{}, fmt.Errorf("plugin build failed: %s", buildOutput)
	}
	frontendSource := filepath.Join(project.SourceDir, "frontend", "index.html")
	digest, err := artifactDigest(manifestRaw, builtExe, frontendSource)
	if err != nil {
		return buildResult{}, err
	}
	bundleDir := filepath.Join(s.dataDir, "plugin-store", "sha256", digest)
	if err = packageRelease(bundleDir, builtExe, frontendSource, manifestRaw); err != nil {
		return buildResult{}, err
	}
	report, _ := json.Marshal(map[string]any{
		"passed": true, "sourceVersion": document.SourceVersion,
		"steps":          []buildStep{{Surface: "backend", Action: "go test", Passed: true, Output: strings.TrimSpace(testOutput)}, {Surface: "backend", Action: "go build", Passed: true, Output: strings.TrimSpace(buildOutput)}, {Surface: "ui", Action: "verify entry", Passed: true}},
		"durationMillis": time.Since(started).Milliseconds(), "checkedAt": time.Now().UTC(),
	})
	return buildResult{digest: digest, bundleDir: bundleDir, report: report}, nil
}

func (s *Service) executeV2Build(ctx context.Context, project Project, manifest pluginmanifest.Manifest) (buildResult, error) {
	started := time.Now()
	steps := []buildStep{}
	artifacts := map[string]string{}

	if manifest.Runtime != nil && manifest.Runtime.Backend != nil {
		goExe, err := findGo()
		if err != nil {
			return buildResult{}, err
		}
		backendDir := filepath.Join(project.SourceDir, "backend")
		if err = validateBackendPolicyV2(backendDir); err != nil {
			return buildResult{}, err
		}
		testOutput, err := runCommand(ctx, backendDir, goExe, "test", "./...")
		if err != nil {
			return buildResult{}, fmt.Errorf("plugin tests failed: %s", testOutput)
		}
		steps = append(steps, buildStep{Surface: "backend", Action: "go test", Passed: true, Output: strings.TrimSpace(testOutput)})
		buildDir := filepath.Join(project.SourceDir, "build", time.Now().UTC().Format("20060102T150405.000000000"))
		if err = os.MkdirAll(buildDir, 0o700); err != nil {
			return buildResult{}, err
		}
		executable := filepath.Join(buildDir, "plugin")
		if runtime.GOOS == "windows" {
			executable += ".exe"
		}
		buildOutput, err := runCommand(ctx, backendDir, goExe, "build", "-trimpath", "-o", executable, ".")
		if err != nil {
			return buildResult{}, fmt.Errorf("plugin build failed: %s", buildOutput)
		}
		steps = append(steps, buildStep{Surface: "backend", Action: "go build", Passed: true, Output: strings.TrimSpace(buildOutput)})
		artifactPath := manifest.Runtime.Backend.Artifact
		if runtime.GOOS != "windows" {
			artifactPath = strings.TrimSuffix(artifactPath, ".exe")
		}
		artifacts[filepath.ToSlash(artifactPath)] = executable
	}

	if manifest.UI != nil {
		uiArtifacts, err := collectUIArtifacts(project.SourceDir, *manifest.UI)
		if err != nil {
			return buildResult{}, err
		}
		for logical, source := range uiArtifacts {
			artifacts[logical] = source
		}
		if err := validateUIArtifacts(uiArtifacts); err != nil {
			return buildResult{}, err
		}
		steps = append(steps, buildStep{Surface: "ui", Action: "verify asset graph", Passed: true, Output: fmt.Sprintf("%d assets", len(uiArtifacts))})
	}

	for _, skill := range manifest.Exports.Skills {
		source, err := sourceArtifact(project.SourceDir, skill.Entry)
		if err != nil {
			return buildResult{}, fmt.Errorf("skill %s: %w", skill.ID, err)
		}
		artifacts[skill.Entry] = source
		steps = append(steps, buildStep{Surface: "skill", Action: "verify " + skill.ID, Passed: true})
	}

	canonicalManifest, _ := json.Marshal(manifest)
	digest, err := digestArtifactSet(canonicalManifest, artifacts)
	if err != nil {
		return buildResult{}, err
	}
	bundleDir := filepath.Join(s.dataDir, "plugin-store", "sha256", digest)
	if err = packageArtifactSet(bundleDir, canonicalManifest, artifacts, manifest); err != nil {
		return buildResult{}, err
	}
	report, _ := json.Marshal(map[string]any{
		"passed": true, "sourceVersion": pluginmanifest.SourceV2, "steps": steps,
		"artifactCount": len(artifacts), "durationMillis": time.Since(started).Milliseconds(), "checkedAt": time.Now().UTC(),
	})
	return buildResult{digest: digest, bundleDir: bundleDir, report: report}, nil
}

func collectUIArtifacts(sourceRoot string, ui pluginmanifest.UI) (map[string]string, error) {
	result := map[string]string{}
	entry, err := sourceArtifact(sourceRoot, ui.Entry)
	if err != nil {
		return nil, fmt.Errorf("UI entry: %w", err)
	}
	result[ui.Entry] = entry
	pattern := ui.Assets
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		root, err := sourceArtifactDirectory(sourceRoot, prefix)
		if err != nil {
			return nil, err
		}
		err = filepath.WalkDir(root, func(current string, item os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if item.IsDir() {
				return nil
			}
			if item.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("UI asset %q is a symbolic link", current)
			}
			relative, err := filepath.Rel(sourceRoot, current)
			if err != nil {
				return err
			}
			result[filepath.ToSlash(relative)] = current
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else if pattern != "" && pattern != "*" {
		matches, err := filepath.Glob(filepath.Join(sourceRoot, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil || info.IsDir() {
				continue
			}
			relative, _ := filepath.Rel(sourceRoot, match)
			result[filepath.ToSlash(relative)] = match
		}
	}
	return result, nil
}

func sourceArtifact(sourceRoot, logical string) (string, error) {
	root, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(logical)))
	if err != nil || !pathWithin(root, target) {
		return "", fmt.Errorf("artifact %q escapes source root", logical)
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("artifact %q is a directory", logical)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil || !pathWithin(resolvedRoot, resolvedTarget) {
		return "", fmt.Errorf("artifact %q resolves outside source root", logical)
	}
	return resolvedTarget, nil
}

func sourceArtifactDirectory(sourceRoot, logical string) (string, error) {
	root, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(logical)))
	if err != nil || !pathWithin(root, target) {
		return "", fmt.Errorf("asset root %q escapes source root", logical)
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("asset root %q is not a directory", logical)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil || !pathWithin(resolvedRoot, resolvedTarget) {
		return "", fmt.Errorf("asset root %q resolves outside source root", logical)
	}
	return resolvedTarget, nil
}

func validateUIArtifacts(artifacts map[string]string) error {
	for logical, source := range artifacts {
		extension := strings.ToLower(filepath.Ext(logical))
		if extension != ".html" && extension != ".js" && extension != ".css" {
			continue
		}
		file, err := os.Open(source)
		if err != nil {
			return err
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 1<<20+1))
		closeErr := file.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(raw) > 1<<20 {
			return fmt.Errorf("UI asset %q exceeds 1 MiB", logical)
		}
		if forbidden := uiPolicyViolation(raw); forbidden != "" {
			return fmt.Errorf("UI asset %q violates strict sandbox policy (%s)", logical, forbidden)
		}
	}
	return nil
}

func uiPolicyViolation(raw []byte) string {
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{"<base ", "javascript:", "http://", "https://", "ws://", "wss://", "serviceworker.register", "new function("} {
		if strings.Contains(lower, forbidden) {
			return forbidden
		}
	}
	return ""
}

func digestArtifactSet(manifest []byte, artifacts map[string]string) (string, error) {
	hash := sha256.New()
	hash.Write([]byte("manifest.json\x00"))
	hash.Write(manifest)
	paths := make([]string, 0, len(artifacts))
	for logical := range artifacts {
		paths = append(paths, logical)
	}
	sort.Strings(paths)
	for _, logical := range paths {
		hash.Write([]byte("\x00" + logical + "\x00"))
		file, err := os.Open(artifacts[logical])
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func packageArtifactSet(bundleDir string, manifest []byte, artifacts map[string]string, definition pluginmanifest.Manifest) error {
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		return err
	}
	for logical, source := range artifacts {
		mode := os.FileMode(0o600)
		if definition.Runtime != nil && definition.Runtime.Backend != nil {
			backendPath := definition.Runtime.Backend.Artifact
			if runtime.GOOS != "windows" {
				backendPath = strings.TrimSuffix(backendPath, ".exe")
			}
			if filepath.ToSlash(logical) == filepath.ToSlash(backendPath) {
				mode = 0o700
			}
		}
		target := filepath.Join(bundleDir, filepath.FromSlash(logical))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := copyIfMissing(source, target, mode); err != nil {
			return err
		}
	}
	manifestPath := filepath.Join(bundleDir, "manifest.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		return os.WriteFile(manifestPath, manifest, 0o600)
	}
	return nil
}
