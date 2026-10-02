//go:build linux

package sandbox

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func TestPrepareLinuxNetworkAllowlistPinsOnlyResolvedPublicAddresses(t *testing.T) {
	lookup := func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "api.example.test" {
			t.Fatalf("unexpected DNS lookup: network=%q host=%q", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.8"), netip.MustParseAddr("2606:4700:4700::1111")}, nil
	}
	hostsFile, addresses, cleanup, err := prepareLinuxNetworkAllowlistWithResolver(context.Background(), []string{"API.EXAMPLE.TEST."}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Errorf("clean private hosts file: %v", err)
		}
	}()
	content, err := os.ReadFile(hostsFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, "93.184.216.34 api.example.test") || !strings.Contains(text, "2606:4700:4700::1111 api.example.test") || strings.Contains(text, "10.0.0.8") {
		t.Fatalf("private hosts map does not pin only public resolved addresses: %q", text)
	}
	if len(addresses) != 2 || !addresses[0].Is4() || addresses[1].Is4() {
		t.Fatalf("resolved destination IP allow-list = %v, want sorted IPv4+IPv6 public addresses", addresses)
	}
	if info, err := os.Stat(hostsFile); err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("private hosts map permissions = %v, err=%v; want 0400", info, err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hostsFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private hosts file remains after cleanup: %v", err)
	}
}

func TestIPFilterProbeRequiresBashConnectionDenial(t *testing.T) {
	for _, stderr := range []string{
		"bash: connect: Permission denied\nbash: /dev/tcp/127.0.0.1/43210: Permission denied",
		"bash: connect: Operation not permitted",
	} {
		if !isBashIPFilterDenial(stderr) {
			t.Errorf("Bash kernel denial was not recognized: %q", stderr)
		}
	}
	for _, stderr := range []string{
		"Failed to connect to bus: Permission denied",
		"systemd-run: Permission denied",
		"bash: connect: Connection refused",
		"",
	} {
		if isBashIPFilterDenial(stderr) {
			t.Errorf("non-filter failure was mistaken for a kernel denial: %q", stderr)
		}
	}
}

func TestPrepareLinuxNetworkAllowlistRejectsResolutionFailureAndNoPublicAnswers(t *testing.T) {
	failed := func(context.Context, string, string) ([]netip.Addr, error) { return nil, errors.New("DNS unavailable") }
	if _, _, _, err := prepareLinuxNetworkAllowlistWithResolver(context.Background(), []string{"example.test"}, failed); err == nil {
		t.Fatal("network allow-list accepted a failed DNS lookup")
	}
	privateOnly := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	}
	if _, _, _, err := prepareLinuxNetworkAllowlistWithResolver(context.Background(), []string{"example.test"}, privateOnly); err == nil {
		t.Fatal("network allow-list accepted a destination with only private addresses")
	}
}
