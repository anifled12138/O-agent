//go:build linux

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const linuxCredentialGuestDir = "/run/o-agent-git"
const linuxCredentialSocketName = "credential.sock"

type linuxGitCredentialBridge struct {
	dir      string
	socket   string
	server   *http.Server
	listener net.Listener
	done     chan error
}

func startLinuxGitCredentialBridge(broker GitCredentialBroker, repositoryURLs []string) (*linuxGitCredentialBridge, error) {
	if broker == nil || len(repositoryURLs) == 0 {
		return nil, nil
	}
	allowed := make(map[string]bool, len(repositoryURLs))
	for _, raw := range repositoryURLs {
		canonical, err := canonicalHTTPSRepositoryURL(raw)
		if err != nil {
			return nil, fmt.Errorf("validate scoped Git credential URL: %w", err)
		}
		allowed[canonical] = true
	}
	dir, err := os.MkdirTemp("/tmp", "o-git-")
	if err != nil {
		return nil, fmt.Errorf("create private Git credential bridge: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	socket := filepath.Join(dir, linuxCredentialSocketName)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("listen on private Git credential bridge: %w", err), os.RemoveAll(dir))
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		return nil, errors.Join(err, listener.Close(), os.RemoveAll(dir))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /credential", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request struct {
			RepositoryURL string `json:"repositoryUrl"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid credential request", http.StatusBadRequest)
			return
		}
		canonical, err := canonicalHTTPSRepositoryURL(request.RepositoryURL)
		if err != nil || !allowed[canonical] {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		username, password, ok := broker.Lookup(canonical)
		if !ok || username == "" || password == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(map[string]string{"username": username, "password": password}); err != nil {
			return
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}
	bridge := &linuxGitCredentialBridge{dir: dir, socket: socket, listener: listener, server: server, done: make(chan error, 1)}
	go func() { bridge.done <- server.Serve(listener) }()
	return bridge, nil
}

func (b *linuxGitCredentialBridge) close() error {
	if b == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	shutdownErr := b.server.Shutdown(shutdownCtx)
	var forceCloseErr error
	if shutdownErr != nil {
		forceCloseErr = b.server.Close()
	}
	serveErr := <-b.done
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	removeErr := os.RemoveAll(b.dir)
	if _, err := os.Stat(b.dir); !errors.Is(err, os.ErrNotExist) {
		removeErr = errors.Join(removeErr, fmt.Errorf("Git credential bridge directory remains after cleanup: %s", b.dir), err)
	}
	return errors.Join(shutdownErr, forceCloseErr, serveErr, removeErr)
}

func canonicalHTTPSRepositoryURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" || parsed.RawPath != "" {
		return "", errors.New("repository credential scope must be a plain HTTPS URL without credentials, port, query, or fragment")
	}
	cleanPath := strings.Trim(strings.TrimSpace(parsed.EscapedPath()), "/")
	if cleanPath == "" || strings.Contains(cleanPath, "\\") || strings.Contains(cleanPath, "%") {
		return "", errors.New("repository credential scope has an invalid path")
	}
	for _, component := range strings.Split(cleanPath, "/") {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("repository credential scope has an invalid path")
		}
	}
	return "https://" + strings.ToLower(parsed.Hostname()) + "/" + cleanPath, nil
}

// RunGitCredentialHelper is a subprocess entry point used by Git inside the
// sandbox. Credentials cross only a private Unix socket and are printed only
// to Git's credential-helper protocol output.
func RunGitCredentialHelper(args []string, stdin io.Reader, stdout io.Writer) error {
	var socket string
	var operation string
	for i := 0; i < len(args); i++ {
		if args[i] == "--socket" && i+1 < len(args) {
			socket = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(args[i], "-") || operation != "" {
			return errors.New("Git credential helper received unsupported arguments")
		}
		operation = args[i]
	}
	if socket == "" || !filepath.IsAbs(socket) || operation == "" {
		return errors.New("Git credential helper requires a socket and operation")
	}
	if operation == "store" || operation == "erase" {
		return nil
	}
	if operation != "get" {
		return errors.New("Git credential helper operation is unsupported")
	}
	values, err := parseGitCredentialInput(stdin)
	if err != nil {
		return err
	}
	if values["protocol"] != "https" || values["host"] == "" || values["path"] == "" {
		return nil
	}
	repositoryURL, err := canonicalHTTPSRepositoryURL("https://" + values["host"] + "/" + values["path"])
	if err != nil {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"repositoryUrl": repositoryURL})
	if err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	request, err := http.NewRequest(http.MethodPost, "http://o-agent/credential", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request scoped Git credentials: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("scoped Git credential broker returned HTTP %d", response.StatusCode)
	}
	var credential struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&credential); err != nil {
		return fmt.Errorf("decode scoped Git credential response: %w", err)
	}
	if credential.Username == "" || credential.Password == "" || strings.ContainsAny(credential.Username+credential.Password, "\r\n\x00") {
		return errors.New("scoped Git credential response is invalid")
	}
	if _, err := fmt.Fprintf(stdout, "username=%s\npassword=%s\n\n", credential.Username, credential.Password); err != nil {
		return err
	}
	return nil
}

func parseGitCredentialInput(reader io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(io.LimitReader(reader, 4096))
	scanner.Buffer(make([]byte, 256), 1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsAny(key+value, "\r\n\x00") {
			return nil, errors.New("Git credential input is malformed")
		}
		if key == "protocol" || key == "host" || key == "path" {
			if _, exists := values[key]; exists {
				return nil, errors.New("Git credential input contains duplicate fields")
			}
			values[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}
