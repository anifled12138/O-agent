//go:build linux

package artifactstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"axiom.local/agent/internal/domain"
)

// inspectWorkspaceHardlinks counts regular-file directory entries below the
// workspace (excluding Git metadata). A file with links outside this count
// could expose another host file when its contents are archived.
func inspectWorkspaceHardlinks(ctx context.Context, root string) (map[string]uint64, error) {
	counts := make(map[string]uint64)
	linksByIdentity := make(map[string]uint64)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, component := range strings.Split(filepath.ToSlash(relative), "/") {
			if component == ".git" {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		identity, links, supported := archiveFileLinkIdentity(info)
		if !supported || links <= 1 {
			return nil
		}
		if identity == "" || links == 0 {
			return fmt.Errorf("regular file has invalid hard-link metadata: %w", domain.ErrConflict)
		}
		counts[identity]++
		linksByIdentity[identity] = links
		return nil
	})
	if err != nil {
		return nil, err
	}
	for identity, count := range counts {
		if count != linksByIdentity[identity] {
			return nil, fmt.Errorf("workspace contains a regular file with hard links outside the workspace: %w", domain.ErrConflict)
		}
	}
	return linksByIdentity, nil
}
