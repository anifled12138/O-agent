package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/projectpolicy"
)

const maxProjectSubmoduleDeltaFiles = 100000

type projectSubmoduleFile struct {
	Path                string `json:"path"`
	Entry               string `json:"entry,omitempty"`
	Action              string `json:"action"`
	Mode                int64  `json:"mode,omitempty"`
	Size                int64  `json:"size,omitempty"`
	SHA256              string `json:"sha256,omitempty"`
	LinkTarget          string `json:"linkTarget,omitempty"`
	SubmoduleBaseCommit string `json:"submoduleBaseCommit,omitempty"`
	SubmoduleRepository string `json:"submoduleRepository,omitempty"`
}

type projectSubmoduleSnapshot struct {
	Path       string                 `json:"path"`
	Repository string                 `json:"repository"`
	BaseCommit string                 `json:"baseCommit"`
	HeadCommit string                 `json:"headCommit"`
	Files      []projectSubmoduleFile `json:"files"`
}

type projectSubmoduleDeltaManifest struct {
	Version int                        `json:"version"`
	TaskID  string                     `json:"taskId"`
	Modules []projectSubmoduleSnapshot `json:"modules"`
}

type projectSubmoduleDeltaBuild struct {
	path      string
	baseLinks []projectSubmoduleLink
}

type projectSubmoduleLink struct {
	path string
	sha  string
}

// createProjectSubmoduleDeltaArchive records file-level changes inside nested
// submodules while keeping the superproject gitlinks pinned to their bases.
// This lets a receiving node hydrate the authorized base first, then apply the
// content snapshot without requiring an unpublished child commit to exist on
// GitHub/Gitee. Files are content-hashed; deletes are explicit manifest rows.
func (s *Service) createProjectSubmoduleDeltaArchive(ctx context.Context, root, rootURL, baseSHA, taskID, commonGitDir string, run func([]string, bool) (string, error)) (projectSubmoduleDeltaBuild, error) {
	manifest := projectSubmoduleDeltaManifest{Version: 1, TaskID: taskID}
	result := projectSubmoduleDeltaBuild{}
	moduleCount := 0
	if err := s.collectProjectSubmoduleDeltas(ctx, root, "", rootURL, baseSHA, 0, run, &manifest, &result, &moduleCount); err != nil {
		return projectSubmoduleDeltaBuild{}, err
	}
	if len(manifest.Modules) == 0 {
		return result, persistProjectSubmoduleDeltaMarker(commonGitDir, taskID, "none")
	}
	sort.Slice(manifest.Modules, func(i, j int) bool { return manifest.Modules[i].Path < manifest.Modules[j].Path })
	entryNumber := 0
	for mi := range manifest.Modules {
		moduleRoot := filepath.Join(root, filepath.FromSlash(manifest.Modules[mi].Path))
		for fi := range manifest.Modules[mi].Files {
			record := &manifest.Modules[mi].Files[fi]
			if record.Action != "write" || record.Mode == int64(os.ModeSymlink) {
				continue
			}
			entryNumber++
			if entryNumber > maxProjectSubmoduleDeltaFiles {
				return projectSubmoduleDeltaBuild{}, fmt.Errorf("submodule delta has more than %d files", maxProjectSubmoduleDeltaFiles)
			}
			record.Entry = fmt.Sprintf("data/%08d", entryNumber)
			file, err := os.Open(filepath.Join(moduleRoot, filepath.FromSlash(record.Path)))
			if err != nil {
				return projectSubmoduleDeltaBuild{}, err
			}
			info, statErr := file.Stat()
			hasher := sha256.New()
			written, hashErr := io.Copy(hasher, file)
			closeErr := file.Close()
			if statErr != nil || hashErr != nil || closeErr != nil || !info.Mode().IsRegular() || written != info.Size() {
				return projectSubmoduleDeltaBuild{}, errors.Join(fmt.Errorf("hash submodule delta file %q: %w", record.Path, domain.ErrConflict), statErr, hashErr, closeErr)
			}
			record.Size = written
			record.SHA256 = hex.EncodeToString(hasher.Sum(nil))
		}
	}
	archivePath := filepath.Join(commonGitDir, "o-agent-submodule-delta-"+taskID+".tar")
	if info, err := os.Lstat(archivePath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return projectSubmoduleDeltaBuild{}, fmt.Errorf("existing submodule delta archive is unsafe: %w", domain.ErrConflict)
		}
		if err := verifyProjectSubmoduleDeltaArchive(archivePath, manifest); err != nil {
			return projectSubmoduleDeltaBuild{}, fmt.Errorf("existing task submodule delta differs from current workspace: %w", err)
		}
		if err := persistProjectSubmoduleDeltaMarker(commonGitDir, taskID, hashProjectSubmoduleDeltaFile(archivePath)); err != nil {
			return projectSubmoduleDeltaBuild{}, err
		}
		result.path = archivePath
		return result, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return projectSubmoduleDeltaBuild{}, err
	}
	file, err := os.CreateTemp(commonGitDir, ".o-agent-submodule-delta-"+taskID+"-*.tar")
	if err != nil {
		return projectSubmoduleDeltaBuild{}, err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	writer := tar.NewWriter(file)
	for mi := range manifest.Modules {
		moduleRoot := filepath.Join(root, filepath.FromSlash(manifest.Modules[mi].Path))
		for fi := range manifest.Modules[mi].Files {
			record := &manifest.Modules[mi].Files[fi]
			if record.Action != "write" || record.Mode == int64(os.ModeSymlink) {
				continue
			}
			sourcePath := filepath.Join(moduleRoot, filepath.FromSlash(record.Path))
			source, err := os.Open(sourcePath)
			if err != nil {
				_ = writer.Close()
				_ = file.Close()
				return projectSubmoduleDeltaBuild{}, fmt.Errorf("open submodule delta file %q: %w", record.Path, err)
			}
			openedInfo, statErr := source.Stat()
			pathInfo, pathErr := os.Lstat(sourcePath)
			if statErr != nil || pathErr != nil || !openedInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, pathInfo) || openedInfo.Size() != record.Size {
				_ = source.Close()
				_ = writer.Close()
				_ = file.Close()
				return projectSubmoduleDeltaBuild{}, errors.Join(fmt.Errorf("submodule delta file changed while being packaged: %w", domain.ErrConflict), statErr, pathErr)
			}
			header := &tar.Header{Name: record.Entry, Mode: record.Mode, Size: openedInfo.Size(), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR}
			if err := writer.WriteHeader(header); err != nil {
				_ = source.Close()
				_ = writer.Close()
				_ = file.Close()
				return projectSubmoduleDeltaBuild{}, err
			}
			hasher := sha256.New()
			written, copyErr := io.Copy(io.MultiWriter(writer, hasher), source)
			closeErr := source.Close()
			if copyErr != nil || closeErr != nil || written != openedInfo.Size() || hex.EncodeToString(hasher.Sum(nil)) != record.SHA256 {
				_ = writer.Close()
				_ = file.Close()
				return projectSubmoduleDeltaBuild{}, errors.Join(fmt.Errorf("copy submodule delta file %q: %w", record.Path, copyErr), closeErr)
			}
		}
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		_ = writer.Close()
		_ = file.Close()
		return projectSubmoduleDeltaBuild{}, err
	}
	if err := writer.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifestBytes)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR}); err != nil {
		_ = writer.Close()
		_ = file.Close()
		return projectSubmoduleDeltaBuild{}, err
	}
	if _, err := writer.Write(manifestBytes); err != nil {
		_ = writer.Close()
		_ = file.Close()
		return projectSubmoduleDeltaBuild{}, err
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return projectSubmoduleDeltaBuild{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return projectSubmoduleDeltaBuild{}, err
	}
	if err := file.Close(); err != nil {
		return projectSubmoduleDeltaBuild{}, err
	}
	if err := os.Rename(temporary, archivePath); err != nil {
		return projectSubmoduleDeltaBuild{}, fmt.Errorf("publish submodule delta archive: %w", err)
	}
	if err := verifyProjectSubmoduleDeltaArchive(archivePath, manifest); err != nil {
		return projectSubmoduleDeltaBuild{}, fmt.Errorf("read back generated submodule delta archive: %w", err)
	}
	if err := persistProjectSubmoduleDeltaMarker(commonGitDir, taskID, hashProjectSubmoduleDeltaFile(archivePath)); err != nil {
		_ = os.Remove(archivePath)
		return projectSubmoduleDeltaBuild{}, err
	}
	result.path = archivePath
	return result, nil
}

