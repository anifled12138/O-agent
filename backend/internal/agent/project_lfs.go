package agent

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

// ImportProjectLFSObjects installs and verifies the content referenced by a
// task commit. An absent archive is accepted only when the destination already
// has every referenced object (for example, an unchanged base object).
func (s *Service) ImportProjectLFSObjects(ctx context.Context, project domain.Project, baseSHA, commitSHA string, archive io.Reader, archiveSize int64, archiveSHA string) error {
	if s == nil || project.ID == "" || project.Workdir == "" || !validGitObjectID(strings.ToLower(baseSHA)) || !validGitObjectID(strings.ToLower(commitSHA)) {
		return domain.ErrInvalid
	}
	if archive == nil && (archiveSize != 0 || archiveSHA != "") || archive != nil && (archiveSize <= 0 || len(archiveSHA) != 64) {
		return domain.ErrInvalid
	}
	if archiveSHA != "" {
		if _, err := hex.DecodeString(archiveSHA); err != nil {
			return domain.ErrInvalid
		}
	}
	release, err := s.projectRunLocks.Acquire(ctx, project.ID)
	if err != nil {
		return err
	}
	defer release()
	workdir, err := filepath.Abs(filepath.Clean(project.Workdir))
	if err != nil {
		return err
	}
	workdir, err = filepath.EvalSymlinks(workdir)
	if err != nil {
		return err
	}
	execution := s.executionConfigSnapshot(project.RemoteRepoURL)
	readPaths := []string{workdir}
	gitCommonPath := ""
	run := func(args []string, writes bool) (string, error) {
		writePaths := []string(nil)
		if writes {
			writePaths = []string{workdir}
			if gitCommonPath != "" {
				writePaths = append(writePaths, gitCommonPath)
			}
		}
		stdout, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, execution, "git", args, workdir, readPaths, writePaths, false, 180*time.Second)
		if runErr != nil || cleanupErr != nil {
			var commandErr error
			if runErr != nil {
				commandErr = fmt.Errorf("sandboxed Git %s failed: %w: %s", args[0], runErr, strings.TrimSpace(stderr))
			}
			return "", errors.Join(commandErr, cleanupErr)
		}
		return strings.TrimSpace(stdout), nil
	}
	gitCommon, err := run([]string{"rev-parse", "--git-common-dir"}, false)
	if err != nil {
		return fmt.Errorf("resolve project Git common directory for LFS import: %w", err)
	}
	if !filepath.IsAbs(gitCommon) {
		gitCommon = filepath.Join(workdir, gitCommon)
	}
	gitCommon, err = filepath.EvalSymlinks(filepath.Clean(gitCommon))
	if err != nil {
		return fmt.Errorf("resolve project Git common directory links for LFS import: %w", err)
	}
	gitDirText, err := run([]string{"rev-parse", "--absolute-git-dir"}, false)
	if err != nil {
		return fmt.Errorf("resolve project Git metadata directory for LFS import: %w", err)
	}
	gitDir, err := filepath.EvalSymlinks(filepath.Clean(gitDirText))
	if err != nil || gitDir != gitCommon && !pathWithin(gitCommon, gitDir) {
		return errors.Join(fmt.Errorf("project Git worktree metadata does not belong to its common repository: %w", domain.ErrConflict), err)
	}
	gitCommonPath = gitCommon
	readPaths = append(readPaths, gitCommon, gitDir)
	listing, err := run([]string{"lfs", "ls-files", "--long", strings.ToLower(commitSHA)}, false)
	if err != nil {
		configured, inspectErr := gitLFSConfiguredAtCommit(func(args []string) (string, error) { return run(args, false) }, commitSHA)
		if inspectErr != nil {
			return errors.Join(fmt.Errorf("inspect Git LFS metadata after listing failed: %w", inspectErr), err)
		}
		if !configured {
			if archive != nil {
				return fmt.Errorf("task supplied Git LFS objects for a project without Git LFS attributes: %w", domain.ErrConflict)
			}
			return nil
		}
		return fmt.Errorf("list project Git LFS objects before import: %w", err)
	}
	objects, err := parseProjectLFSListing(listing)
	if err != nil {
		return err
	}
	baseListing, err := run([]string{"lfs", "ls-files", "--long", strings.ToLower(baseSHA)}, false)
	if err != nil {
		return fmt.Errorf("list base commit Git LFS objects: %w", err)
	}
	baseObjects, err := parseProjectLFSListing(baseListing)
	if err != nil {
		return err
	}
	baseSet := make(map[string]bool, len(baseObjects))
	for _, oid := range baseObjects {
		baseSet[oid] = true
	}
	changedObjects := make([]string, 0)
	for _, oid := range objects {
		if !baseSet[oid] {
			changedObjects = append(changedObjects, oid)
		}
	}
	if len(changedObjects) == 0 {
		if archive != nil {
			return fmt.Errorf("task supplied Git LFS objects for a commit with no newly introduced LFS pointers: %w", domain.ErrConflict)
		}
		return nil
	}
	if _, err := run([]string{"config", "lfs.storage", filepath.Join(gitCommon, "lfs")}, true); err != nil {
		return fmt.Errorf("bind project LFS import to durable common storage: %w", err)
	}
	mediaRoot := filepath.Join(gitCommon, "lfs", "objects")
	if err := ensureSafeProjectLFSDirectory(mediaRoot); err != nil {
		return err
	}
	missing := make(map[string]bool, len(changedObjects))
	for _, oid := range changedObjects {
		if err := verifyProjectLFSObject(filepath.Join(mediaRoot, oid[:2], oid[2:4], oid), oid, -1); err != nil {
			missing[oid] = true
		}
	}
	if len(missing) > 0 && archive == nil {
		return fmt.Errorf("task Git LFS objects are absent and no LFS archive was supplied: %w", domain.ErrConflict)
	}
	if archive != nil {
		expectedObjects := make(map[string]bool, len(changedObjects))
		for _, oid := range changedObjects {
			expectedObjects[oid] = true
		}
		hasher := sha256.New()
		counter := &projectLFSCountingReader{reader: io.LimitReader(archive, archiveSize+1)}
		stream := io.TeeReader(counter, hasher)
		if err := importProjectLFSArchive(stream, mediaRoot, expectedObjects); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, stream); err != nil {
			return fmt.Errorf("finish reading task Git LFS archive: %w", err)
		}
		if counter.count != archiveSize || hex.EncodeToString(hasher.Sum(nil)) != strings.ToLower(archiveSHA) {
			return fmt.Errorf("task Git LFS archive size or SHA-256 did not match its manifest: %w", domain.ErrConflict)
		}
	}
	if _, err := run([]string{"lfs", "checkout"}, true); err != nil {
		return fmt.Errorf("hydrate task project files from verified Git LFS objects: %w", err)
	}
	readBack, err := run([]string{"lfs", "ls-files", "--long", strings.ToLower(commitSHA)}, false)
	if err != nil {
		return fmt.Errorf("read back hydrated project Git LFS files: %w", err)
	}
	verified, err := parseProjectLFSListing(readBack)
	if err != nil {
		return err
	}
	if len(verified) != len(objects) {
		return fmt.Errorf("hydrated Git LFS object set did not read back completely: %w", domain.ErrConflict)
	}
	for _, oid := range changedObjects {
		if err := verifyProjectLFSObject(filepath.Join(mediaRoot, oid[:2], oid[2:4], oid), oid, -1); err != nil {
			return err
		}
	}
	return s.VerifyProjectLFSImport(ctx, project, baseSHA, commitSHA)
}

