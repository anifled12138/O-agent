package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"axiom.local/agent/internal/coretools"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

func TestCreateProjectDeltaBundlePreservesUnpublishedSubmoduleWorktree(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	child := filepath.Join(parent, "libs", "core")
	helper := filepath.Join(child, "deps", "helper")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, child, "init")
	runGitTest(t, ctx, git, child, "config", "user.name", "Test User")
	runGitTest(t, ctx, git, child, "config", "user.email", "test@example.invalid")
	runGitTest(t, ctx, git, child, "remote", "add", "origin", "https://github.com/example/core.git")
	if err := os.WriteFile(filepath.Join(child, "core.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(helper, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, helper, "init")
	runGitTest(t, ctx, git, helper, "config", "user.name", "Test User")
	runGitTest(t, ctx, git, helper, "config", "user.email", "test@example.invalid")
	runGitTest(t, ctx, git, helper, "remote", "add", "origin", "https://github.com/example/helper.git")
	if err := os.WriteFile(filepath.Join(helper, "helper.txt"), []byte("nested base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, helper, "add", "--all")
	runGitTest(t, ctx, git, helper, "commit", "-m", "nested submodule base")
	baseHelper := runGitTest(t, ctx, git, helper, "rev-parse", "HEAD")
	helperBaseRead := exec.CommandContext(ctx, git, "-C", helper, "cat-file", "blob", baseHelper+":helper.txt")
	helperBaseContent, err := helperBaseRead.Output()
	if err != nil {
		t.Fatal(err)
	}
	helperRepository := filepath.Join(root, "helper-origin.git")
	runGitTest(t, ctx, git, root, "clone", "--bare", helper, helperRepository)
	if err := os.WriteFile(filepath.Join(child, ".gitmodules"), []byte("[submodule \"helper\"]\n\tpath = deps/helper\n\turl = https://github.com/example/helper.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, child, "add", "--all")
	runGitTest(t, ctx, git, child, "update-index", "--add", "--cacheinfo", "160000,"+baseHelper+",deps/helper")
	runGitTest(t, ctx, git, child, "submodule", "absorbgitdirs", "--", "deps/helper")
	runGitTest(t, ctx, git, child, "commit", "-m", "submodule base")
	baseChild := runGitTest(t, ctx, git, child, "rev-parse", "HEAD")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, parent, "init")
	runGitTest(t, ctx, git, parent, "config", "user.name", "Test User")
	runGitTest(t, ctx, git, parent, "config", "user.email", "test@example.invalid")
	runGitTest(t, ctx, git, parent, "remote", "add", "origin", "https://github.com/example/app.git")
	if err := os.WriteFile(filepath.Join(parent, ".gitmodules"), []byte("[submodule \"core\"]\n\tpath = libs/core\n\turl = https://github.com/example/core.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "README.md"), []byte("parent base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, ctx, git, parent, "add", ".gitmodules", "README.md")
	runGitTest(t, ctx, git, parent, "update-index", "--add", "--cacheinfo", "160000,"+baseChild+",libs/core")
	runGitTest(t, ctx, git, parent, "submodule", "absorbgitdirs", "--", "libs/core")
	runGitTest(t, ctx, git, parent, "commit", "-m", "parent base")
	baseParent := runGitTest(t, ctx, git, parent, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(child, "core.txt"), []byte("unpublished child change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "new.txt"), []byte("untracked child output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(helper, "helper.txt"), []byte("unpublished nested change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(helper, "new-helper.txt"), []byte("nested untracked output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parentHeadBefore := runGitTest(t, ctx, git, parent, "rev-parse", "HEAD")
	childStatusBefore := runGitTest(t, ctx, git, child, "status", "--porcelain=v1", "-z")
	service := &Service{sandboxCommand: func(ctx context.Context, _ coretools.ExecutionConfig, command string, args []string, dir string, _, _ []string, _ bool, _ time.Duration) (string, string, error, error) {
		for index := 0; index+1 < len(args); index++ {
			if args[index] == "submodule" && args[index+1] == "update" {
				destination := filepath.Join(dir, "libs", "core")
				if err := os.RemoveAll(destination); err != nil {
					return "", "", err, nil
				}
				clone := exec.CommandContext(ctx, "git", "clone", "--no-checkout", child, destination)
				if output, err := clone.CombinedOutput(); err != nil {
					return "", string(output), err, nil
				}
				checkout := exec.CommandContext(ctx, "git", "-C", destination, "checkout", "--force", "--detach", baseChild)
				if output, err := checkout.CombinedOutput(); err != nil {
					return "", string(output), err, nil
				}
				if err := os.WriteFile(filepath.Join(destination, "core.txt"), []byte("base\n"), 0o600); err != nil {
					return "", "", err, nil
				}
				if err := os.WriteFile(filepath.Join(destination, ".gitmodules"), []byte("[submodule \"helper\"]\n\tpath = deps/helper\n\turl = https://github.com/example/helper.git\n"), 0o600); err != nil {
					return "", "", err, nil
				}
				nestedDestination := filepath.Join(destination, "deps", "helper")
				if err := os.MkdirAll(filepath.Dir(nestedDestination), 0o700); err != nil {
					return "", "", err, nil
				}
				nestedClone := exec.CommandContext(ctx, "git", "clone", "--no-checkout", helperRepository, nestedDestination)
				if output, err := nestedClone.CombinedOutput(); err != nil {
					return "", string(output), err, nil
				}
				nestedCheckout := exec.CommandContext(ctx, "git", "-C", nestedDestination, "checkout", "--force", "--detach", baseHelper)
				if output, err := nestedCheckout.CombinedOutput(); err != nil {
					return "", string(output), err, nil
				}
				if err := os.WriteFile(filepath.Join(nestedDestination, "helper.txt"), helperBaseContent, 0o600); err != nil {
					return "", "", err, nil
				}
				setNestedOrigin := exec.CommandContext(ctx, "git", "-C", nestedDestination, "remote", "set-url", "origin", "https://github.com/example/helper.git")
				if output, err := setNestedOrigin.CombinedOutput(); err != nil {
					return "", string(output), err, nil
				}
				setOrigin := exec.CommandContext(ctx, "git", "-C", destination, "remote", "set-url", "origin", "https://github.com/example/core.git")
				if output, err := setOrigin.CombinedOutput(); err != nil {
					return "", string(output), err, nil
				}
				return "", "", nil, nil
			}
		}
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err, nil
	}}
	delta, err := service.CreateProjectDeltaBundle(ctx, domain.Project{ID: "parent-project", Workdir: parent, RemoteRepoURL: "https://github.com/example/app.git", ResolvedCommit: baseParent}, "task_submodule_delta")
	if err != nil {
		t.Fatal(err)
	}
	if delta.Path == "" || delta.SubmoduleDeltasPath == "" || delta.Commit == baseParent {
		t.Fatalf("submodule-only delta was not made transferable: %+v", delta)
	}
	retry, err := service.CreateProjectDeltaBundle(ctx, domain.Project{ID: "parent-project", Workdir: parent, RemoteRepoURL: "https://github.com/example/app.git", ResolvedCommit: baseParent}, "task_submodule_delta")
	if err != nil || retry.Path != delta.Path || retry.Commit != delta.Commit || retry.SubmoduleDeltasPath != delta.SubmoduleDeltasPath {
		t.Fatalf("submodule delta did not survive idempotent retry: first=%+v retry=%+v err=%v", delta, retry, err)
	}
	if got := runGitTest(t, ctx, git, parent, "rev-parse", "HEAD"); got != parentHeadBefore {
		t.Fatalf("snapshot changed parent HEAD: got %s want %s", got, parentHeadBefore)
	}
	if got := runGitTest(t, ctx, git, child, "status", "--porcelain=v1", "-z"); got != childStatusBefore {
		t.Fatalf("snapshot changed child worktree: before=%q after=%q", childStatusBefore, got)
	}
	archive, err := os.Open(delta.SubmoduleDeltasPath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	reader := tar.NewReader(archive)
	entries := map[string][]byte{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = data
	}
	if len(entries) != 5 || string(entries["data/00000001"]) != "unpublished child change\n" || string(entries["data/00000002"]) != "untracked child output\n" || string(entries["data/00000003"]) != "unpublished nested change\n" || string(entries["data/00000004"]) != "nested untracked output\n" || len(entries["manifest.json"]) == 0 {
		t.Fatalf("submodule archive did not capture exact changed content: %#v", entries)
	}
	var archivedManifest projectSubmoduleDeltaManifest
	if err := json.Unmarshal(entries["manifest.json"], &archivedManifest); err != nil || len(archivedManifest.Modules) != 2 || archivedManifest.Modules[0].Path != "libs/core" || archivedManifest.Modules[1].Path != "libs/core/deps/helper" {
		t.Fatalf("submodule archive did not include the nested repository delta: manifest=%+v err=%v", archivedManifest, err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	bundleBytes, err := os.ReadFile(delta.Path)
	if err != nil {
		t.Fatal(err)
	}
	bundleDigest := sha256.Sum256(bundleBytes)
	submoduleBytes, err := os.ReadFile(delta.SubmoduleDeltasPath)
	if err != nil {
		t.Fatal(err)
	}
	submoduleDigest := sha256.Sum256(submoduleBytes)
	state := filepath.Join(root, "cloud-state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := store.EnsureLocalWorkspaceOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service.store = store
	service.workspaceRoot = filepath.Join(root, "cloud-projects")
	source := domain.Project{ID: "cloud-source", UserID: owner, Name: "app", Workdir: parent, RemoteRepoURL: "https://github.com/example/app.git", ResolvedCommit: baseParent, RemoteBranch: "main"}
	badDigest := sha256.Sum256([]byte("wrong archive"))
	if _, _, err := service.ImportProjectDeltaBundleWithArtifacts(ctx, owner, source, "task_submodule_delta", bytes.NewReader(bundleBytes), int64(len(bundleBytes)), hex.EncodeToString(bundleDigest[:]), baseParent, delta.Commit, nil, 0, "", bytes.NewReader(submoduleBytes), int64(len(submoduleBytes)), hex.EncodeToString(badDigest[:])); err == nil {
		t.Fatal("tampered submodule archive was accepted")
	}
	if _, err := store.Project(ctx, owner, ProjectDeltaProjectID(source.ID, "task_submodule_delta")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed submodule import did not roll back its task project: %v", err)
	}
	imported, _, err := service.ImportProjectDeltaBundleWithArtifacts(ctx, owner, source, "task_submodule_delta", bytes.NewReader(bundleBytes), int64(len(bundleBytes)), hex.EncodeToString(bundleDigest[:]), baseParent, delta.Commit, nil, 0, "", bytes.NewReader(submoduleBytes), int64(len(submoduleBytes)), hex.EncodeToString(submoduleDigest[:]))
	if err != nil {
		t.Fatal(err)
	}
	for relative, want := range map[string]string{"libs/core/core.txt": "unpublished child change\n", "libs/core/new.txt": "untracked child output\n", "libs/core/deps/helper/helper.txt": "unpublished nested change\n", "libs/core/deps/helper/new-helper.txt": "nested untracked output\n"} {
		got, err := os.ReadFile(filepath.Join(imported.Workdir, filepath.FromSlash(relative)))
		if err != nil || string(got) != want {
			t.Fatalf("imported submodule file %s = %q, err=%v; want %q", relative, got, err, want)
		}
	}
	retriedImport, created, err := service.ImportProjectDeltaBundleWithArtifacts(ctx, owner, source, "task_submodule_delta", bytes.NewReader(bundleBytes), int64(len(bundleBytes)), hex.EncodeToString(bundleDigest[:]), baseParent, delta.Commit, nil, 0, "", bytes.NewReader(submoduleBytes), int64(len(submoduleBytes)), hex.EncodeToString(submoduleDigest[:]))
	if err != nil || created || retriedImport.Workdir != imported.Workdir {
		t.Fatalf("idempotent nested submodule import did not read back the existing task project: project=%+v created=%t err=%v", retriedImport, created, err)
	}
	if err := os.WriteFile(filepath.Join(child, "core.txt"), []byte("later work must not rewrite a task snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateProjectDeltaBundle(ctx, domain.Project{ID: "parent-project", Workdir: parent, RemoteRepoURL: "https://github.com/example/app.git", ResolvedCommit: baseParent}, "task_submodule_delta"); err == nil {
		t.Fatal("retry replaced an immutable task submodule snapshot with later work")
	}
	if err := os.RemoveAll(helper); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, ".gitmodules"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	removalDelta, err := service.CreateProjectDeltaBundle(ctx, domain.Project{ID: "parent-project", Workdir: parent, RemoteRepoURL: "https://github.com/example/app.git", ResolvedCommit: baseParent}, "task_submodule_remove")
	if err != nil {
		t.Fatal(err)
	}
	removalBundle, err := os.ReadFile(removalDelta.Path)
	if err != nil {
		t.Fatal(err)
	}
	removalArchive, err := os.ReadFile(removalDelta.SubmoduleDeltasPath)
	if err != nil {
		t.Fatal(err)
	}
	var removalManifest projectSubmoduleDeltaManifest
	removalTar := tar.NewReader(bytes.NewReader(removalArchive))
	for {
		header, nextErr := removalTar.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if header.Name == "manifest.json" {
			encoded, err := io.ReadAll(removalTar)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &removalManifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(removalManifest.Modules) != 1 || len(removalManifest.Modules[0].Files) == 0 {
		t.Fatalf("nested submodule removal was missing from the task sidecar: %+v", removalManifest)
	}
	var removedNested bool
	for _, record := range removalManifest.Modules[0].Files {
		if record.Path == "deps/helper" && record.Action == "remove_submodule" && record.SubmoduleBaseCommit == baseHelper {
			removedNested = true
		}
	}
	if !removedNested {
		t.Fatalf("nested gitlink removal was not explicitly represented: %+v", removalManifest.Modules[0].Files)
	}
	removalBundleDigest := sha256.Sum256(removalBundle)
	removalArchiveDigest := sha256.Sum256(removalArchive)
	removedProject, created, err := service.ImportProjectDeltaBundleWithArtifacts(ctx, owner, source, "task_submodule_remove", bytes.NewReader(removalBundle), int64(len(removalBundle)), hex.EncodeToString(removalBundleDigest[:]), baseParent, removalDelta.Commit, nil, 0, "", bytes.NewReader(removalArchive), int64(len(removalArchive)), hex.EncodeToString(removalArchiveDigest[:]))
	if err != nil || !created {
		t.Fatalf("nested gitlink removal did not import as a new task project: project=%+v created=%t err=%v", removedProject, created, err)
	}
	if _, err := os.Lstat(filepath.Join(removedProject.Workdir, "libs", "core", "deps", "helper")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nested submodule directory remained after its removal delta: %v", err)
	}
	removedRetry, created, err := service.ImportProjectDeltaBundleWithArtifacts(ctx, owner, source, "task_submodule_remove", bytes.NewReader(removalBundle), int64(len(removalBundle)), hex.EncodeToString(removalBundleDigest[:]), baseParent, removalDelta.Commit, nil, 0, "", bytes.NewReader(removalArchive), int64(len(removalArchive)), hex.EncodeToString(removalArchiveDigest[:]))
	if err != nil || created || removedRetry.Workdir != removedProject.Workdir {
		t.Fatalf("nested gitlink removal retry was not idempotent: project=%+v created=%t err=%v", removedRetry, created, err)
	}
}
