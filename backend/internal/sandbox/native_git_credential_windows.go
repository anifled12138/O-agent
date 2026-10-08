//go:build windows

package sandbox

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepareNativeGitCredentialHelper(environment map[string]string, scratch, runnerPath, pipeName, commandID string) error {
	if strings.ContainsAny(runnerPath+pipeName+commandID, "\r\n\x00\"") || !validNativeCommandID(commandID) || pipeName != `\\.\pipe\OAgentCredential-`+commandID {
		return errors.New("credential helper command channel or path is invalid")
	}
	helperDir := filepath.Join(scratch, "git-helper")
	if err := os.Mkdir(helperDir, 0o700); err != nil {
		return err
	}
	helperPath := filepath.Join(helperDir, "git-credential-oagent-"+commandID+".exe")
	if err := copyNativeCredentialHelperExecutable(runnerPath, helperPath); err != nil {
		return fmt.Errorf("copy trusted runner as command-scoped Git credential executable: %w", err)
	}
	oldPath := environment["PATH"]
	if oldPath == "" {
		return errors.New("sandbox PATH is empty")
	}
	environment["PATH"] = helperDir + string(os.PathListSeparator) + oldPath
	count, err := strconv.Atoi(environment["GIT_CONFIG_COUNT"])
	if err != nil || count < 0 || count > 256 {
		return errors.New("sandbox Git config environment is invalid")
	}
	for i, item := range [][2]string{{"credential.helper", ""}, {"credential.helper", "oagent-" + commandID}, {"credential.useHttpPath", "true"}} {
		environment[fmt.Sprintf("GIT_CONFIG_KEY_%d", count+i)] = item[0]
		environment[fmt.Sprintf("GIT_CONFIG_VALUE_%d", count+i)] = item[1]
	}
	environment["GIT_CONFIG_COUNT"] = strconv.Itoa(count + 3)
	return nil
}

func validNativeCommandID(commandID string) bool {
	if len(commandID) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(commandID)
	if err != nil || len(decoded) != 16 {
		return false
	}
	return commandID == strings.ToLower(commandID)
}

func copyNativeCredentialHelperExecutable(sourcePath, destinationPath string) (resultErr error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open installed command runner: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("inspect installed command runner: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 128<<20 {
		return fmt.Errorf("installed command runner size %d is outside the credential-helper copy limit", info.Size())
	}
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fmt.Errorf("create command-scoped credential executable: %w", err)
	}
	copyBytes, copyErr := io.Copy(destination, source)
	syncErr := destination.Sync()
	closeErr := destination.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return errors.Join(copyErr, syncErr, closeErr)
	}
	if copyBytes != info.Size() {
		return fmt.Errorf("copied command runner length changed from %d to %d bytes", info.Size(), copyBytes)
	}
	copied, err := os.Stat(destinationPath)
	if err != nil {
		return fmt.Errorf("read back command-scoped credential executable: %w", err)
	}
	if !copied.Mode().IsRegular() || copied.Size() != info.Size() {
		return errors.New("command-scoped credential executable read-back does not match the runner")
	}
	sourceHash, err := sha256File(sourcePath)
	if err != nil {
		return fmt.Errorf("hash installed command runner: %w", err)
	}
	destinationHash, err := sha256File(destinationPath)
	if err != nil {
		return fmt.Errorf("hash command-scoped credential executable: %w", err)
	}
	if sourceHash != destinationHash {
		return errors.New("command-scoped credential executable hash differs from the installed runner")
	}
	return nil
}

func nativeGitCredentialHelperID(executable string) (string, bool) {
	if !strings.EqualFold(filepath.Ext(executable), ".exe") {
		return "", false
	}
	name := strings.TrimSuffix(filepath.Base(executable), filepath.Ext(executable))
	const prefix = "git-credential-oagent-"
	if !strings.HasPrefix(strings.ToLower(name), prefix) {
		return "", false
	}
	commandID := strings.ToLower(name[len(prefix):])
	decoded, err := hex.DecodeString(commandID)
	if err != nil || len(decoded) != 16 {
		clear(decoded)
		return "", false
	}
	clear(decoded)
	return commandID, true
}

func nativeGitCredentialHelperArgs(executable string, args []string) ([]string, bool) {
	commandID, ok := nativeGitCredentialHelperID(executable)
	if !ok {
		return nil, false
	}
	helperArgs := []string{"--pipe", `\\.\pipe\OAgentCredential-` + commandID, "--command-id", commandID}
	return append(helperArgs, args...), true
}