func (s *Service) collectProjectSubmoduleDeltas(ctx context.Context, workspaceRoot, repositoryPath, repositoryURL, baseCommit string, depth int, run func([]string, bool) (string, error), manifest *projectSubmoduleDeltaManifest, result *projectSubmoduleDeltaBuild, moduleCount *int) error {
	if depth > maxSubmoduleDepth {
		return fmt.Errorf("submodule delta nesting exceeds %d levels: %w", maxSubmoduleDepth, domain.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	withRepository := func(args ...string) []string {
		if repositoryPath == "" {
			return args
		}
		return append([]string{"-C", filepath.ToSlash(repositoryPath)}, args...)
	}
	manifestTree, err := run(withRepository("ls-tree", "-z", baseCommit, "--", ".gitmodules"), false)
	if err != nil {
		return fmt.Errorf("inspect pinned submodule manifest at %q: %w", repositoryPath, err)
	}
	if _, exists, err := parseGitBlobEntry(manifestTree, ".gitmodules"); err != nil || !exists {
		return err
	}
	configOutput, err := run(withRepository("config", "--null", "--blob", baseCommit+":.gitmodules", "--get-regexp", `^submodule\..*\.(path|url)$`), false)
	if err != nil {
		return fmt.Errorf("read pinned submodule manifest at %q: %w", repositoryPath, err)
	}
	modules, err := parseGitSubmoduleConfig(configOutput)
	if err != nil {
		return err
	}
	for _, module := range modules {
		if err := ctx.Err(); err != nil {
			return err
		}
		(*moduleCount)++
		if *moduleCount > maxSubmoduleCount {
			return fmt.Errorf("submodule delta includes more than %d repositories: %w", maxSubmoduleCount, domain.ErrInvalid)
		}
		globalPath := path.Join(repositoryPath, module.Path)
		if err := validateGitSubmodulePath(globalPath); err != nil {
			return fmt.Errorf("submodule path %q: %w", globalPath, err)
		}
		gitlinkOutput, err := run(withRepository("ls-tree", "-z", baseCommit, "--", module.Path), false)
		if err != nil {
			return fmt.Errorf("read pinned gitlink %q: %w", globalPath, err)
		}
		base, ok := gitSubmoduleLinkCommit(gitlinkOutput, module.Path)
		if !ok {
			return fmt.Errorf("pinned submodule %q is not a gitlink: %w", globalPath, domain.ErrConflict)
		}
		moduleURL, err := resolveGitSubmoduleURL(repositoryURL, module.URL)
		if err != nil {
			return fmt.Errorf("resolve pinned submodule %q URL: %w", globalPath, err)
		}
		if err := ensureSafeSubmoduleDestination(workspaceRoot, globalPath); err != nil {
			return err
		}
		moduleRoot := filepath.Join(workspaceRoot, filepath.FromSlash(globalPath))
		info, err := os.Lstat(moduleRoot)
		if errors.Is(err, os.ErrNotExist) {
			continue // A removed gitlink remains represented by its enclosing repository delta.
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(fmt.Errorf("submodule worktree %q is missing or unsafe: %w", globalPath, domain.ErrConflict), err)
		}
		marker, err := os.Lstat(filepath.Join(moduleRoot, ".git"))
		if err != nil || !(marker.IsDir() || marker.Mode().IsRegular()) || marker.Mode()&os.ModeSymlink != 0 {
			return errors.Join(fmt.Errorf("submodule worktree %q is not initialized safely: %w", globalPath, domain.ErrConflict), err)
		}
		gitModulePath := filepath.ToSlash(globalPath)
		head, err := run([]string{"-C", gitModulePath, "rev-parse", "--verify", "HEAD^{commit}"}, false)
		if err != nil || !validGitObjectID(strings.ToLower(head)) {
			return errors.Join(fmt.Errorf("read submodule %q HEAD: %w", globalPath, domain.ErrConflict), err)
		}
		origin, err := run([]string{"-C", gitModulePath, "remote", "get-url", "origin"}, false)
		if err != nil {
			return fmt.Errorf("read submodule %q origin: %w", globalPath, err)
		}
		_, normalizedOrigin, err := projectpolicy.ValidateRepositoryURL(strings.TrimSpace(origin))
		if err != nil || normalizedOrigin != moduleURL {
			return errors.Join(fmt.Errorf("submodule %q origin does not match its committed manifest: %w", globalPath, domain.ErrConflict), err)
		}
		changed, err := run([]string{"-C", gitModulePath, "diff", "--name-only", "-z", base, "--", "."}, false)
		if err != nil {
			return fmt.Errorf("list tracked submodule changes for %q: %w", globalPath, err)
		}
		untracked, err := run([]string{"-C", gitModulePath, "ls-files", "--others", "--exclude-standard", "-z"}, false)
		if err != nil {
			return fmt.Errorf("list untracked submodule files for %q: %w", globalPath, err)
		}
		paths := uniqueSubmodulePaths(changed, untracked)
		if len(paths) > 0 || !strings.EqualFold(head, base) {
			snapshot := projectSubmoduleSnapshot{Path: globalPath, Repository: moduleURL, BaseCommit: base, HeadCommit: strings.ToLower(head)}
			for _, relative := range paths {
				if err := validateSubmoduleFilePath(relative); err != nil {
					return fmt.Errorf("submodule %q: %w", globalPath, err)
				}
				if err := ensureSafeSubmoduleFileParents(moduleRoot, relative); err != nil {
					return err
				}
				full := filepath.Join(moduleRoot, filepath.FromSlash(relative))
				fileInfo, err := os.Lstat(full)
				if errors.Is(err, os.ErrNotExist) {
					baseEntry, entryErr := run([]string{"-C", gitModulePath, "ls-tree", "-z", base, "--", relative}, false)
					if entryErr != nil {
						return entryErr
					}
					if childCommit, isGitlink := gitSubmoduleLinkCommit(baseEntry, relative); isGitlink {
						var childURL string
						childConfig, configErr := run([]string{"-C", gitModulePath, "config", "--null", "--blob", base + ":.gitmodules", "--get-regexp", `^submodule\..*\.(path|url)$`}, false)
						childModules, parseErr := parseGitSubmoduleConfig(childConfig)
						if configErr != nil || parseErr != nil {
							return errors.Join(fmt.Errorf("read pinned nested manifest for removed gitlink %q: %w", globalPath+"/"+relative, domain.ErrConflict), configErr, parseErr)
						}
						for _, childModule := range childModules {
							if childModule.Path == relative {
								childURL, entryErr = resolveGitSubmoduleURL(moduleURL, childModule.URL)
								break
							}
						}
						if entryErr != nil || childURL == "" {
							return errors.Join(fmt.Errorf("removed nested gitlink %q is absent from its pinned manifest: %w", globalPath+"/"+relative, domain.ErrConflict), entryErr)
						}
						snapshot.Files = append(snapshot.Files, projectSubmoduleFile{Path: relative, Action: "remove_submodule", SubmoduleBaseCommit: childCommit, SubmoduleRepository: childURL})
						continue
					}
					snapshot.Files = append(snapshot.Files, projectSubmoduleFile{Path: relative, Action: "delete"})
					continue
				}
				if err != nil {
					return err
				}
				if fileInfo.IsDir() || fileInfo.Mode()&os.ModeNamedPipe != 0 || fileInfo.Mode()&os.ModeSocket != 0 || fileInfo.Mode()&os.ModeDevice != 0 {
					if fileInfo.IsDir() {
						baseEntry, entryErr := run([]string{"-C", gitModulePath, "ls-tree", "-z", base, "--", relative}, false)
						if entryErr != nil {
							return entryErr
						}
						if _, isGitlink := gitSubmoduleLinkCommit(baseEntry, relative); isGitlink {
							continue // The nested repository's files are captured by the recursive walk below.
						}
					}
					return fmt.Errorf("submodule change %q is not a regular file or symlink: %w", globalPath+"/"+relative, domain.ErrInvalid)
				}
				record := projectSubmoduleFile{Path: relative, Action: "write", Mode: int64(fileInfo.Mode().Perm())}
				if fileInfo.Mode()&os.ModeSymlink != 0 {
					target, err := os.Readlink(full)
					if err != nil {
						return err
					}
					if err := validateSubmoduleSymlinkTarget(relative, target); err != nil {
						return fmt.Errorf("submodule symlink %q/%q: %w", globalPath, relative, err)
					}
					record.Mode = int64(os.ModeSymlink)
					record.LinkTarget = target
					digest := sha256.Sum256([]byte(target))
					record.SHA256 = hex.EncodeToString(digest[:])
					record.Size = int64(len(target))
				} else if !fileInfo.Mode().IsRegular() {
					return fmt.Errorf("submodule change %q has an unsupported file type: %w", relative, domain.ErrInvalid)
				}
				snapshot.Files = append(snapshot.Files, record)
			}
			manifest.Modules = append(manifest.Modules, snapshot)
			if depth == 0 {
				result.baseLinks = append(result.baseLinks, projectSubmoduleLink{path: globalPath, sha: base})
			}
		}
		if err := s.collectProjectSubmoduleDeltas(ctx, workspaceRoot, globalPath, moduleURL, base, depth+1, run, manifest, result, moduleCount); err != nil {
			return err
		}
	}
	return nil
}

func hashProjectSubmoduleDeltaFile(filePath string) string {
	file, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return ""
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func persistProjectSubmoduleDeltaMarker(commonGitDir, taskID, digest string) error {
	if digest != "none" {
		if len(digest) != 64 {
			return fmt.Errorf("submodule delta snapshot hash is unavailable: %w", domain.ErrConflict)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return domain.ErrConflict
		}
	}
	marker := filepath.Join(commonGitDir, "o-agent-submodule-delta-"+taskID+".sha256")
	if current, err := os.ReadFile(marker); err == nil {
		if strings.TrimSpace(string(current)) != digest {
			return fmt.Errorf("task submodule snapshot changed after its first durable capture: %w", domain.ErrConflict)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(commonGitDir, ".o-agent-submodule-marker-"+taskID+"-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := io.WriteString(temporary, digest+"\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, marker); err != nil {
		if current, readErr := os.ReadFile(marker); readErr == nil && strings.TrimSpace(string(current)) == digest {
			return nil
		}
		return err
	}
	readBack, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(readBack)) != digest {
		return errors.Join(fmt.Errorf("submodule delta marker failed durable read-back: %w", domain.ErrConflict), err)
	}
	return nil
}

func restorePinnedSubmoduleGitlinks(run func([]string, bool) (string, error), links []projectSubmoduleLink) error {
	for _, link := range links {
		if _, err := run([]string{"update-index", "--add", "--cacheinfo", "160000," + link.sha + "," + link.path}, true); err != nil {
			return fmt.Errorf("keep superproject gitlink %q pinned while its file delta is transferred: %w", link.path, err)
		}
	}
	return nil
}

func validateSubmoduleFilePath(value string) error {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\\:\x00\r\n") || strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return errors.New("changed file path is unsafe")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || strings.EqualFold(component, ".git") {
			return errors.New("changed file path contains an unsafe component")
		}
	}
	return nil
}

func uniqueSubmodulePaths(outputs ...string) []string {
	set := map[string]struct{}{}
	for _, output := range outputs {
		for _, value := range strings.Split(output, "\x00") {
			if value != "" {
				set[value] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func verifyProjectSubmoduleDeltaArchive(archivePath string, expected projectSubmoduleDeltaManifest) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := tar.NewReader(file)
	seenManifest := false
	dataEntries := map[string]bool{}
	expectedFiles := map[string]projectSubmoduleFile{}
	for _, module := range expected.Modules {
		for _, record := range module.Files {
			if record.Entry != "" {
				expectedFiles[record.Entry] = record
			}
		}
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > 1<<40 {
			return errors.New("submodule delta archive contains an unsupported tar entry")
		}
		if header.Name == "manifest.json" {
			if seenManifest || header.Size > 64<<20 {
				return errors.New("submodule delta archive has an invalid manifest entry")
			}
			bytes, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil || int64(len(bytes)) != header.Size {
				return errors.Join(errors.New("submodule delta manifest length is invalid"), err)
			}
			var actual projectSubmoduleDeltaManifest
			if err := json.Unmarshal(bytes, &actual); err != nil || actual.Version != expected.Version || actual.TaskID != expected.TaskID || len(actual.Modules) != len(expected.Modules) {
				return errors.Join(errors.New("submodule delta manifest did not read back as expected"), err)
			}
			encodedExpected, _ := json.Marshal(expected)
			encodedActual, _ := json.Marshal(actual)
			if string(encodedExpected) != string(encodedActual) {
				return errors.New("submodule delta manifest changed during packaging")
			}
			seenManifest = true
			continue
		}
		if !strings.HasPrefix(header.Name, "data/") || len(header.Name) != len("data/")+8 || dataEntries[header.Name] {
			return errors.New("submodule delta archive contains an unexpected file entry")
		}
		record, exists := expectedFiles[header.Name]
		if !exists || record.Size != header.Size {
			return errors.New("submodule delta archive entry does not match its manifest")
		}
		hasher := sha256.New()
		if _, err := io.Copy(hasher, reader); err != nil || hex.EncodeToString(hasher.Sum(nil)) != record.SHA256 {
			return errors.Join(errors.New("submodule delta archive entry hash does not match its manifest"), err)
		}
		dataEntries[header.Name] = true
	}
	if !seenManifest {
		return errors.New("submodule delta archive is missing its manifest")
	}
	for _, module := range expected.Modules {
		for _, record := range module.Files {
			if record.Action == "write" && record.Mode != int64(os.ModeSymlink) && !dataEntries[record.Entry] {
				return fmt.Errorf("submodule delta archive is missing %q", record.Entry)
			}
		}
	}
	return nil
}

type stagedSubmoduleDelta struct {
	manifest projectSubmoduleDeltaManifest
	data     map[string]string
}

func (s *Service) applyProjectSubmoduleDeltaArchive(ctx context.Context, project domain.Project, taskID string, archive io.Reader, archiveSize int64, archiveSHA string) (retErr error) {
	if archive == nil || archiveSize <= 0 || len(archiveSHA) != 64 || taskID == "" {
		return fmt.Errorf("submodule delta archive manifest is incomplete: %w", domain.ErrInvalid)
	}
	if _, err := hex.DecodeString(archiveSHA); err != nil {
		return domain.ErrInvalid
	}
	stage, err := os.MkdirTemp("", "o-agent-submodule-import-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(stage)) }()
	counter := &limitedProjectSubmoduleReader{reader: io.LimitReader(archive, archiveSize+1)}
	hasher := sha256.New()
	stream := io.TeeReader(counter, hasher)
	tarReader := tar.NewReader(stream)
	staged := stagedSubmoduleDelta{data: map[string]string{}}
	seenEntries := map[string]bool{}
	manifestSeen := false
	totalFiles := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("read project submodule archive: %w", nextErr)
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > archiveSize {
			return fmt.Errorf("project submodule archive contains an unsupported entry: %w", domain.ErrInvalid)
		}
		if header.Name == "manifest.json" {
			if manifestSeen || header.Size > 64<<20 {
				return fmt.Errorf("project submodule archive manifest is invalid: %w", domain.ErrInvalid)
			}
			manifestBytes, err := io.ReadAll(io.LimitReader(tarReader, header.Size+1))
			if err != nil || int64(len(manifestBytes)) != header.Size {
				return errors.Join(fmt.Errorf("read project submodule manifest: %w", domain.ErrInvalid), err)
			}
			decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&staged.manifest); err != nil {
				return fmt.Errorf("decode project submodule manifest: %w", err)
			}
			var trailing any
			if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
				return fmt.Errorf("project submodule manifest has trailing data: %w", domain.ErrInvalid)
			}
			manifestSeen = true
			continue
		}
		if manifestSeen || !validSubmoduleDeltaEntryName(header.Name) || seenEntries[header.Name] {
			return fmt.Errorf("project submodule archive entry is unexpected or duplicated: %w", domain.ErrInvalid)
		}
		seenEntries[header.Name] = true
		totalFiles++
		if totalFiles > maxProjectSubmoduleDeltaFiles {
			return fmt.Errorf("project submodule archive exceeds file count limit: %w", domain.ErrInvalid)
		}
		stagedPath := filepath.Join(stage, filepath.FromSlash(header.Name))
		if err := os.MkdirAll(filepath.Dir(stagedPath), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		entryHash := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(file, entryHash), tarReader, header.Size)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || written != header.Size {
			return errors.Join(fmt.Errorf("stage project submodule archive file: %w", copyErr), closeErr)
		}
		staged.data[header.Name] = stagedPath
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return err
	}
	if counter.read != archiveSize || hex.EncodeToString(hasher.Sum(nil)) != strings.ToLower(archiveSHA) || !manifestSeen || staged.manifest.Version != 1 || staged.manifest.TaskID != taskID || len(staged.manifest.Modules) > maxSubmoduleCount {
		return fmt.Errorf("project submodule archive failed size, digest or manifest verification: %w", domain.ErrConflict)
	}
	if err := verifyStagedSubmoduleManifest(staged.manifest, staged.data); err != nil {
		return err
	}
	return s.applyStagedProjectSubmoduleDelta(ctx, project, staged)
}

type limitedProjectSubmoduleReader struct {
	reader io.Reader
	read   int64
}

func (r *limitedProjectSubmoduleReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.read += int64(n)
	return n, err
}

func validSubmoduleDeltaEntryName(value string) bool {
	if len(value) != len("data/")+8 || !strings.HasPrefix(value, "data/") {
		return false
	}
	for _, character := range value[len("data/"):] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func verifyStagedSubmoduleManifest(manifest projectSubmoduleDeltaManifest, entries map[string]string) error {
	if len(manifest.Modules) == 0 {
		return fmt.Errorf("project submodule manifest is empty: %w", domain.ErrInvalid)
	}
	seenPaths := map[string]bool{}
	seenEntries := map[string]bool{}
	files := 0
	for _, module := range manifest.Modules {
		if err := validateGitSubmodulePath(module.Path); err != nil || module.Repository == "" || !validGitObjectID(module.BaseCommit) || !validGitObjectID(module.HeadCommit) || seenPaths[module.Path] {
			return errors.Join(fmt.Errorf("project submodule record is invalid: %w", domain.ErrInvalid), err)
		}
		seenPaths[module.Path] = true
		for _, record := range module.Files {
			files++
			if files > maxProjectSubmoduleDeltaFiles || validateSubmoduleFilePath(record.Path) != nil {
				return fmt.Errorf("project submodule file record is invalid: %w", domain.ErrInvalid)
			}
			switch record.Action {
			case "delete":
				if record.Entry != "" || record.Size != 0 || record.SHA256 != "" || record.LinkTarget != "" || record.SubmoduleBaseCommit != "" || record.SubmoduleRepository != "" {
					return fmt.Errorf("project submodule deletion record has content: %w", domain.ErrInvalid)
				}
			case "remove_submodule":
				_, normalizedURL, urlErr := projectpolicy.ValidateRepositoryURL(record.SubmoduleRepository)
				if record.Entry != "" || record.Mode != 0 || record.Size != 0 || record.SHA256 != "" || record.LinkTarget != "" || !validGitObjectID(record.SubmoduleBaseCommit) || urlErr != nil || normalizedURL != record.SubmoduleRepository {
					return errors.Join(fmt.Errorf("nested submodule removal record is invalid: %w", domain.ErrInvalid), urlErr)
				}
			case "write":
				if record.Mode == int64(os.ModeSymlink) {
					digest := sha256.Sum256([]byte(record.LinkTarget))
					if record.Entry != "" || int64(len(record.LinkTarget)) != record.Size || hex.EncodeToString(digest[:]) != record.SHA256 || validateSubmoduleSymlinkTarget(record.Path, record.LinkTarget) != nil {
						return fmt.Errorf("project submodule symlink record failed verification: %w", domain.ErrConflict)
					}
				} else {
					if record.Mode <= 0 || record.Mode&^int64(0o777) != 0 || !validSubmoduleDeltaEntryName(record.Entry) || seenEntries[record.Entry] || record.Size < 0 || record.Size > 1<<40 || entries[record.Entry] == "" {
						return fmt.Errorf("project submodule file record is invalid: %w", domain.ErrInvalid)
					}
					data, err := os.Open(entries[record.Entry])
					if err != nil {
						return err
					}
					hasher := sha256.New()
					size, hashErr := io.Copy(hasher, data)
					closeErr := data.Close()
					if hashErr != nil || closeErr != nil || size != record.Size || hex.EncodeToString(hasher.Sum(nil)) != record.SHA256 {
						return errors.Join(fmt.Errorf("project submodule file content failed verification: %w", domain.ErrConflict), hashErr, closeErr)
					}
					seenEntries[record.Entry] = true
				}
			default:
				return fmt.Errorf("project submodule action is unsupported: %w", domain.ErrInvalid)
			}
		}
	}
	if len(seenEntries) != len(entries) {
		return fmt.Errorf("project submodule archive contains unreferenced data: %w", domain.ErrInvalid)
	}
	return nil
}

func validateSubmoduleSymlinkTarget(filePath, target string) error {
	if target == "" || len(target) > 4096 || strings.ContainsAny(target, "\\:\x00\r\n") || strings.HasPrefix(target, "/") || filepath.IsAbs(target) {
		return errors.New("symlink target is absolute or malformed")
	}
	resolved := path.Clean(path.Join(path.Dir(filePath), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) {
		return errors.New("symlink target escapes the submodule")
	}
	return nil
}

func (s *Service) applyStagedProjectSubmoduleDelta(ctx context.Context, project domain.Project, staged stagedSubmoduleDelta) error {
	root, err := filepath.EvalSymlinks(filepath.Clean(project.Workdir))
	if err != nil {
		return err
	}
	_, rootURL, err := projectpolicy.ValidateRepositoryURL(project.RemoteRepoURL)
	if err != nil {
		return err
	}
	expectedModules, err := s.readPinnedProjectSubmodules(ctx, root, rootURL, project.ResolvedCommit)
	if err != nil {
		return err
	}
	var authorized []string
	for _, snapshot := range staged.manifest.Modules {
		if err := ctx.Err(); err != nil {
			return err
		}
		expectedURL, exists := expectedModules[snapshot.Path]
		if !exists || expectedURL != snapshot.Repository {
			return fmt.Errorf("submodule snapshot does not match the pinned manifest at %q: %w", snapshot.Path, domain.ErrConflict)
		}
		authorized = append(authorized, expectedURL)
	}
	config := s.executionConfigSnapshot(append([]string{rootURL}, authorized...)...)
	run := func(args []string, write bool) (string, error) {
		writePaths := []string(nil)
		if write {
			writePaths = []string{root}
		}
		stdout, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", args, root, []string{root}, writePaths, false, 30*time.Second)
		if runErr != nil || cleanupErr != nil {
			return "", errors.Join(fmt.Errorf("verify submodule snapshot with Git: %w: %s", runErr, strings.TrimSpace(stderr)), cleanupErr)
		}
		return strings.TrimSpace(stdout), nil
	}
	for _, snapshot := range staged.manifest.Modules {
		if err := ctx.Err(); err != nil {
			return err
		}
		moduleRoot := filepath.Join(root, filepath.FromSlash(snapshot.Path))
		if err := ensureSafeSubmoduleDestination(root, snapshot.Path); err != nil {
			return err
		}
		head, err := run([]string{"-C", filepath.ToSlash(snapshot.Path), "rev-parse", "--verify", "HEAD^{commit}"}, false)
		if err != nil || !strings.EqualFold(head, snapshot.BaseCommit) {
			return errors.Join(fmt.Errorf("submodule %q is not at its pinned base before applying snapshot: %w", snapshot.Path, domain.ErrConflict), err)
		}
		origin, err := run([]string{"-C", filepath.ToSlash(snapshot.Path), "remote", "get-url", "origin"}, false)
		if err != nil {
			return err
		}
		_, normalizedOrigin, err := projectpolicy.ValidateRepositoryURL(strings.TrimSpace(origin))
		if err != nil || normalizedOrigin != snapshot.Repository {
			return errors.Join(fmt.Errorf("submodule %q origin changed before applying snapshot: %w", snapshot.Path, domain.ErrConflict), err)
		}
		for _, record := range snapshot.Files {
			if err := ensureSafeSubmoduleFileParents(moduleRoot, record.Path); err != nil {
				return err
			}
			baseEntry, err := run([]string{"-C", filepath.ToSlash(snapshot.Path), "ls-tree", "-z", snapshot.BaseCommit, "--", record.Path}, false)
			if err != nil {
				return err
			}
			fullPath := filepath.Join(moduleRoot, filepath.FromSlash(record.Path))
			currentInfo, statErr := os.Lstat(fullPath)
			currentExists := statErr == nil
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
			if record.Action == "remove_submodule" {
				baseCommit, isGitlink := gitSubmoduleLinkCommit(baseEntry, record.Path)
				globalPath := path.Join(snapshot.Path, record.Path)
				expectedURL, exists := expectedModules[globalPath]
				if !isGitlink || !strings.EqualFold(baseCommit, record.SubmoduleBaseCommit) || !exists || expectedURL != record.SubmoduleRepository {
					return fmt.Errorf("nested submodule removal does not match its pinned gitlink: %s/%s: %w", snapshot.Path, record.Path, domain.ErrConflict)
				}
				if !currentExists {
					continue // Replaying the same task removal is idempotent.
				}
				if !currentInfo.IsDir() || currentInfo.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("nested submodule destination is unsafe: %s/%s: %w", snapshot.Path, record.Path, domain.ErrConflict)
				}
				nestedPath := filepath.ToSlash(globalPath)
				nestedHead, headErr := run([]string{"-C", nestedPath, "rev-parse", "--verify", "HEAD^{commit}"}, false)
				nestedOrigin, originErr := run([]string{"-C", nestedPath, "remote", "get-url", "origin"}, false)
				_, nestedOrigin, normalizeErr := projectpolicy.ValidateRepositoryURL(strings.TrimSpace(nestedOrigin))
				trackedChanges, diffErr := run([]string{"-C", nestedPath, "diff", "--name-only", "-z", record.SubmoduleBaseCommit, "--", "."}, false)
				untrackedChanges, cleanErr := run([]string{"-C", nestedPath, "clean", "-n", "-d", "-x", "--"}, false)
				if headErr != nil || originErr != nil || normalizeErr != nil || diffErr != nil || cleanErr != nil || !strings.EqualFold(nestedHead, record.SubmoduleBaseCommit) || nestedOrigin != record.SubmoduleRepository || trackedChanges != "" || untrackedChanges != "" {
					return errors.Join(fmt.Errorf("refuse to remove nested submodule with changed identity or local files: %s/%s (headMatch=%t originMatch=%t hasTrackedChanges=%t hasOtherFiles=%t): %w", snapshot.Path, record.Path, strings.EqualFold(nestedHead, record.SubmoduleBaseCommit), nestedOrigin == record.SubmoduleRepository, trackedChanges != "", untrackedChanges != "", domain.ErrConflict), headErr, originErr, normalizeErr, diffErr, cleanErr)
				}
				continue
			}
			baseOID, exists, err := parseSubmoduleTreeBlob(baseEntry, record.Path)
			if err != nil {
				return err
			}
			if record.Action == "delete" {
				if !currentExists {
					continue
				}
				currentOID, err := run([]string{"-C", filepath.ToSlash(snapshot.Path), "hash-object", "--no-filters", "--", record.Path}, false)
				if err != nil || !exists || !strings.EqualFold(currentOID, baseOID) {
					return errors.Join(fmt.Errorf("refuse to delete submodule file changed after snapshot: %s/%s: %w", snapshot.Path, record.Path, domain.ErrConflict), err)
				}
				continue
			}
			if currentExists && submodulePathSHA256(fullPath, currentInfo) == record.SHA256 {
				continue
			}
			if currentExists {
				currentOID, err := run([]string{"-C", filepath.ToSlash(snapshot.Path), "hash-object", "--no-filters", "--", record.Path}, false)
				if err != nil || !exists || !strings.EqualFold(currentOID, baseOID) {
					return errors.Join(fmt.Errorf("refuse to overwrite submodule file changed after snapshot: %s/%s (current=%s base=%s exists=%t): %w", snapshot.Path, record.Path, currentOID, baseOID, exists, domain.ErrConflict), err)
				}
			} else if exists {
				return fmt.Errorf("refuse to replace missing submodule base file %s/%s: %w", snapshot.Path, record.Path, domain.ErrConflict)
			}
		}
	}
	for _, snapshot := range staged.manifest.Modules {
		moduleRoot := filepath.Join(root, filepath.FromSlash(snapshot.Path))
		for _, record := range snapshot.Files {
			fullPath := filepath.Join(moduleRoot, filepath.FromSlash(record.Path))
			if record.Action == "remove_submodule" {
				if err := ensureSafeSubmoduleDestination(root, path.Join(snapshot.Path, record.Path)); err != nil {
					return err
				}
				if err := os.RemoveAll(fullPath); err != nil {
					return err
				}
				continue
			}
			if record.Action == "delete" {
				if err := os.Remove(fullPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
				return err
			}
			if record.Mode == int64(os.ModeSymlink) {
				temporary := fullPath + ".o-agent-" + staged.manifest.TaskID + ".tmp"
				_ = os.Remove(temporary)
				if err := os.Symlink(record.LinkTarget, temporary); err != nil {
					return err
				}
				if err := os.Rename(temporary, fullPath); err != nil {
					_ = os.Remove(temporary)
					return err
				}
				continue
			}
			source, err := os.Open(staged.data[record.Entry])
			if err != nil {
				return err
			}
			temporary, err := os.CreateTemp(filepath.Dir(fullPath), ".o-agent-"+staged.manifest.TaskID+"-*.tmp")
			if err != nil {
				_ = source.Close()
				return err
			}
			_, copyErr := io.Copy(temporary, source)
			closeSourceErr := source.Close()
			chmodErr := temporary.Chmod(os.FileMode(record.Mode))
			closeTemporaryErr := temporary.Close()
			if err := errors.Join(copyErr, closeSourceErr, chmodErr, closeTemporaryErr); err != nil {
				_ = os.Remove(temporary.Name())
				return err
			}
			if err := os.Rename(temporary.Name(), fullPath); err != nil {
				_ = os.Remove(temporary.Name())
				return err
			}
		}
	}
	for _, snapshot := range staged.manifest.Modules {
		for _, record := range snapshot.Files {
			fullPath := filepath.Join(root, filepath.FromSlash(snapshot.Path), filepath.FromSlash(record.Path))
			if record.Action == "delete" || record.Action == "remove_submodule" {
				if _, err := os.Lstat(fullPath); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("deleted submodule output did not read back absent: %s/%s: %w", snapshot.Path, record.Path, domain.ErrConflict)
				}
			} else {
				info, err := os.Lstat(fullPath)
				if err != nil || submodulePathSHA256(fullPath, info) != record.SHA256 {
					return errors.Join(fmt.Errorf("submodule output did not read back with its content hash: %s/%s: %w", snapshot.Path, record.Path, domain.ErrConflict), err)
				}
			}
		}
	}
	return nil
}

func (s *Service) readPinnedProjectSubmodules(ctx context.Context, root, rootURL, commit string) (map[string]string, error) {
	config := s.executionConfigSnapshot(rootURL)
	run := func(args []string) (string, error) {
		stdout, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, config, "git", args, root, []string{root}, nil, false, 30*time.Second)
		if runErr != nil || cleanupErr != nil {
			return "", errors.Join(fmt.Errorf("read pinned submodule configuration: %w: %s", runErr, strings.TrimSpace(stderr)), cleanupErr)
		}
		return stdout, nil
	}
	result := make(map[string]string)
	moduleCount := 0
	var walk func(repositoryPath, repositoryURL, pinnedCommit string, depth int) error
	walk = func(repositoryPath, repositoryURL, pinnedCommit string, depth int) error {
		if depth > maxSubmoduleDepth {
			return fmt.Errorf("submodule delta nesting exceeds %d levels: %w", maxSubmoduleDepth, domain.ErrInvalid)
		}
		withRepository := func(args ...string) []string {
			if repositoryPath == "" {
				return args
			}
			return append([]string{"-C", filepath.ToSlash(repositoryPath)}, args...)
		}
		manifestTree, err := run(withRepository("ls-tree", "-z", pinnedCommit, "--", ".gitmodules"))
		if err != nil {
			return err
		}
		if _, exists, err := parseGitBlobEntry(manifestTree, ".gitmodules"); err != nil || !exists {
			return err
		}
		configBytes, err := run(withRepository("config", "--null", "--blob", pinnedCommit+":.gitmodules", "--get-regexp", `^submodule\..*\.(path|url)$`))
		if err != nil {
			return err
		}
		modules, err := parseGitSubmoduleConfig(configBytes)
		if err != nil {
			return err
		}
		for _, module := range modules {
			moduleCount++
			if moduleCount > maxSubmoduleCount {
				return fmt.Errorf("submodule delta includes more than %d repositories: %w", maxSubmoduleCount, domain.ErrInvalid)
			}
			globalPath := path.Join(repositoryPath, module.Path)
			if err := validateGitSubmodulePath(globalPath); err != nil {
				return err
			}
			if err := ensureSafeSubmoduleDestination(root, globalPath); err != nil {
				return err
			}
			linkOutput, err := run(withRepository("ls-tree", "-z", pinnedCommit, "--", module.Path))
			if err != nil {
				return err
			}
			childCommit, ok := gitSubmoduleLinkCommit(linkOutput, module.Path)
			if !ok {
				return fmt.Errorf("pinned submodule %q is not a gitlink: %w", globalPath, domain.ErrConflict)
			}
			resolved, err := resolveGitSubmoduleURL(repositoryURL, module.URL)
			if err != nil {
				return err
			}
			result[globalPath] = resolved
			moduleRoot := filepath.Join(root, filepath.FromSlash(globalPath))
			moduleInfo, statErr := os.Lstat(moduleRoot)
			if errors.Is(statErr, os.ErrNotExist) {
				continue // A task sidecar may have removed this pinned nested gitlink.
			}
			if statErr != nil || !moduleInfo.IsDir() || moduleInfo.Mode()&os.ModeSymlink != 0 {
				return errors.Join(fmt.Errorf("pinned submodule worktree is unsafe at %q: %w", globalPath, domain.ErrConflict), statErr)
			}
			if err := walk(globalPath, resolved, childCommit, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk("", rootURL, commit, 0); err != nil {
		return nil, err
	}
	return result, nil
}

func parseSubmoduleTreeBlob(output, expectedPath string) (string, bool, error) {
	if output == "" {
		return "", false, nil
	}
	var oid string
	for _, item := range strings.Split(output, "\x00") {
		if item == "" {
			continue
		}
		metadata, name, ok := strings.Cut(item, "\t")
		fields := strings.Fields(metadata)
		if !ok || name != expectedPath || len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000") || !validGitObjectID(strings.ToLower(fields[2])) {
			return "", false, fmt.Errorf("submodule base tree entry is invalid: %w", domain.ErrConflict)
		}
		if oid != "" {
			return "", false, fmt.Errorf("submodule base tree entry is duplicated: %w", domain.ErrConflict)
		}
		oid = strings.ToLower(fields[2])
	}
	return oid, oid != "", nil
}

func ensureSafeSubmoduleFileParents(root, relative string) error {
	if err := validateSubmoduleFilePath(relative); err != nil {
		return err
	}
	current := root
	parts := strings.Split(filepath.FromSlash(relative), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(fmt.Errorf("submodule snapshot path parent is unsafe: %w", domain.ErrConflict), err)
		}
	}
	return nil
}

func submodulePathSHA256(filePath string, info os.FileInfo) string {
	if info == nil {
		return ""
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(filePath)
		if err != nil {
			return ""
		}
		digest := sha256.Sum256([]byte(target))
		return hex.EncodeToString(digest[:])
	}
	if !info.Mode().IsRegular() {
		return ""
	}
	file, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	hasher := sha256.New()
	_, hashErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if hashErr != nil || closeErr != nil {
		return ""
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
