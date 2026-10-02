//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

var proxyEnvironmentKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"}

// commandProxyEnvironment merges explicitly configured proxy variables with
// the current user's static Windows proxy. Explicit environment values win.
// PAC-only settings are rejected because command-line clients do not evaluate
// Windows PAC scripts consistently.
func commandProxyEnvironment(input []string) (map[string]string, error) {
	explicit := make(map[string]string, len(proxyEnvironmentKeys))
	explicitProxy := false
	for _, entry := range input {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		key = strings.ToUpper(key)
		for _, allowed := range proxyEnvironmentKeys {
			if key == allowed && strings.TrimSpace(value) != "" {
				explicit[key] = value
				if key != "NO_PROXY" {
					explicitProxy = true
				}
			}
		}
	}

	result := map[string]string{}
	for key, value := range explicit {
		result[key] = value
	}
	if explicitProxy {
		completeProxyEnvironment(result)
		return result, nil
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return nil, fmt.Errorf("read current-user Windows proxy settings: %w", err)
	}
	defer key.Close()

	enabled, _, enabledErr := key.GetIntegerValue("ProxyEnable")
	proxyServer, _, serverErr := key.GetStringValue("ProxyServer")
	autoConfigURL, _, pacErr := key.GetStringValue("AutoConfigURL")
	proxyOverride, _, overrideErr := key.GetStringValue("ProxyOverride")
	if enabledErr != nil && !errors.Is(enabledErr, registry.ErrNotExist) {
		return nil, fmt.Errorf("read Windows ProxyEnable: %w", enabledErr)
	}
	if serverErr != nil && !errors.Is(serverErr, registry.ErrNotExist) {
		return nil, fmt.Errorf("read Windows ProxyServer: %w", serverErr)
	}
	if pacErr != nil && !errors.Is(pacErr, registry.ErrNotExist) {
		return nil, fmt.Errorf("read Windows AutoConfigURL: %w", pacErr)
	}
	if overrideErr != nil && !errors.Is(overrideErr, registry.ErrNotExist) {
		return nil, fmt.Errorf("read Windows ProxyOverride: %w", overrideErr)
	}
	if strings.TrimSpace(autoConfigURL) != "" {
		return nil, errors.New("Windows uses a PAC proxy script; configure HTTP_PROXY/HTTPS_PROXY/ALL_PROXY for sandbox commands because PAC is not supported")
	}
	if enabled == 0 || strings.TrimSpace(proxyServer) == "" {
		return result, nil
	}
	proxies, err := parseWindowsProxyServer(proxyServer)
	if err != nil {
		return nil, err
	}
	for key, value := range proxies {
		result[key] = value
	}
	completeProxyEnvironment(result)
	if strings.TrimSpace(proxyOverride) != "" {
		result["NO_PROXY"] = windowsProxyOverrideToNoProxy(proxyOverride)
	}
	return result, nil
}

func parseWindowsProxyServer(raw string) (map[string]string, error) {
	result := map[string]string{}
	if !strings.Contains(raw, "=") {
		proxy, err := normalizeProxyURL(raw)
		if err != nil {
			return nil, fmt.Errorf("parse Windows ProxyServer: %w", err)
		}
		result["HTTP_PROXY"], result["HTTPS_PROXY"] = proxy, proxy
		return result, nil
	}
	for _, mapping := range strings.Split(raw, ";") {
		name, address, ok := strings.Cut(strings.TrimSpace(mapping), "=")
		if !ok || strings.TrimSpace(address) == "" {
			continue
		}
		scheme := strings.ToLower(strings.TrimSpace(name))
		if scheme != "http" && scheme != "https" && scheme != "socks" {
			continue
		}
		proxyAddress := strings.TrimSpace(address)
		if scheme == "socks" && !strings.Contains(proxyAddress, "://") {
			proxyAddress = "socks5://" + proxyAddress
		}
		proxy, err := normalizeProxyURL(proxyAddress)
		if err != nil {
			return nil, fmt.Errorf("parse Windows %s proxy: %w", scheme, err)
		}
		switch scheme {
		case "http":
			result["HTTP_PROXY"] = proxy
		case "https":
			result["HTTPS_PROXY"] = proxy
		case "socks":
			result["ALL_PROXY"] = proxy
		}
	}
	if len(result) == 0 {
		return nil, errors.New("Windows ProxyServer contains no supported proxy entries")
	}
	return result, nil
}

func completeProxyEnvironment(environment map[string]string) {
	if environment["HTTP_PROXY"] == "" && environment["ALL_PROXY"] != "" {
		environment["HTTP_PROXY"] = environment["ALL_PROXY"]
	}
	if environment["HTTPS_PROXY"] == "" && environment["HTTP_PROXY"] != "" {
		environment["HTTPS_PROXY"] = environment["HTTP_PROXY"]
	}
}

func normalizeProxyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("proxy address is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Scheme == "" {
		return "", fmt.Errorf("invalid proxy address %q", raw)
	}
	return parsed.String(), nil
}

func windowsProxyOverrideToNoProxy(raw string) string {
	entries := make([]string, 0)
	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.EqualFold(entry, "<local>") {
			entries = append(entries, "localhost", "127.0.0.1", "::1")
			continue
		}
		if strings.HasPrefix(entry, "*.") {
			entry = strings.TrimPrefix(entry, "*")
		}
		entries = append(entries, entry)
	}
	return strings.Join(entries, ",")
}

func proxyNeedsLoopbackExemption(environment map[string]string) bool {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		parsed, err := url.Parse(environment[key])
		if err != nil || parsed.Hostname() == "" {
			continue
		}
		host := strings.Trim(parsed.Hostname(), "[]")
		if strings.EqualFold(host, "localhost") || strings.EqualFold(host, "localhost.") {
			return true
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return true
		}
	}
	return false
}

func setAppContainerLoopbackExemption(sid string, add bool) error {
	checkNetIsolation := filepath.Join(os.Getenv("SystemRoot"), "System32", "CheckNetIsolation.exe")
	operation := "-d"
	if add {
		operation = "-a"
	}
	command := exec.Command(checkNetIsolation, "LoopbackExempt", operation, "-p="+sid)
	output, err := command.CombinedOutput()
	if err != nil {
		verb := "remove"
		if add {
			verb = "grant"
		}
		return fmt.Errorf("%s temporary AppContainer loopback exemption: %w: %s", verb, err, strings.TrimSpace(string(output)))
	}
	return nil
}