func prepareNativeSSHBlocker(environment map[string]string, scratch string) error {
	blockerDir := filepath.Join(scratch, "ssh")
	if err := os.Mkdir(blockerDir, 0o700); err != nil {
		return err
	}
	blockerPath := filepath.Join(blockerDir, "git-ssh-unavailable.cmd")
	body := "@echo off\r\n>&2 echo O Agent sandbox: SSH authentication and SSH commit signing need the current Windows user's OpenSSH agent running with a loaded identity; use HTTPS credentials when SSH is unavailable.\r\nexit /b 126\r\n"
	if err := os.WriteFile(blockerPath, []byte(body), 0o600); err != nil {
		return err
	}
	environment["GIT_SSH_COMMAND"] = `"` + blockerPath + `"`
	environment["GIT_SSH_VARIANT"] = "ssh"
	return nil
}

func prepareNativeSSHAgentEnvironment(environment map[string]string, scratch, agentPipe string, keys [][]byte, signingFormat, signingIdentity, commitSigning, tagSigning string) error {
	if agentPipe == "" || !strings.HasPrefix(agentPipe, `\\.\pipe\OAgentSSHAgent-`) || len(keys) == 0 {
		return errors.New("command-scoped Host SSH agent configuration is incomplete")
	}
	sshDir := filepath.Join(environment["USERPROFILE"], ".ssh")
	if !pathWithin(scratch, sshDir) {
		return errors.New("generated SSH configuration escapes command scratch")
	}
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("create private command SSH configuration: %w", err)
	}
	knownHostsPath := filepath.Join(sshDir, "known_hosts")
	hostHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve Host profile for SSH host-key verification: %w", err)
	}
	hostKnownHosts := filepath.Join(hostHome, ".ssh", "known_hosts")
	if info, statErr := os.Stat(hostKnownHosts); statErr == nil {
		if !info.Mode().IsRegular() || info.Size() > 4<<20 {
			return errors.New("Host known_hosts must be a regular file no larger than 4 MiB")
		}
		contents, readErr := os.ReadFile(hostKnownHosts)
		if readErr != nil {
			return fmt.Errorf("read Host SSH known_hosts: %w", readErr)
		}
		defer clear(contents)
		if err := os.WriteFile(knownHostsPath, contents, 0o600); err != nil {
			return fmt.Errorf("copy public Host SSH host keys into command scratch: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect Host SSH known_hosts: %w", statErr)
	}
	var configuration strings.Builder
	configuration.WriteString("Host *\r\n")
	configuration.WriteString("  BatchMode yes\r\n")
	configuration.WriteString("  ForwardAgent no\r\n")
	configuration.WriteString("  IdentitiesOnly yes\r\n")
	configuration.WriteString("  StrictHostKeyChecking yes\r\n")
	configuration.WriteString("  UserKnownHostsFile \"" + strings.ReplaceAll(knownHostsPath, "\"", "") + "\"\r\n")
	configuration.WriteString("  IdentityAgent //./pipe/" + strings.TrimPrefix(agentPipe, `\\.\pipe\`) + "\r\n")
	for index, key := range keys {
		line, err := sshAgentPublicKeyLine(key)
		if err != nil {
			return fmt.Errorf("format loaded Host SSH identity %d: %w", index+1, err)
		}
		keyPath := filepath.Join(sshDir, fmt.Sprintf("identity-%02d.pub", index+1))
		if err := os.WriteFile(keyPath, []byte(line+"\r\n"), 0o600); err != nil {
			return fmt.Errorf("write public SSH identity into command scratch: %w", err)
		}
		configuration.WriteString("  IdentityFile \"" + strings.ReplaceAll(keyPath, "\"", "") + "\"\r\n")
	}
	configPath := filepath.Join(sshDir, "config")
	if err := os.WriteFile(configPath, []byte(configuration.String()), 0o600); err != nil {
		return fmt.Errorf("write command-scoped SSH configuration: %w", err)
	}
	if err := appendNativeGitConfig(environment, [][2]string{{"core.sshCommand", "ssh.exe"}}); err != nil {
		return err
	}
	environment["SSH_AUTH_SOCK"] = agentPipe
	environment["GIT_SSH_VARIANT"] = "ssh"
	delete(environment, "GIT_SSH_COMMAND")
	if strings.EqualFold(strings.TrimSpace(signingFormat), "ssh") && signingIdentity != "" {
		if err := prepareNativeSSHSigningEnvironment(environment, scratch, agentPipe, signingIdentity, commitSigning, tagSigning); err != nil {
			return err
		}
	}
	return nil
}

// prepareNativeSSHSigningEnvironment carries only the selected public identity
// and signing policy into Git. agentPipe can name an absent command lease when
// the Host agent is unavailable; this preserves the user's signing requirement
// so a signing operation fails instead of silently creating an unsigned commit.
func prepareNativeSSHSigningEnvironment(environment map[string]string, scratch, agentPipe, signingIdentity, commitSigning, tagSigning string) error {
	if agentPipe == "" || !strings.HasPrefix(agentPipe, `\\.\pipe\OAgentSSHAgent-`) || strings.ContainsAny(agentPipe, "\r\n\x00\"") {
		return errors.New("command-scoped SSH signing agent path is invalid")
	}
	sshDir := filepath.Join(environment["USERPROFILE"], ".ssh")
	if !pathWithin(scratch, sshDir) {
		return errors.New("generated SSH signing configuration escapes command scratch")
	}
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("create private command SSH signing configuration: %w", err)
	}
	signingPath := filepath.Join(sshDir, "signing.pub")
	publicKey, blob, keyErr := readHostSelectedSSHPublicKey(signingIdentity)
	if keyErr == nil {
		defer clear(blob)
		if err := os.WriteFile(signingPath, []byte(publicKey+"\r\n"), 0o600); err != nil {
			return fmt.Errorf("write selected SSH signing public key into command scratch: %w", err)
		}
	}
	// Keep a command-scoped signing-key override even when the selected public
	// key is unavailable. The missing scratch path makes signing fail at the
	// signing operation and prevents a repository-local key from being selected
	// implicitly, while ordinary local Git commands remain usable.
	entries := [][2]string{{"gpg.format", "ssh"}, {"user.signingkey", signingPath}}
	if commitSigning != "" {
		entries = append(entries, [2]string{"commit.gpgsign", commitSigning})
	}
	if tagSigning != "" {
		entries = append(entries, [2]string{"tag.gpgsign", tagSigning})
	}
	if err := appendNativeGitConfig(environment, entries); err != nil {
		return err
	}
	environment["SSH_AUTH_SOCK"] = agentPipe
	return nil
}

func sshAgentPublicKeyLine(blob []byte) (string, error) {
	keyType, _, err := readSSHAgentString(blob, 0)
	if err != nil || len(keyType) == 0 {
		return "", errors.Join(errors.New("SSH public key blob is malformed"), err)
	}
	return string(keyType) + " " + base64.StdEncoding.EncodeToString(blob) + " O-Agent", nil
}

func readHostSelectedSSHPublicKey(value string) (string, []byte, error) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "key::"))
	if strings.Contains(value, " ") || strings.Contains(value, "\t") {
		return parseOpenSSHPublicKeyLine(value)
	}
	if strings.HasPrefix(value, "~\\") || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, err
		}
		value = filepath.Join(home, value[2:])
	}
	if strings.EqualFold(filepath.Ext(value), ".pub") {
		// Only public-key files are read. A private key path is never opened.
	} else {
		value += ".pub"
	}
	info, err := os.Stat(value)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<10 {
		return "", nil, errors.New("selected SSH signing public key file is invalid or too large")
	}
	contents, err := os.ReadFile(value)
	if err != nil {
		return "", nil, err
	}
	defer clear(contents)
	return parseOpenSSHPublicKeyLine(strings.TrimSpace(string(contents)))
}

func parseOpenSSHPublicKeyLine(line string) (string, []byte, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || strings.ContainsAny(line, "\r\x00") {
		return "", nil, errors.New("selected SSH signing public key line is invalid")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", nil, errors.New("selected SSH signing public key is not valid Base64")
	}
	keyType, _, err := readSSHAgentString(blob, 0)
	if err != nil || string(keyType) != fields[0] {
		clear(blob)
		return "", nil, errors.Join(errors.New("selected SSH signing key type does not match its key blob"), err)
	}
	return strings.TrimSpace(line), blob, nil
}

func appendNativeGitConfig(environment map[string]string, entries [][2]string) error {
	count, err := strconv.Atoi(environment["GIT_CONFIG_COUNT"])
	if err != nil || count < 0 || count > 256 || count+len(entries) > 256 {
		return errors.New("sandbox Git config environment is invalid")
	}
	for index, item := range entries {
		suffix := strconv.Itoa(count + index)
		environment["GIT_CONFIG_KEY_"+suffix] = item[0]
		environment["GIT_CONFIG_VALUE_"+suffix] = item[1]
	}
	environment["GIT_CONFIG_COUNT"] = strconv.Itoa(count + len(entries))
	return nil
}

func startNativeGitCredentialBroker(pipeName, commandID, ownerSID, accountSID string, logonSID *windows.SID, broker GitCredentialBroker, allowedURLs []string) (func() error, error) {
	if broker == nil || logonSID == nil || !logonSID.IsValid() {
		return nil, errors.New("credential broker identity is unavailable")
	}
	allowed := make(map[string]bool)
	for _, raw := range allowedURLs {
		host, repository, err := scopeFromURL(raw)
		if err != nil {
			continue
		}
		allowed[host+"/"+repository] = true
	}
	if len(allowed) == 0 {
		return nil, errors.New("command has no validated HTTPS repository URL")
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	var mu sync.Mutex
	var stopOnce sync.Once
	var stopErr error
	initialHandle, err := createNativeCredentialPipe(pipeName, ownerSID, logonSID.String(), true)
	if err != nil {
		return nil, fmt.Errorf("reserve command credential pipe: %w", err)
	}
	active := initialHandle
	go func() {
		handle := initialHandle
		for {
			if nativeCredentialBrokerStopped(stop) {
				mu.Lock()
				if active == handle {
					active = 0
				}
				mu.Unlock()
				done <- windows.CloseHandle(handle)
				return
			}
			if handle == 0 {
				var createErr error
				handle, createErr = createNativeCredentialPipe(pipeName, ownerSID, logonSID.String(), false)
				if createErr != nil {
					if nativeCredentialBrokerStopped(stop) {
						done <- nil
					} else {
						done <- fmt.Errorf("create credential pipe: %w", createErr)
					}
					return
				}
			}
			mu.Lock()
			if nativeCredentialBrokerStopped(stop) {
				if active == handle {
					active = 0
				}
				mu.Unlock()
				done <- windows.CloseHandle(handle)
				return
			}
			active = handle
			mu.Unlock()
			connectErr := windows.ConnectNamedPipe(handle, nil)
			if errors.Is(connectErr, windows.ERROR_PIPE_CONNECTED) {
				connectErr = nil
			}
			var closeErr error
			if connectErr == nil {
				file := os.NewFile(uintptr(handle), "oagent-native-git-credential")
				connectErr = serveNativeGitCredential(file, commandID, accountSID, logonSID.String(), broker, allowed)
				mu.Lock()
				if active == handle {
					active = 0
				}
				mu.Unlock()
				closeErr = file.Close()
			} else {
				mu.Lock()
				if active == handle {
					active = 0
				}
				mu.Unlock()
				closeErr = windows.CloseHandle(handle)
			}
			handle = 0
			if nativeCredentialBrokerStopped(stop) {
				done <- closeErr
				return
			}
			if connectErr != nil {
				done <- errors.Join(connectErr, closeErr)
				return
			}
			if closeErr != nil {
				done <- closeErr
				return
			}
		}
	}()
	return func() error {
		stopOnce.Do(func() {
			close(stop)
			deadline := time.Now().Add(2 * time.Second)
			var cancelErr error
			reportedCancelErrors := make(map[windows.Handle]bool)
			for {
				mu.Lock()
				handle := active
				if handle != 0 {
					err := cancelNativePipeIO(handle)
					if err != nil && !errors.Is(err, windows.ERROR_NOT_FOUND) && !reportedCancelErrors[handle] {
						cancelErr = errors.Join(cancelErr, err)
						reportedCancelErrors[handle] = true
					}
				}
				mu.Unlock()
				// Cancelling one I/O operation does not prevent the listener from
				// entering its next read. Keep cancelling until it has closed its
				// pipe and reported completion, including the connect/read race.
				select {
				case err := <-done:
					stopErr = errors.Join(cancelErr, err)
					return
				default:
				}
				if time.Until(deadline) <= 0 {
					stopErr = errors.Join(cancelErr, errors.New("Git credential broker did not stop"))
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
		return stopErr
	}, nil
}

func nativeCredentialBrokerStopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

func cancelNativePipeIO(handle windows.Handle) error {
	result, _, callErr := procCancelSynchronousPipeIO.Call(uintptr(handle), 0)
	if result != 0 {
		return nil
	}
	if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
		return windows.ERROR_NOT_FOUND
	}
	if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
		return errors.New("cancel named pipe I/O failed without a Windows error")
	}
	return fmt.Errorf("cancel named pipe I/O: %w", callErr)
}

func waitForNativeNamedPipe(name *uint16, timeoutMS uint32) error {
	if name == nil {
		return errors.New("WaitNamedPipeW requires a pipe name")
	}
	wait := windows.NewLazySystemDLL("kernel32.dll").NewProc("WaitNamedPipeW")
	result, _, callErr := wait.Call(uintptr(unsafe.Pointer(name)), uintptr(timeoutMS))
	if result != 0 || errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) || errors.Is(callErr, windows.ERROR_SEM_TIMEOUT) {
		return nil
	}
	if callErr == nil || errors.Is(callErr, syscall.Errno(0)) {
		return errors.New("WaitNamedPipeW failed without a Windows error")
	}
	return fmt.Errorf("WaitNamedPipeW: %w", callErr)
}

func createNativeCredentialPipe(name, ownerSID, logonSID string, first bool) (windows.Handle, error) {
	sddl := fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)(A;;GRGW;;;%s)", ownerSID, logonSID)
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, err
	}
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	handle, err := windows.CreateNamedPipe(name16, flags, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 64<<10, 64<<10, 0, &security)
	runtime.KeepAlive(descriptor)
	return handle, err
}

func serveNativeGitCredential(file *os.File, commandID, accountSID, logonSID string, broker GitCredentialBroker, allowed map[string]bool) (resultErr error) {
	var clientPID uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(file.Fd()), &clientPID); err != nil {
		return err
	}
	if clientPID == 0 {
		return errors.New("credential pipe client process is unavailable")
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, clientPID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(process)) }()
	user, clientLogonSID, err := nativeProcessTokenIdentity(process)
	if err != nil {
		return err
	}
	if user != accountSID || clientLogonSID != logonSID {
		return errors.New("credential pipe client is outside the authenticated command logon")
	}
	request, err := readNativeFrame(file)
	if err != nil {
		return err
	}
	if request.Version != nativeProtocolVersion || request.Type != "credential-request" || request.CommandID != commandID || request.URL == "" {
		return errors.New("Git credential broker request is invalid")
	}
	host, repository, err := scopeFromURL(request.URL)
	if err != nil || !allowed[host+"/"+repository] {
		return writeNativeFrame(file, nativeFrame{Version: nativeProtocolVersion, Type: "credential-response", CommandID: commandID, Error: "repository is outside this command's authorized Git scope"})
	}
	username, password, ok := broker.Lookup(request.URL)
	if !ok || username == "" || password == "" {
		return writeNativeFrame(file, nativeFrame{Version: nativeProtocolVersion, Type: "credential-response", CommandID: commandID, Error: "no matching HTTPS credential is configured"})
	}
	defer zeroString(&password)
	return writeNativeFrame(file, nativeFrame{Version: nativeProtocolVersion, Type: "credential-response", CommandID: commandID, Username: username, Password: password})
}

func scopeFromURL(raw string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.ToLower(parsed.Scheme) != "https" || parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || strings.Contains(parsed.EscapedPath(), "%") {
		return "", "", errors.New("invalid Git credential URL")
	}
	return normalizeCredentialScope(parsed.Hostname(), strings.Trim(parsed.Path, "/"))
}

func normalizeCredentialScope(host, repository string) (string, string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if host == "" || repository == "" || strings.ContainsAny(host+repository, "\r\n\x00") || strings.Contains(repository, "\\") {
		return "", "", errors.New("invalid Git credential scope")
	}
	parsed, err := url.Parse("https://" + host)
	if err != nil || parsed.Hostname() != host || parsed.User != nil || parsed.Port() != "" {
		return "", "", errors.New("invalid Git credential host")
	}
	if strings.HasSuffix(strings.ToLower(repository), ".git") {
		repository = repository[:len(repository)-len(".git")]
	}
	for _, part := range strings.Split(repository, "/") {
		if part == "" || part == "." || part == ".." {
			return "", "", errors.New("invalid Git repository path")
		}
	}
	return host, repository, nil
}
