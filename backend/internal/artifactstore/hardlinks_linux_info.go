//go:build linux

package artifactstore

import (
	"fmt"
	"os"
	"syscall"
)

func archiveFileLinkIdentity(info os.FileInfo) (string, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return "", 0, false
	}
	return fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino)), uint64(stat.Nlink), true
}
