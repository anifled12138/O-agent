// Package projectpolicy contains repository identity validation and transfer
// strategy hints for local project import.
package projectpolicy

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DirectTransferBatchBytes is the decimal 500 MB point after which data should
// use incremental/chunked transfer or an artifact reference. It is not a cap on
// project size, local execution, clone, or total upload volume.
const DirectTransferBatchBytes int64 = 500_000_000

var (
	ErrUnsafeLink     = errors.New("project root is a symbolic link or reparse point")
	ErrInvalidRepoURL = errors.New("repository URL must be a public HTTPS GitHub or Gitee repository URL without credentials")
)

// Measure counts logical bytes under root, including .git, ignored files,
// dependencies, and generated outputs. A child symbolic link contributes its
// target-path bytes and is never followed, so external linked data is neither
// read nor accidentally counted as project content.
func Measure(root string) (int64, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return 0, fmt.Errorf("project workdir is empty")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return 0, fmt.Errorf("inspect project workdir: %w", err)
	}
	if !rootInfo.IsDir() {
		if rootInfo.Mode()&os.ModeSymlink != 0 {
			return 0, ErrUnsafeLink
		}
		return 0, fmt.Errorf("project workdir is not a directory")
	}
	var total int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			linkBytes := int64(len(target))
			if total > int64(^uint64(0)>>1)-linkBytes {
				return fmt.Errorf("project size overflow")
			}
			total += linkBytes
			return nil
		}
		// Windows junctions and other reparse points are not consistently
		// represented as ModeSymlink. Reject any non-regular, non-directory node.
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported project filesystem entry %q", path)
		}
		if info.Mode().IsRegular() {
			if info.Size() < 0 || total > int64(^uint64(0)>>1)-info.Size() {
				return fmt.Errorf("project size overflow")
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return total, fmt.Errorf("measure project: %w", err)
	}
	return total, nil
}

// NeedsChunkedTransfer selects incremental/resumable transfer for larger data.
// It never denies a project, task, clone, or total transfer volume.
func NeedsChunkedTransfer(size int64) bool {
	return size > DirectTransferBatchBytes
}

// ValidateRepositoryURL accepts only GitHub/Gitee HTTPS repository URLs. It
// deliberately rejects URL credentials, alternate transports and extra path
// segments before a URL is persisted or passed to git.
func ValidateRepositoryURL(raw string) (provider string, normalized string, err error) {
	u, parseErr := url.Parse(strings.TrimSpace(raw))
	if parseErr != nil || u == nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() != "" {
		return "", "", ErrInvalidRepoURL
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "github.com":
		provider = "github"
	case "gitee.com":
		provider = "gitee"
	default:
		return "", "", ErrInvalidRepoURL
	}
	escapedPath := strings.ToLower(u.EscapedPath())
	if u.Opaque != "" || strings.Contains(u.Path, "\\") || strings.Contains(escapedPath, "%2f") || strings.Contains(escapedPath, "%5c") {
		return "", "", ErrInvalidRepoURL
	}
	cleanPath := strings.Trim(u.Path, "/")
	parts := strings.Split(cleanPath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", ErrInvalidRepoURL
	}
	for _, part := range parts {
		if part == "." || part == ".." || strings.ContainsAny(part, " %:\x00") {
			return "", "", ErrInvalidRepoURL
		}
	}
	parts[1] = strings.TrimSuffix(parts[1], ".git")
	if parts[1] == "" {
		return "", "", ErrInvalidRepoURL
	}
	return provider, "https://" + host + "/" + parts[0] + "/" + parts[1] + ".git", nil
}
