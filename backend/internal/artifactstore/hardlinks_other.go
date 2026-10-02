//go:build !linux

package artifactstore

import (
	"context"
	"os"
)

func inspectWorkspaceHardlinks(context.Context, string) (map[string]uint64, error) {
	return nil, nil
}

func archiveFileLinkIdentity(os.FileInfo) (string, uint64, bool) {
	return "", 0, false
}