// VerifyProjectLFSImport is a read-only gate used before an imported task
// branch is exposed to another task or continuation.
func (s *Service) VerifyProjectLFSImport(ctx context.Context, project domain.Project, baseSHA, commitSHA string) error {
	if s == nil || project.ID == "" || project.Workdir == "" || !validGitObjectID(strings.ToLower(baseSHA)) || !validGitObjectID(strings.ToLower(commitSHA)) {
		return domain.ErrInvalid
	}
	workdir, err := filepath.Abs(filepath.Clean(project.Workdir))
	if err != nil {
		return err
	}
	workdir, err = filepath.EvalSymlinks(workdir)
	if err != nil {
		return err
	}
	execution := s.executionConfigSnapshot(project.RemoteRepoURL)
	readPaths := []string{workdir}
	run := func(args []string) (string, error) {
		stdout, stderr, runErr, cleanupErr := s.runSandboxedCommand(ctx, execution, "git", args, workdir, readPaths, nil, false, 180*time.Second)
		if runErr != nil || cleanupErr != nil {
			var commandErr error
			if runErr != nil {
				commandErr = fmt.Errorf("sandboxed Git %s failed: %w: %s", args[0], runErr, strings.TrimSpace(stderr))
			}
			return "", errors.Join(commandErr, cleanupErr)
		}
		return strings.TrimSpace(stdout), nil
	}
	commonDirText, err := run([]string{"rev-parse", "--git-common-dir"})
	if err != nil {
		return fmt.Errorf("resolve imported repository LFS storage: %w", err)
	}
	if !filepath.IsAbs(commonDirText) {
		commonDirText = filepath.Join(workdir, commonDirText)
	}
	commonDir, err := filepath.EvalSymlinks(filepath.Clean(commonDirText))
	if err != nil {
		return err
	}
	gitDirText, err := run([]string{"rev-parse", "--absolute-git-dir"})
	if err != nil {
		return err
	}
	gitDir, err := filepath.EvalSymlinks(filepath.Clean(gitDirText))
	if err != nil || gitDir != commonDir && !pathWithin(commonDir, gitDir) {
		return errors.Join(fmt.Errorf("project Git worktree metadata does not belong to its common repository: %w", domain.ErrConflict), err)
	}
	readPaths = append(readPaths, commonDir, gitDir)
	commitListing, err := run([]string{"lfs", "ls-files", "--long", strings.ToLower(commitSHA)})
	if err != nil {
		configured, inspectErr := gitLFSConfiguredAtCommit(run, commitSHA)
		if inspectErr != nil {
			return errors.Join(fmt.Errorf("inspect Git LFS metadata after listing failed: %w", inspectErr), err)
		}
		if !configured {
			return nil
		}
		return fmt.Errorf("read imported commit Git LFS manifest: %w", err)
	}
	changedFiles, err := changedProjectLFSFiles(func(args []string) (string, error) {
		return run(args)
	}, baseSHA, commitSHA, commitListing)
	if err != nil {
		return err
	}
	changed := make(map[string]bool)
	for _, file := range changedFiles {
		changed[file.OID] = true
	}
	if len(changed) == 0 {
		return nil
	}
	mediaRoot := filepath.Join(commonDir, "lfs", "objects")
	for oid := range changed {
		if err := verifyProjectLFSObject(filepath.Join(mediaRoot, oid[:2], oid[2:4], oid), oid, -1); err != nil {
			return err
		}
	}
	for _, fileEntry := range changedFiles {
		oid := fileEntry.OID
		if !changed[oid] {
			continue
		}
		if !fileEntry.Hydrated {
			return fmt.Errorf("imported Git LFS file %s is not hydrated: %w", oid, domain.ErrConflict)
		}
		path := filepath.Join(workdir, filepath.FromSlash(fileEntry.Path))
		if !pathWithin(workdir, path) {
			return fmt.Errorf("imported Git LFS path escapes its worktree: %w", domain.ErrConflict)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(fmt.Errorf("imported Git LFS file did not read back safely: %w", domain.ErrConflict), err)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hasher := sha256.New()
		_, hashErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if hashErr != nil || closeErr != nil || hex.EncodeToString(hasher.Sum(nil)) != oid {
			return errors.Join(fmt.Errorf("imported Git LFS file content did not match its pointer: %w", domain.ErrConflict), hashErr, closeErr)
		}
	}
	return nil
}

type projectLFSFile struct {
	OID      string
	Path     string
	Hydrated bool
}

func changedProjectLFSFiles(run func([]string) (string, error), baseSHA, commitSHA, commitListing string) ([]projectLFSFile, error) {
	if run == nil {
		return nil, domain.ErrInvalid
	}
	files, err := parseProjectLFSFiles(commitListing)
	if err != nil {
		return nil, err
	}
	changedOutput, err := run([]string{"diff", "--name-only", "-z", strings.ToLower(baseSHA), strings.ToLower(commitSHA)})
	if err != nil {
		return nil, fmt.Errorf("read changed project paths for Git LFS synchronization: %w", err)
	}
	changedPaths := make(map[string]bool)
	for _, path := range strings.Split(changedOutput, "\x00") {
		if path != "" {
			changedPaths[filepath.ToSlash(path)] = true
		}
	}
	selected := make([]projectLFSFile, 0)
	for _, file := range files {
		if changedPaths[file.Path] {
			selected = append(selected, file)
		}
	}
	return selected, nil
}

func parseProjectLFSFiles(listing string) ([]projectLFSFile, error) {
	files := make([]projectLFSFile, 0)
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		oid := strings.ToLower(fields[0])
		if len(fields) < 3 || len(oid) != sha256.Size*2 {
			return nil, fmt.Errorf("Git LFS listing contains an invalid object reference: %w", domain.ErrConflict)
		}
		if _, err := hex.DecodeString(oid); err != nil || fields[1] != "*" && fields[1] != "-" {
			return nil, fmt.Errorf("Git LFS listing contains invalid object metadata: %w", domain.ErrConflict)
		}
		prefix := fields[0] + " " + fields[1]
		at := strings.Index(line, prefix)
		if at < 0 {
			return nil, fmt.Errorf("Git LFS listing has no file path: %w", domain.ErrConflict)
		}
		path := strings.TrimSpace(line[at+len(prefix):])
		if strings.HasPrefix(path, "\"") && strings.HasSuffix(path, "\"") {
			unquoted, err := strconv.Unquote(path)
			if err != nil {
				return nil, fmt.Errorf("decode Git LFS file path: %w", err)
			}
			path = unquoted
		}
		if path == "" || filepath.IsAbs(path) {
			return nil, fmt.Errorf("Git LFS listing contains an invalid file path: %w", domain.ErrConflict)
		}
		files = append(files, projectLFSFile{OID: oid, Path: filepath.ToSlash(path), Hydrated: fields[1] == "*"})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Path == files[j].Path {
			return files[i].OID < files[j].OID
		}
		return files[i].Path < files[j].Path
	})
	return files, nil
}

