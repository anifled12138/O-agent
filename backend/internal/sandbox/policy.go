// Package sandbox contains host-owned OS sandbox launchers for agent-controlled
// processes. Policies are constructed by the host and must never be widened by
// model-provided paths or command arguments.
package sandbox

import "errors"

var ErrUnavailable = errors.New("operating system process sandbox is unavailable")

type RecoveryReport struct {
	UnresolvedGrants int
	PermissionDenied int
	OtherFailures    int
}

// Policy describes the filesystem locations a child process may access.
// Network access is deliberately absent from this first policy version: child
// processes receive no AppContainer network capabilities.
type Policy struct {
	ReadOnlyPaths []string
	// PrivateTempWorkingDirectory makes the per-AppContainer TEMP directory
	// the child process working directory. It is used for temporary scripts.
	PrivateTempWorkingDirectory bool
	// JournalPath is an out-of-scope durable record used to restore temporary
	// Windows ACL changes after an interrupted host.
	JournalPath string
}
