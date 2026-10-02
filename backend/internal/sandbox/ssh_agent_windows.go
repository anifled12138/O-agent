//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	defaultWindowsSSHAgentPipe = `\\.\pipe\openssh-ssh-agent`
	maxNativeSSHAgentInstances = 1
)

var procGetNamedPipeClientPIDForSSH = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetNamedPipeClientProcessId")
var procCancelSynchronousPipeIO = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelIoEx")

type nativeSSHAgentBridge struct {
	stop       chan struct{}
	done       chan error
	mu         sync.Mutex
	stopOnce   sync.Once
	waitOnce   sync.Once
	waitErr    error
	serverPipe windows.Handle
	clientPipe windows.Handle
	upstream   map[*nativeSSHAgentConn]struct{}
}

type nativeSSHAgentConn struct {
	handle windows.Handle
	once   sync.Once
	err    error
}

func clearSSHAgentKeys(keys [][]byte) {
	for _, key := range keys {
		clear(key)
	}
}

func resolveHostSSHAgentPipe() (string, error) {
	endpoint := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if endpoint == "" {
		endpoint = defaultWindowsSSHAgentPipe
	}
	if !strings.HasPrefix(strings.ToLower(endpoint), `\\.\pipe\`) || strings.ContainsAny(endpoint, "\r\n\x00") {
		return "", errors.New("the Host SSH_AUTH_SOCK is not a local Windows named pipe")
	}
	return endpoint, nil
}

func probeHostSSHAgent() (string, [][]byte, error) {
	endpoint, err := resolveHostSSHAgentPipe()
	if err != nil {
		return "", nil, err
	}
	connection, err := openNativeSSHAgentPipe(endpoint, 2*time.Second)
	if err != nil {
		return "", nil, fmt.Errorf("connect to the current user's Windows OpenSSH agent: %w", err)
	}
	response, err := forwardSSHAgentRequest(connection, []byte{sshAgentRequestIdentities})
	err = errors.Join(err, connection.Close())
	if err != nil {
		return "", nil, fmt.Errorf("read identities from the current user's Windows OpenSSH agent: %w", err)
	}
	keys, err := sshAgentPublicKeys(response)
	clear(response)
	if err != nil {
		return "", nil, err
	}
	if len(keys) == 0 {
		return "", nil, errors.New("the current user's Windows OpenSSH agent has no loaded identities")
	}
	return endpoint, keys, nil
}

func openNativeSSHAgentPipe(endpoint string, timeout time.Duration) (*nativeSSHAgentConn, error) {
	name16, err := windows.UTF16PtrFromString(endpoint)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		handle, openErr := windows.CreateFile(name16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if openErr == nil {
			return &nativeSSHAgentConn{handle: handle}, nil
		}
		lastErr = openErr
		if !errors.Is(openErr, windows.ERROR_PIPE_BUSY) && !errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) {
			break
		}
		var waitErr error
		if errors.Is(openErr, windows.ERROR_PIPE_BUSY) {
			waitErr = waitForNativeNamedPipe(name16, 100)
		}
		if waitErr != nil {
			lastErr = waitErr
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = windows.ERROR_SEM_TIMEOUT
	}
	return nil, lastErr
}

func (c *nativeSSHAgentConn) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	var read uint32
	if err := windows.ReadFile(c.handle, buffer, &read, nil); err != nil {
		return int(read), err
	}
	if read == 0 {
		return 0, io.EOF
	}
	return int(read), nil
}

func (c *nativeSSHAgentConn) Write(buffer []byte) (int, error) {
	total := 0
	for len(buffer) > 0 {
		chunk := buffer
		if len(chunk) > 64<<10 {
			chunk = chunk[:64<<10]
		}
		var written uint32
		if err := windows.WriteFile(c.handle, chunk, &written, nil); err != nil {
			return total + int(written), err
		}
		if written == 0 {
			return total, io.ErrShortWrite
		}
		total += int(written)
		buffer = buffer[written:]
	}
	return total, nil
}

func (c *nativeSSHAgentConn) Close() error {
	c.once.Do(func() { c.err = windows.CloseHandle(c.handle) })
	return c.err
}

func startNativeSSHAgentBridge(pipeName, endpoint, commandID, ownerSID, accountSID string, logonSID *windows.SID, keys [][]byte) (*nativeSSHAgentBridge, error) {
	if logonSID == nil || !logonSID.IsValid() || endpoint == "" || commandID == "" || len(keys) == 0 {
		return nil, errors.New("SSH agent bridge authorization is incomplete")
	}
	allowed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if len(key) == 0 || len(key) > 64<<10 {
			return nil, errors.New("Host SSH agent returned an invalid identity key")
		}
		allowed[string(key)] = struct{}{}
	}
	name16, err := windows.UTF16PtrFromString(pipeName)
	if err != nil || !strings.HasPrefix(pipeName, `\\.\pipe\OAgentSSHAgent-`) || !strings.HasSuffix(pipeName, commandID) || strings.ContainsAny(commandID, "\\/:\r\n\x00") {
		return nil, errors.New("SSH agent command pipe name is invalid")
	}
	bridge := &nativeSSHAgentBridge{stop: make(chan struct{}), done: make(chan error, 1), upstream: make(map[*nativeSSHAgentConn]struct{})}
	handle, err := createNativeSSHAgentServerPipe(name16, ownerSID, logonSID.String(), true)
	if err != nil {
		return nil, fmt.Errorf("create command-scoped SSH agent pipe: %w", err)
	}
	bridge.serverPipe = handle
	go bridge.serve(name16, pipeName, endpoint, ownerSID, accountSID, logonSID.String(), allowed)
	return bridge, nil
}

func createNativeSSHAgentServerPipe(name *uint16, ownerSID, logonSID string, first bool) (windows.Handle, error) {
	sddl := fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)(A;;GA;;;%s)", ownerSID, logonSID)
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, err
	}
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	handle, err := windows.CreateNamedPipe(name, flags, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		maxNativeSSHAgentInstances, 64<<10, 64<<10, 0, &security)
	runtimeKeepAliveWFP(descriptor, security)
	return handle, err
}

func (b *nativeSSHAgentBridge) serve(name16 *uint16, pipeName, endpoint, ownerSID, accountSID, logonSID string, allowed map[string]struct{}) {
	handle := b.serverPipe
	var resultErr error
	var cleanupErr error
	for {
		b.mu.Lock()
		select {
		case <-b.stop:
			b.mu.Unlock()
			cleanupErr = errors.Join(cleanupErr, windows.CloseHandle(handle))
			b.done <- cleanupErr
			return
		default:
		}
		b.serverPipe = handle
		b.mu.Unlock()
		connectErr := windows.ConnectNamedPipe(handle, nil)
		if errors.Is(connectErr, windows.ERROR_PIPE_CONNECTED) {
			connectErr = nil
		}
		select {
		case <-b.stop:
			b.mu.Lock()
			if b.serverPipe == handle {
				b.serverPipe = 0
			}
			if b.clientPipe == handle {
				b.clientPipe = 0
			}
			b.mu.Unlock()
			cleanupErr = errors.Join(cleanupErr, windows.CloseHandle(handle))
			b.done <- cleanupErr
			return
		default:
		}
		if connectErr != nil {
			resultErr = errors.Join(fmt.Errorf("accept command-scoped SSH agent client: %w", connectErr), windows.CloseHandle(handle))
			b.done <- resultErr
			return
		}
		client := os.NewFile(uintptr(handle), "oagent-native-ssh-agent")
		b.mu.Lock()
		b.serverPipe = 0
		b.clientPipe = handle
		stopped := false
		select {
		case <-b.stop:
			stopped = true
		default:
		}
		b.mu.Unlock()
		if stopped {
			b.done <- errors.Join(cleanupErr, client.Close())
			return
		}
		if err := authenticateNativeSSHAgentClient(handle, accountSID, logonSID); err != nil {
			cleanupErr = errors.Join(cleanupErr, client.Close())
			resultErr = err
		} else {
			var upstream *nativeSSHAgentConn
			upstream, resultErr = openNativeSSHAgentPipe(endpoint, 2*time.Second)
			if resultErr == nil {
				b.mu.Lock()
				b.upstream[upstream] = struct{}{}
				b.mu.Unlock()
				resultErr = serveSSHAgentProxy(client, func(request []byte) ([]byte, error) {
					if err := nativeSSHBridgeStopped(b.stop); err != nil {
						return nil, err
					}
					return forwardSSHAgentRequest(upstream, request)
				}, allowed)
				b.mu.Lock()
				delete(b.upstream, upstream)
				b.mu.Unlock()
				if closeErr := upstream.Close(); closeErr != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close Host SSH agent connection: %w", closeErr))
				}
			} else {
				cleanupErr = errors.Join(cleanupErr, client.Close())
			}
		}
		b.mu.Lock()
		b.clientPipe = 0
		b.mu.Unlock()
		// A malformed or unauthorized client can only terminate its own pipe;
		// the listener remains available for the rest of this command lease.
		_ = resultErr
		if cleanupErr != nil {
			b.done <- cleanupErr
			return
		}
		select {
		case <-b.stop:
			b.done <- cleanupErr
			return
		default:
		}
		handle, connectErr = createNativeSSHAgentServerPipe(name16, ownerSID, logonSID, false)
		if connectErr != nil {
			resultErr = fmt.Errorf("recreate command-scoped SSH agent pipe %q: %w", pipeName, connectErr)
			b.done <- resultErr
			return
		}
	}
}

func nativeSSHBridgeStopped(stop <-chan struct{}) error {
	select {
	case <-stop:
		return errors.New("SSH agent command lease has ended")
	default:
		return nil
	}
}

func authenticateNativeSSHAgentClient(pipe windows.Handle, accountSID, logonSID string) (resultErr error) {
	var clientPID uint32
	r1, _, callErr := procGetNamedPipeClientPIDForSSH.Call(uintptr(pipe), uintptr(unsafe.Pointer(&clientPID)))
	if r1 == 0 {
		return fmt.Errorf("read SSH agent pipe client process ID: %w", callErr)
	}
	if clientPID == 0 {
		return errors.New("SSH agent pipe client process is unavailable")
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, clientPID)
	if err != nil {
		return fmt.Errorf("open SSH agent pipe client identity: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(process)) }()
	user, clientLogonSID, err := nativeProcessTokenIdentity(process)
	if err != nil {
		return err
	}
	if user != accountSID || clientLogonSID != logonSID {
		return errors.New("SSH agent pipe client is outside the authenticated command logon")
	}
	return nil
}

func (b *nativeSSHAgentBridge) stopAndWait() error {
	if b == nil {
		return nil
	}
	b.stopOnce.Do(func() {
		close(b.stop)
		b.mu.Lock()
		upstream := make([]*nativeSSHAgentConn, 0, len(b.upstream))
		for connection := range b.upstream {
			upstream = append(upstream, connection)
		}
		b.mu.Unlock()
		for _, connection := range upstream {
			b.waitErr = errors.Join(b.waitErr, connection.Close())
		}
	})
	b.waitOnce.Do(func() {
		deadline := time.Now().Add(3 * time.Second)
		reportedCancelErrors := make(map[windows.Handle]bool)
		for {
			b.mu.Lock()
			server, client := b.serverPipe, b.clientPipe
			var cancelErr error
			for _, handle := range []windows.Handle{server, client} {
				if handle == 0 {
					continue
				}
				err := cancelNativePipeIO(handle)
				if err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) && !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
					if !reportedCancelErrors[handle] {
						cancelErr = errors.Join(cancelErr, err)
						reportedCancelErrors[handle] = true
					}
				}
			}
			b.mu.Unlock()
			b.waitErr = errors.Join(b.waitErr, cancelErr)
			select {
			case err := <-b.done:
				b.waitErr = errors.Join(b.waitErr, err)
				return
			default:
			}
			if time.Until(deadline) <= 0 {
				b.waitErr = errors.Join(b.waitErr, errors.New("command-scoped SSH agent bridge did not stop"))
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return b.waitErr
}