func parseProjectLFSListing(listing string) ([]string, error) {
	files, err := parseProjectLFSFiles(listing)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(files))
	objects := make([]string, 0, len(files))
	for _, file := range files {
		if !seen[file.OID] {
			seen[file.OID] = true
			objects = append(objects, file.OID)
		}
	}
	sort.Strings(objects)
	return objects, nil
}

func ensureSafeProjectLFSDirectory(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	current := volume + string(os.PathSeparator)
	rest := strings.TrimPrefix(clean, current)
	for _, part := range strings.Split(rest, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("create Git LFS object directory: %w", err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(fmt.Errorf("Git LFS object directory is unsafe: %w", domain.ErrConflict), err)
		}
	}
	return nil
}

func verifyProjectLFSObject(path, oid string, expectedSize int64) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || expectedSize >= 0 && info.Size() != expectedSize {
		return errors.Join(fmt.Errorf("Git LFS object %s did not read back safely: %w", oid, domain.ErrConflict), err)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	_, hashErr := io.Copy(hasher, f)
	closeErr := f.Close()
	if hashErr != nil || closeErr != nil || hex.EncodeToString(hasher.Sum(nil)) != oid {
		return errors.Join(fmt.Errorf("Git LFS object %s failed its content hash: %w", oid, domain.ErrConflict), hashErr, closeErr)
	}
	return nil
}

