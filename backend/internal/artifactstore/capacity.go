package artifactstore

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Capacity reports durable artifact storage and physical filesystem headroom.
// It is observational only: artifact and project sizes are not capped here.
type Capacity struct {
	Backend                    string    `json:"backend"`
	RemoteObjectsAuthoritative bool      `json:"remoteObjectsAuthoritative"`
	ObjectBytes                int64     `json:"objectBytes"`
	StagingBytes               int64     `json:"stagingBytes"`
	TemporaryBytes             int64     `json:"temporaryBytes"`
	MetadataDatabaseBytes      int64     `json:"metadataDatabaseBytes"`
	MetadataWALBytes           int64     `json:"metadataWalBytes"`
	MetadataSHMBytes           int64     `json:"metadataShmBytes"`
	FilesystemTotalBytes       int64     `json:"filesystemTotalBytes,omitempty"`
	FilesystemFreeBytes        int64     `json:"filesystemFreeBytes,omitempty"`
	FilesystemMeasured         bool      `json:"filesystemMeasured"`
	ObservedAt                 time.Time `json:"observedAt"`
}

func (s *Service) Capacity(ctx context.Context) (Capacity, error) {
	if s == nil {
		return Capacity{}, fmt.Errorf("artifact storage is unavailable")
	}
	var result Capacity
	result.Backend = "local"
	if s.remote != nil {
		result.Backend = "s3"
		result.RemoteObjectsAuthoritative = true
	}
	var err error
	result.ObjectBytes, err = directoryBytes(ctx, filepath.Join(s.root, "objects", "sha256"))
	if err != nil {
		return Capacity{}, fmt.Errorf("measure content-addressed artifacts: %w", err)
	}
	result.StagingBytes, err = directoryBytes(ctx, filepath.Join(s.root, "staging"))
	if err != nil {
		return Capacity{}, fmt.Errorf("measure artifact staging: %w", err)
	}
	result.TemporaryBytes, err = directoryBytes(ctx, filepath.Join(s.root, "objects", ".tmp"))
	if err != nil {
		return Capacity{}, fmt.Errorf("measure temporary artifact objects: %w", err)
	}
	dataDir := filepath.Dir(s.root)
	result.MetadataDatabaseBytes, err = optionalFileBytes(filepath.Join(dataDir, "axiom.db"))
	if err != nil {
		return Capacity{}, fmt.Errorf("measure SQLite metadata database: %w", err)
	}
	result.MetadataWALBytes, err = optionalFileBytes(filepath.Join(dataDir, "axiom.db-wal"))
	if err != nil {
		return Capacity{}, fmt.Errorf("measure SQLite write-ahead log: %w", err)
	}
	result.MetadataSHMBytes, err = optionalFileBytes(filepath.Join(dataDir, "axiom.db-shm"))
	if err != nil {
		return Capacity{}, fmt.Errorf("measure SQLite shared-memory file: %w", err)
	}
	result.FilesystemTotalBytes, result.FilesystemFreeBytes, result.FilesystemMeasured, err = filesystemCapacity(s.root)
	if err != nil {
		return Capacity{}, fmt.Errorf("measure artifact filesystem capacity: %w", err)
	}
	result.ObservedAt = time.Now().UTC()
	return result, nil
}

func optionalFileBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return 0, fmt.Errorf("expected a regular file at %s", path)
	}
	return info.Size(), nil
}

func directoryBytes(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || total > int64(^uint64(0)>>1)-info.Size() {
			return fmt.Errorf("byte count overflow while measuring %s", path)
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
