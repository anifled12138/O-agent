//go:build linux

package projectquota

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const maxQuotaHelperRequestBytes = 4096

type quotaHelperRequest struct {
	Action    string `json:"action"`
	Path      string `json:"path"`
	ProjectID uint32 `json:"projectId"`
	Limit     int64  `json:"limitBytes,omitempty"`
}

type quotaHelperResponse struct {
	Quota *WorkspaceQuota `json:"quota,omitempty"`
	Mount *ProbeResult    `json:"mount,omitempty"`
	Error string          `json:"error,omitempty"`
}

// ServeQuotaHelper exposes only apply/inspect/release for canonical task
// directory names under one configured workspace root. The caller is
// authenticated with SO_PEERCRED; this is intended to run as root in a
// separate system service, while the Agent service connects as oagent.
func ServeQuotaHelper(ctx context.Context, socketPath, workspaceRoot string, allowedUID, allowedGID uint32, maximumLimitBytes int64) (retErr error) {
	if ctx == nil || allowedUID == 0 || allowedGID == 0 || maximumLimitBytes <= 0 {
		return errors.New("quota helper requires a context, non-root client UID/GID, and positive maximum limit")
	}
	if os.Geteuid() != 0 {
		return errors.New("quota helper must run as root")
	}
	socketPath, err := filepath.Abs(filepath.Clean(socketPath))
	if err != nil || socketPath == string(filepath.Separator) {
		return errors.Join(errors.New("quota helper socket path must be an absolute non-root path"), err)
	}
	root, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil || strings.TrimSpace(workspaceRoot) == "" {
		return errors.Join(errors.New("quota helper workspace root must be configured"), err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve configured quota workspace root: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return errors.Join(errors.New("quota helper workspace root is not a directory"), err)
	}
	parentInfo, err := os.Lstat(filepath.Dir(socketPath))
	if err != nil {
		return fmt.Errorf("inspect quota helper socket directory: %w", err)
	}
	parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || parentStat.Uid != 0 || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("quota helper socket directory must be a non-writable, root-owned real directory")
	}
	if err := removeStaleRootSocket(socketPath); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on quota helper socket: %w", err)
	}
	defer func() {
		closeErr := listener.Close()
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
		retErr = errors.Join(retErr, closeErr, removeRootSocket(socketPath))
	}()
	if err := os.Chmod(socketPath, 0o660); err != nil {
		return fmt.Errorf("set quota helper socket mode: %w", err)
	}
	if err := os.Chown(socketPath, 0, int(allowedGID)); err != nil {
		return fmt.Errorf("set quota helper socket group: %w", err)
	}
	if err := verifyRootSocket(socketPath, allowedGID); err != nil {
		return err
	}
	var active sync.WaitGroup
	connections := make(chan struct{}, 16)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				active.Wait()
				return nil
			}
			return fmt.Errorf("accept quota helper connection: %w", acceptErr)
		}
		select {
		case connections <- struct{}{}:
			active.Add(1)
			go func() {
				defer active.Done()
				defer func() { <-connections }()
				if err := serveQuotaHelperConnection(connection, root, allowedUID, maximumLimitBytes); err != nil {
					slog.Warn("quota helper request failed", "error", err)
				}
			}()
		default:
			if err := writeQuotaHelperResponse(connection, quotaHelperResponse{Error: "quota helper is at its connection limit"}); err != nil {
				slog.Warn("quota helper could not report connection limit", "error", err)
			}
			if err := connection.Close(); err != nil {
				slog.Warn("quota helper rejected connection close failed", "error", err)
			}
		}
	}
}

