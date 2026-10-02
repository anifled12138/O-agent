//go:build !linux && !windows

package artifactstore

func filesystemCapacity(string) (totalBytes, freeBytes int64, measured bool, err error) {
	return 0, 0, false, nil
}
