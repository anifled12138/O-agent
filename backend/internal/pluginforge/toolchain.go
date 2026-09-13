package pluginforge

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// findGo resolves a project-managed toolchain before falling back to a machine
// installation. O_GO_BINARY is the explicit deployment override.
func (s *Service) findGo() (string, error) {
	candidates := []string{strings.TrimSpace(os.Getenv("O_GO_BINARY")), "go"}
	if runtime.GOOS == "windows" {
		candidates = []string{
			strings.TrimSpace(os.Getenv("O_GO_BINARY")),
			filepath.Join(s.workspaceRoot, "work", "toolchains", "go", "bin", "go.exe"),
			`D:\DevTools\go\bin\go.exe`,
			"go.exe",
		}
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Go toolchain is unavailable; set O_GO_BINARY or install the project-managed toolchain")
}
