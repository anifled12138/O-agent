package pluginruntime

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"axiom.local/agent/internal/pluginforge"
)

const (
	maxBrokerFileBytes = 1 << 20
	maxBrokerHTTPBytes = 2 << 20
)

type SecretResolver interface {
	ResolvePluginSecret(context.Context, string, string) ([]byte, error)
}
type BrokerCommand func(context.Context, json.RawMessage) (json.RawMessage, error)
type BrokerDirEntry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size,omitempty"`
}

// ResourceBroker is the only intended path from a contained sidecar to Host
// resources. Every operation checks the mounted release's approved manifest;
// command execution is a trusted Host registry, never an arbitrary executable.
type ResourceBroker struct {
	workspaceRoot string
	dataRoot      string
	secrets       SecretResolver
	commands      map[string]BrokerCommand
}

func NewResourceBroker(workspaceRoot, dataRoot string, secrets SecretResolver, commands map[string]BrokerCommand) (*ResourceBroker, error) {
	workspace, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, err
	}
	data, err := filepath.Abs(dataRoot)
	if err != nil {
		return nil, err
	}
	return &ResourceBroker{workspaceRoot: workspace, dataRoot: data, secrets: secrets, commands: commands}, nil
}

func (b *ResourceBroker) ReadFile(release pluginforge.Release, scope, relative string) ([]byte, error) {
	root, err := b.scopeRoot(release, scope, false)
	if err != nil {
		return nil, err
	}
	target, err := brokerPath(root, relative, true)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, maxBrokerFileBytes+1))
	if len(value) > maxBrokerFileBytes {
		return nil, errors.New("brokered file exceeds 1 MiB")
	}
	return value, err
}

func (b *ResourceBroker) ListDir(release pluginforge.Release, scope, relative string) ([]BrokerDirEntry, error) {
	root, err := b.scopeRoot(release, scope, false)
	if err != nil {
		return nil, err
	}
	target, err := brokerPath(root, relative, true)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	if len(items) > 2000 {
		return nil, errors.New("brokered directory exceeds 2000 entries")
	}
	result := make([]BrokerDirEntry, 0, len(items))
	for _, item := range items {
		if item.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, infoErr := item.Info()
		if infoErr != nil {
			continue
		}
		result = append(result, BrokerDirEntry{Name: item.Name(), Directory: item.IsDir(), Size: info.Size()})
	}
	return result, nil
}

func (b *ResourceBroker) WriteFile(release pluginforge.Release, scope, relative string, value []byte) error {
	if len(value) > maxBrokerFileBytes {
		return errors.New("brokered file exceeds 1 MiB")
	}
	root, err := b.scopeRoot(release, scope, true)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	target, err := brokerPathForWrite(root, relative)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, value, 0o600)
}

func (b *ResourceBroker) Secret(ctx context.Context, userID string, release pluginforge.Release, name string) ([]byte, error) {
	if !hasPermission(release.Manifest.Permissions.Secrets, name) {
		return nil, errors.New("secret is not granted to this release")
	}
	if b.secrets == nil {
		return nil, errors.New("plugin secret resolver is unavailable")
	}
	return b.secrets.ResolvePluginSecret(ctx, userID, name)
}

func (b *ResourceBroker) Run(ctx context.Context, release pluginforge.Release, name string, input json.RawMessage) (json.RawMessage, error) {
	if !release.Manifest.Permissions.Process {
		return nil, errors.New("process broker is not granted to this release")
	}
	handler := b.commands[name]
	if handler == nil {
		return nil, fmt.Errorf("host command %q is not registered", name)
	}
	return handler(ctx, input)
}

func (b *ResourceBroker) Fetch(ctx context.Context, release pluginforge.Release, method, rawURL string, body io.Reader) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return nil, errors.New("brokered network URL must be credential-free HTTPS")
	}
	if !hasPermission(release.Manifest.Permissions.Network, strings.ToLower(u.Hostname())) {
		return nil, errors.New("network host is not granted to this release")
	}
	if method != http.MethodGet && method != http.MethodPost {
		return nil, errors.New("brokered network method is not allowed")
	}
	hostname := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if port != "443" {
		return nil, errors.New("brokered HTTPS only allows port 443")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostname}}
	transport.DialContext = func(dialCtx context.Context, network, _ string) (net.Conn, error) {
		addresses, resolveErr := net.DefaultResolver.LookupNetIP(dialCtx, "ip", hostname)
		if resolveErr != nil {
			return nil, resolveErr
		}
		for _, address := range addresses {
			if !publicBrokerAddress(address) {
				return nil, fmt.Errorf("network host resolved to blocked address %s", address)
			}
		}
		if len(addresses) == 0 {
			return nil, errors.New("network host did not resolve")
		}
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(dialCtx, network, net.JoinHostPort(addresses[0].String(), port))
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("brokered redirects are disabled") }}
	request, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	response.Body = &limitedReadCloser{Reader: io.LimitReader(response.Body, maxBrokerHTTPBytes+1), Closer: response.Body}
	return response, nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

func (b *ResourceBroker) scopeRoot(release pluginforge.Release, scope string, write bool) (string, error) {
	permissions := release.Manifest.Permissions.Filesystem.Read
	if write {
		permissions = release.Manifest.Permissions.Filesystem.Write
	}
	if !hasPermission(permissions, scope) {
		return "", fmt.Errorf("filesystem scope %q is not granted", scope)
	}
	switch scope {
	case "${workspace}":
		return b.workspaceRoot, nil
	case "${pluginData}":
		return filepath.Join(b.dataRoot, "plugin-data", release.PluginID), nil
	case "${temp}":
		return filepath.Join(b.dataRoot, "plugin-temp", release.ID), nil
	default:
		return "", errors.New("unknown filesystem scope")
	}
}

func brokerPath(root, relative string, mustExist bool) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relative)))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errors.New("brokered path escapes its scope")
	}
	rootEval, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(rootEval, clean)
	if mustExist {
		target, err = filepath.EvalSymlinks(target)
		if err != nil {
			return "", err
		}
	}
	absolute, err := filepath.Abs(target)
	if err != nil || !within(rootEval, absolute) {
		return "", errors.New("brokered path escapes its scope")
	}
	return absolute, nil
}

func brokerPathForWrite(root, relative string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relative)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", errors.New("brokered path escapes its scope")
	}
	rootAbsolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(filepath.Join(rootAbsolute, clean))
	for {
		if _, statErr := os.Stat(parent); statErr == nil {
			break
		}
		next := filepath.Dir(parent)
		if next == parent || !within(rootAbsolute, next) {
			return "", errors.New("brokered path has no safe parent")
		}
		parent = next
	}
	parentEval, err := filepath.EvalSymlinks(parent)
	if err != nil || !within(rootAbsolute, parentEval) {
		return "", errors.New("brokered path parent escapes its scope")
	}
	target := filepath.Join(rootAbsolute, clean)
	if !within(rootAbsolute, target) {
		return "", errors.New("brokered path escapes its scope")
	}
	return target, nil
}

func publicBrokerAddress(address netip.Addr) bool {
	if !address.IsValid() || address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	if prefix := netip.MustParsePrefix("100.64.0.0/10"); prefix.Contains(address) {
		return false
	}
	return true
}