func serveQuotaHelperConnection(connection *net.UnixConn, root string, allowedUID uint32, maximumLimitBytes int64) error {
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	uid, err := unixPeerUID(connection)
	if err != nil {
		return err
	}
	if uid != allowedUID {
		return writeQuotaHelperResponse(connection, quotaHelperResponse{Error: "caller UID is not authorized"})
	}
	line, err := bufio.NewReaderSize(connection, maxQuotaHelperRequestBytes).ReadSlice('\n')
	if err != nil {
		return writeQuotaHelperResponse(connection, quotaHelperResponse{Error: "request must be one newline-terminated JSON object"})
	}
	if len(line) > maxQuotaHelperRequestBytes {
		return writeQuotaHelperResponse(connection, quotaHelperResponse{Error: "request exceeds the configured size limit"})
	}
	var request quotaHelperRequest
	decoder := json.NewDecoder(bytes.NewReader(line[:len(line)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return writeQuotaHelperResponse(connection, quotaHelperResponse{Error: "request JSON is invalid"})
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return writeQuotaHelperResponse(connection, quotaHelperResponse{Error: "request must contain exactly one JSON object"})
	}
	var response quotaHelperResponse
	switch request.Action {
	case "health":
		probe, probeErr := ProbeFilesystem(root)
		if probeErr != nil {
			response.Error = probeErr.Error()
		} else if !probe.MountSupported || !probe.QuotaOptionFound {
			response.Mount = &probe
			response.Error = "task workspace filesystem project quota is unavailable: " + probe.Reason
		} else {
			response.Mount = &probe
		}
	case "apply":
		if !validHelperWorkspacePath(request.Path) {
			response.Error = "workspace path is outside the task quota namespace"
			break
		}
		if request.Limit <= 0 || request.Limit > maximumLimitBytes {
			response.Error = "workspace quota exceeds the root-configured maximum"
			break
		}
		quota, applyErr := ApplyEmptyWorkspace(root, request.Path, request.ProjectID, request.Limit, maximumLimitBytes)
		if applyErr != nil {
			response.Error = applyErr.Error()
		} else {
			response.Quota = &quota
		}
	case "inspect":
		if !validHelperWorkspacePath(request.Path) {
			response.Error = "workspace path is outside the task quota namespace"
			break
		}
		quota, inspectErr := InspectWorkspace(root, request.Path, request.ProjectID)
		if inspectErr != nil {
			response.Error = inspectErr.Error()
		} else {
			response.Quota = &quota
		}
	case "release":
		if !validHelperWorkspacePath(request.Path) {
			response.Error = "workspace path is outside the task quota namespace"
			break
		}
		quota, releaseErr := ReleaseTaskWorkspace(root, request.Path, request.ProjectID)
		if releaseErr != nil {
			response.Error = releaseErr.Error()
		} else {
			if quota.Applied || quota.LimitBytes != 0 {
				response.Error = "released quota did not read back as cleared"
			} else {
				response.Quota = &quota
			}
		}
	default:
		response.Error = "unsupported quota helper action"
	}
	return writeQuotaHelperResponse(connection, response)
}

func validHelperWorkspacePath(value string) bool {
	if validateQuotaRelativePath(value) != nil {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] != ".o-projects" {
		return false
	}
	name := parts[1]
	var prefix string
	switch {
	case strings.HasPrefix(name, "execution-"):
		prefix = "execution-"
	case strings.HasPrefix(name, "scratch-"):
		prefix = "scratch-"
	default:
		return false
	}
	digest := strings.TrimPrefix(name, prefix)
	if len(digest) != 24 {
		return false
	}
	for _, character := range digest {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func unixPeerUID(connection *net.UnixConn) (uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *unix.Ucred
	var socketErr error
	controlErr := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if controlErr != nil {
		return 0, controlErr
	}
	if socketErr != nil {
		return 0, socketErr
	}
	if credentials == nil {
		return 0, errors.New("read quota helper peer credentials returned no identity")
	}
	return credentials.Uid, nil
}

func writeQuotaHelperResponse(connection *net.UnixConn, response quotaHelperResponse) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFull(connection, encoded)
}

func removeStaleRootSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode()&os.ModeSocket == 0 {
		return errors.New("refusing to remove a non-root-owned or non-socket quota helper path")
	}
	return os.Remove(path)
}

func removeRootSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode()&os.ModeSocket == 0 {
		return errors.New("quota helper socket changed identity before cleanup")
	}
	return os.Remove(path)
}

func verifyRootSocket(path string, allowedGID uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != allowedGID || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o660 {
		return errors.New("quota helper socket owner, type, or permissions failed read-back")
	}
	return nil
}

func QuotaHelperConfig(args []string) (socketPath, workspaceRoot string, allowedUID, allowedGID uint32, maximumBytes int64, err error) {
	if len(args) != 5 {
		return "", "", 0, 0, 0, errors.New("usage: axiom-quota-helper <socket-path> <workspace-root> <allowed-uid> <allowed-gid> <maximum-limit-bytes>")
	}
	uidValue, parseErr := strconv.ParseUint(args[2], 10, 32)
	if parseErr != nil || uidValue == 0 {
		return "", "", 0, 0, 0, errors.New("allowed UID must be a nonzero unsigned integer")
	}
	gidValue, parseErr := strconv.ParseUint(args[3], 10, 32)
	if parseErr != nil || gidValue == 0 {
		return "", "", 0, 0, 0, errors.New("allowed GID must be a nonzero unsigned integer")
	}
	maximumBytes, parseErr = strconv.ParseInt(args[4], 10, 64)
	if parseErr != nil || maximumBytes <= 0 {
		return "", "", 0, 0, 0, errors.New("maximum quota limit must be a positive byte count")
	}
	return args[0], args[1], uint32(uidValue), uint32(gidValue), maximumBytes, nil
}