type projectLFSCountingReader struct {
	reader io.Reader
	count  int64
}

func (r *projectLFSCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}

func importProjectLFSArchive(archive io.Reader, mediaRoot string, expected map[string]bool) (retErr error) {
	tr := tar.NewReader(archive)
	seen := make(map[string]bool, len(expected))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read task Git LFS archive: %w", err)
		}
		oid := strings.TrimPrefix(header.Name, "objects/")
		if header.Name != "objects/"+oid || header.Typeflag != tar.TypeReg || len(oid) != 64 || seen[oid] || !expected[oid] {
			return fmt.Errorf("task Git LFS archive contains an unexpected or duplicate member: %w", domain.ErrConflict)
		}
		if _, err := hex.DecodeString(oid); err != nil || header.Size < 0 {
			return fmt.Errorf("task Git LFS archive has invalid object metadata: %w", domain.ErrConflict)
		}
		destination := filepath.Join(mediaRoot, oid[:2], oid[2:4], oid)
		if err := ensureSafeProjectLFSDirectory(filepath.Dir(destination)); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(destination), ".o-agent-lfs-object-*.tmp")
		if err != nil {
			return fmt.Errorf("stage Git LFS object %s: %w", oid, err)
		}
		tmpPath := tmp.Name()
		tmpClosed, published := false, false
		defer func() {
			if !tmpClosed {
				retErr = errors.Join(retErr, tmp.Close())
			}
			if !published {
				removeErr := os.Remove(tmpPath)
				if !errors.Is(removeErr, os.ErrNotExist) {
					retErr = errors.Join(retErr, fmt.Errorf("remove staged Git LFS object %s: %w", oid, removeErr))
				}
			}
		}()
		hasher := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(tmp, hasher), tr, header.Size)
		if copyErr != nil || written != header.Size || hex.EncodeToString(hasher.Sum(nil)) != oid {
			return errors.Join(fmt.Errorf("task Git LFS object %s failed verification: %w", oid, domain.ErrConflict), copyErr)
		}
		if err := tmp.Sync(); err != nil {
			return err
		}
		closeErr := tmp.Close()
		tmpClosed = true
		if closeErr != nil {
			return closeErr
		}
		if err := os.Chmod(tmpPath, 0o600); err != nil {
			return err
		}
		if existingErr := verifyProjectLFSObject(destination, oid, header.Size); existingErr == nil {
			if err := os.Remove(tmpPath); err != nil {
				return fmt.Errorf("remove duplicate staged Git LFS object %s: %w", oid, err)
			}
			published = true
		} else if !errors.Is(existingErr, os.ErrNotExist) {
			// A corrupt content-addressed object is retained for diagnosis; never overwrite it.
			return existingErr
		} else {
			if err := os.Rename(tmpPath, destination); err != nil {
				return err
			}
			published = true
			if err := verifyProjectLFSObject(destination, oid, header.Size); err != nil {
				return err
			}
		}
		seen[oid] = true
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("task Git LFS archive is missing required objects: %w", domain.ErrConflict)
	}
	return nil
}

