//go:build linux

package sandbox

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestNetworkScopeDoesNotImposeHardwareOrLifetimeLimits(t *testing.T) {
	unit, args, err := systemdScopeArgs("/usr/bin/bwrap", []string{"--unshare-all", "--", "/usr/bin/git", "status"}, 17*time.Second, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(unit, "o-agent-") || !strings.HasSuffix(unit, ".scope") {
		t.Fatalf("scope name is not isolated per task: %q", unit)
	}
	want := []string{
		"--user", "--scope", "--collect", "--unit", unit, "--slice", "o-agent.slice",
		"--", "/usr/bin/bwrap",
		"--unshare-all", "--", "/usr/bin/git", "status",
	}
	for _, value := range want {
		if !containsValue(args, value) {
			t.Fatalf("systemd scope args do not contain %q: %q", value, args)
		}
	}
	for _, argument := range args {
		for _, property := range []string{"CPUQuota=", "MemoryMax=", "MemorySwapMax=", "TasksMax=", "RuntimeMaxSec="} {
			if strings.Contains(argument, property) {
				t.Fatalf("network scope retains hard limit: %s", argument)
			}
		}
	}
	if containsValue(args, "--wait") {
		t.Fatalf("systemd-run --wait cannot be combined with --scope: %q", args)
	}
	if args[len(args)-len([]string{"--unshare-all", "--", "/usr/bin/git", "status"})] != "--unshare-all" {
		t.Fatalf("bwrap command was not preserved as the scoped command: %q", args)
	}
}

func TestSystemdScopeArgsRejectIncompleteLaunchPolicy(t *testing.T) {
	if _, _, err := systemdScopeArgs("", nil, time.Second, false, nil); err == nil {
		t.Fatal("resource scope accepted an empty sandbox executable")
	}
	if _, _, err := systemdScopeArgs("/usr/bin/bwrap", nil, 0, false, nil); err == nil {
		t.Fatal("resource scope accepted an unlimited lifetime")
	}
}

func TestSystemdScopeArgsDenyNetworkByDefaultAndPinAllowedAddresses(t *testing.T) {
	unit, args, err := systemdScopeArgs("/usr/bin/bwrap", []string{"--share-net", "--", "/usr/bin/curl", "https://example.test"}, 17*time.Second, true, []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("2606:4700:4700::1111")})
	if err != nil {
		t.Fatal(err)
	}
	if !containsValue(args, "--property=IPAddressDeny=any") || !containsValue(args, "--property=IPAddressAllow=93.184.216.34/32") || !containsValue(args, "--property=IPAddressAllow=2606:4700:4700::1111/128") {
		t.Fatalf("systemd scope did not deny by default and allow only pinned destination addresses: %q", args)
	}
	if unit == "" {
		t.Fatal("egress-filtered scope has no unit identity")
	}
	if _, _, err := systemdScopeArgs("/usr/bin/bwrap", nil, time.Second, true, []netip.Addr{netip.MustParseAddr("127.0.0.1")}); err == nil {
		t.Fatal("systemd scope accepted a loopback egress allow-list")
	}
}

func containsValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
