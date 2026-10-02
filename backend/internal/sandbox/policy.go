// Package sandbox contains host-owned OS sandbox launchers for agent-controlled
// processes. Policies are constructed by the host and must never be widened by
// model-provided paths or command arguments.
package sandbox

import (
	"errors"
	"net/netip"
	"runtime"
	"time"
)

var ErrUnavailable = errors.New("operating system process sandbox is unavailable")
var ErrNativeNotInstalled = errors.New("the Windows native sandbox is not installed or healthy")

// Backend is selected by trusted host configuration and never by model input.
type Backend string

const (
	BackendAppContainer  Backend = "appcontainer"
	BackendWindowsNative Backend = "windows-native"
	BackendBubblewrap    Backend = "bubblewrap"
)

func PlatformDefaultBackend() Backend {
	if runtime.GOOS == "linux" {
		return BackendBubblewrap
	}
	return BackendAppContainer
}

func BackendSupportedOnCurrentOS(backend Backend) bool {
	switch runtime.GOOS {
	case "windows":
		return backend == BackendAppContainer || backend == BackendWindowsNative
	case "linux":
		return backend == BackendBubblewrap
	default:
		return false
	}
}

type RecoveryReport struct {
	UnresolvedGrants int
	PermissionDenied int
	OtherFailures    int
}

type NativeHealth struct {
	Installation           string `json:"installation"`
	Health                 string `json:"health"`
	Backend                string `json:"backend"`
	Reason                 string `json:"reason,omitempty"`
	NetworkEgressFiltering bool   `json:"networkEgressFiltering,omitempty"`
	NetworkEgressReason    string `json:"networkEgressReason,omitempty"`
	OwnerSID               string `json:"ownerSid,omitempty"`
	OfflineUser            string `json:"offlineUser,omitempty"`
	OnlineUser             string `json:"onlineUser,omitempty"`
	Runner                 string `json:"runner,omitempty"`
	CheckedAt              string `json:"checkedAt"`
}

// Policy describes the filesystem and network capabilities a child process
// may access. It is constructed by the trusted host after permission checks.
type Policy struct {
	Backend           Backend
	InstallDir        string
	RunnerPath        string
	ReadOnlyPaths     []string
	WritePaths        []string
	NetworkAccess     bool
	NetworkAllowHosts []string
	NetworkAllowIPs   []netip.Addr
	NetworkHostsFile  string
	GitCredentials    GitCredentialBroker
	GitCredentialURLs []string
	ProtectedPaths    []string
	Timeout           time.Duration
	// PowerShellExitWrapper asks the Windows dispatcher to preserve terminating
	// script errors and the last native command's exit code.
	PowerShellExitWrapper bool
	// PrivateTempWorkingDirectory makes the per-AppContainer TEMP directory
	// the child process working directory. It is used for temporary scripts.
	PrivateTempWorkingDirectory bool
	// JournalPath is an out-of-scope durable record used to restore temporary
	// Windows ACL changes after an interrupted host.
	JournalPath string
}

// GitCredentialBroker resolves an HTTPS Git credential only for a validated
// repository URL and only inside a single native sandbox command lease.
type GitCredentialBroker interface {
	Lookup(repositoryURL string) (username, password string, ok bool)
}