// createProjectDeltaLFSArchive snapshots the hydrated LFS objects referenced by
// a Git commit. Git bundles contain pointer blobs only; keeping this archive
// beside the task delta makes the binary content independently transferable.
func createProjectDeltaLFSArchive(ctx context.Context, taskID, baseSHA, commitSHA, commonGitDir string, runGit func([]string, bool) (string, error)) (result string, retErr error) {
	if !validProjectTaskID(taskID) || !validGitObjectID(strings.ToLower(baseSHA)) || !validGitObjectID(strings.ToLower(commitSHA)) || runGit == nil {
		return "", domain.ErrInvalid
	}
	listing, err := runGit([]string{"lfs", "ls-files", "--long", strings.ToLower(commitSHA)}, false)
	if err != nil {
		configured, inspectErr := gitLFSConfiguredAtCommit(func(args []string) (string, error) { return runGit(args, false) }, commitSHA)
		if inspectErr != nil {
			return "", errors.Join(fmt.Errorf("inspect Git LFS metadata after listing failed: %w", inspectErr), err)
		}
		if !configured {
			return "", nil
		}
		return "", fmt.Errorf("list Git LFS objects for project commit: %w", err)
	}
	changedFiles, err := changedProjectLFSFiles(func(args []string) (string, error) {
		return runGit(args, false)
	}, baseSHA, commitSHA, listing)
	if err != nil {
		return "", err
	}
	objects := make([]string, 0, len(changedFiles))
	seen := make(map[string]bool)
	hydrated := make(map[string]bool)
	for _, file := range changedFiles {
		if !seen[file.OID] {
			seen[file.OID] = true
			objects = append(objects, file.OID)
			hydrated[file.OID] = file.Hydrated
		}
	}
	if len(objects) == 0 {
		return "", nil
	}
	for _, oid := range objects {
		if !hydrated[oid] {
			return "", fmt.Errorf("new Git LFS object %s is not hydrated on this node; task results remain incomplete: %w", oid, domain.ErrConflict)
		}
	}
	sort.Strings(objects)
	mediaDir, err := runGit([]string{"lfs", "env"}, false)
	if err != nil {
		return "", fmt.Errorf("locate durable Git LFS object storage: %w", err)
	}
	mediaRoot := ""
	for _, line := range strings.Split(mediaDir, "\n") {
		if strings.HasPrefix(line, "LocalMediaDir=") {
			mediaRoot = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "LocalMediaDir=")), "\"'")
			break
		}
	}
	if mediaRoot == "" {
		return "", fmt.Errorf("Git LFS did not report a local media directory: %w", domain.ErrConflict)
	}
	if !filepath.IsAbs(mediaRoot) {
		mediaRoot = filepath.Join(commonGitDir, mediaRoot)
	}
	mediaRoot, err = filepath.Abs(filepath.Clean(mediaRoot))
	if err != nil {
		return "", fmt.Errorf("resolve Git LFS media directory: %w", err)
	}
	archivePath := filepath.Join(commonGitDir, "o-agent-lfs-"+taskID+".tar")
	if info, statErr := os.Lstat(archivePath); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("existing task LFS archive is not a regular file: %w", domain.ErrConflict)
		}
		if err := verifyProjectLFSArchive(archivePath, mediaRoot, objects); err == nil {
			return archivePath, nil
		}
		return "", fmt.Errorf("existing task LFS archive did not verify against the project commit: %w", err)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect task LFS archive: %w", statErr)
	}
	tmp, err := os.CreateTemp(commonGitDir, ".o-agent-lfs-"+taskID+"-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create staged task LFS archive: %w", err)
	}
	tmpPath := tmp.Name()
	published := false
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			retErr = errors.Join(retErr, tmp.Close())
		}
		if !published {
			removeErr := os.Remove(tmpPath)
			if !errors.Is(removeErr, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("remove staged task LFS archive: %w", removeErr))
			}
		}
	}()
	tw := tar.NewWriter(tmp)
	for _, oid := range objects {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		objectPath := filepath.Join(mediaRoot, oid[:2], oid[2:4], oid)
		objectInfo, err := os.Lstat(objectPath)
		if err != nil || !objectInfo.Mode().IsRegular() || objectInfo.Mode()&os.ModeSymlink != 0 {
			return "", errors.Join(fmt.Errorf("Git LFS object %s is missing or unsafe: %w", oid, domain.ErrConflict), err)
		}
		object, err := os.Open(objectPath)
		if err != nil {
			return "", fmt.Errorf("open Git LFS object %s: %w", oid, err)
		}
		header := &tar.Header{Name: "objects/" + oid, Mode: 0o600, Size: objectInfo.Size(), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(header); err != nil {
			return "", errors.Join(fmt.Errorf("write Git LFS archive header: %w", err), object.Close())
		}
		hasher := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(tw, hasher), object, objectInfo.Size())
		closeErr := object.Close()
		if copyErr != nil || closeErr != nil || written != objectInfo.Size() {
			return "", fmt.Errorf("copy Git LFS object %s into archive: %w", oid, errors.Join(copyErr, closeErr))
		}
		if hex.EncodeToString(hasher.Sum(nil)) != oid {
			return "", fmt.Errorf("Git LFS object %s failed its content hash check: %w", oid, domain.ErrConflict)
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("finish task LFS archive: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("flush task LFS archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		tmpClosed = true
		return "", fmt.Errorf("close task LFS archive: %w", err)
	}
	tmpClosed = true
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return "", fmt.Errorf("restrict task LFS archive permissions: %w", err)
	}
	if err := os.Rename(tmpPath, archivePath); err != nil {
		return "", fmt.Errorf("publish task LFS archive: %w", err)
	}
	published = true
	if err := verifyProjectLFSArchive(archivePath, mediaRoot, objects); err != nil {
		removeErr := os.Remove(archivePath)
		return "", errors.Join(fmt.Errorf("read back task LFS archive: %w", err), removeErr)
	}
	return archivePath, nil
}

