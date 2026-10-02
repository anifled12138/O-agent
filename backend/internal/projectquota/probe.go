package projectquota

import (
	"errors"
	"path"
	"strings"
)

type Mount struct {
	Root         string
	MountPoint   string
	MountOptions []string
	Filesystem   string
	Source       string
	SuperOptions []string
}

type ProbeResult struct {
	Filesystem        string `json:"filesystem,omitempty"`
	MountPoint        string `json:"mountPoint,omitempty"`
	MountSupported    bool   `json:"mountSupported"`
	QuotaOptionFound  bool   `json:"quotaOptionFound"`
	HardLimitVerified bool   `json:"hardLimitVerified"`
	Reason            string `json:"reason,omitempty"`
}

type WorkspaceQuota struct {
	Filesystem string `json:"filesystem"`
	MountPoint string `json:"mountPoint"`
	ProjectID  uint32 `json:"projectId"`
	LimitBytes int64  `json:"limitBytes"`
	UsedBytes  int64  `json:"usedBytes"`
	Applied    bool   `json:"applied"`
}

func parseMountInfo(contents string) ([]Mount, error) {
	var mounts []Mount
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || len(fields) < separator+4 {
			return nil, errors.New("invalid /proc/self/mountinfo record")
		}
		mounts = append(mounts, Mount{
			Root:         unescapeMountField(fields[3]),
			MountPoint:   path.Clean(unescapeMountField(fields[4])),
			MountOptions: splitOptions(fields[5]),
			Filesystem:   fields[separator+1],
			Source:       unescapeMountField(fields[separator+2]),
			SuperOptions: splitOptions(fields[separator+3]),
		})
	}
	if len(mounts) == 0 {
		return nil, errors.New("mountinfo contains no mount records")
	}
	return mounts, nil
}

func unescapeMountField(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}

func splitOptions(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func mountContainsPath(mountPoint, target string) bool {
	mountPoint, target = path.Clean(mountPoint), path.Clean(target)
	if !path.IsAbs(mountPoint) || !path.IsAbs(target) {
		return false
	}
	if mountPoint == "/" {
		return true
	}
	return target == mountPoint || strings.HasPrefix(target, strings.TrimRight(mountPoint, "/")+"/")
}