func gitLFSConfiguredAtCommit(run func([]string) (string, error), commitSHA string) (bool, error) {
	if run == nil || !validGitObjectID(strings.ToLower(commitSHA)) {
		return false, domain.ErrInvalid
	}
	paths, err := run([]string{"ls-tree", "-r", "--name-only", strings.ToLower(commitSHA), "--", ".gitattributes", "*.gitattributes"})
	if err != nil {
		return false, fmt.Errorf("list committed Git attributes: %w", err)
	}
	seen := make(map[string]bool)
	for _, path := range strings.Split(paths, "\n") {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		content, err := run([]string{"show", strings.ToLower(commitSHA) + ":" + path})
		if err != nil {
			return false, fmt.Errorf("read committed Git attributes %q: %w", path, err)
		}
		if strings.Contains(content, "filter=lfs") {
			return true, nil
		}
	}
	return false, nil
}

func verifyProjectLFSArchive(archivePath, mediaRoot string, expected []string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	seen := make(map[string]bool, len(expected))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		oid := strings.TrimPrefix(header.Name, "objects/")
		if header.Typeflag != tar.TypeReg || header.Name != "objects/"+oid || len(oid) != 64 || seen[oid] {
			return fmt.Errorf("task LFS archive contains an unexpected entry: %w", domain.ErrConflict)
		}
		if _, err := hex.DecodeString(oid); err != nil {
			return fmt.Errorf("task LFS archive contains a malformed object ID: %w", domain.ErrConflict)
		}
		index := sort.SearchStrings(expected, oid)
		if index >= len(expected) || expected[index] != oid {
			return fmt.Errorf("task LFS archive contains an unreferenced object: %w", domain.ErrConflict)
		}
		hasher := sha256.New()
		written, err := io.CopyN(hasher, tr, header.Size)
		if err != nil || written != header.Size || hex.EncodeToString(hasher.Sum(nil)) != oid {
			return errors.Join(fmt.Errorf("task LFS archive object %s failed content verification: %w", oid, domain.ErrConflict), err)
		}
		if mediaRoot != "" {
			path := filepath.Join(mediaRoot, oid[:2], oid[2:4], oid)
			info, statErr := os.Lstat(path)
			if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != header.Size {
				return errors.Join(fmt.Errorf("source Git LFS object %s did not read back: %w", oid, domain.ErrConflict), statErr)
			}
		}
		seen[oid] = true
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("task LFS archive is missing referenced objects: %w", domain.ErrConflict)
	}
	return nil
}
