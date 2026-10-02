package sandbox

import (
	"net/netip"
	"testing"
)

func TestNormalizeEgressHostnameRejectsAddressLiteralsAndWildcard(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "*.example.com", "https://example.com", "internal.example/path", "host name"} {
		if host, err := normalizeEgressHostname(raw); err == nil {
			t.Errorf("normalizeEgressHostname(%q) = %q, want rejection", raw, host)
		}
	}
	if got, err := normalizeEgressHostname("XN--BCHER-KVA.EXAMPLE."); err != nil || got != "xn--bcher-kva.example" {
		t.Fatalf("IDNA ASCII hostname = %q, err=%v", got, err)
	}
}

func TestIsPublicEgressAddressRejectsLocalPrivateAndSpecialRanges(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.1", "127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "255.255.255.255",
		"192.0.2.1", "192.88.99.1", "198.18.0.1", "64:ff9b::a00:1", "64:ff9b:1::1",
		"100::1", "100:0:0:1::1", "2001::1", "2001:2::1", "2001:db8::1", "2002::1",
		"3fff::1", "5f00::1", "fec0::1", "::1", "fc00::1", "fe80::1", "ff02::1",
	} {
		if isPublicEgressAddress(netip.MustParseAddr(raw)) {
			t.Errorf("non-public egress address %q was allowed", raw)
		}
	}
	for _, raw := range []string{"93.184.216.34", "2606:4700:4700::1111"} {
		if !isPublicEgressAddress(netip.MustParseAddr(raw)) {
			t.Errorf("public egress address %q was rejected", raw)
		}
	}
}

func TestUniqueEgressAddressesUnmapsDeduplicatesAndSorts(t *testing.T) {
	got := uniqueEgressAddresses([]netip.Addr{
		netip.MustParseAddr("2606:4700:4700::1111"),
		netip.MustParseAddr("::ffff:93.184.216.34"),
		netip.MustParseAddr("93.184.216.34"),
	})
	if len(got) != 2 || got[0].String() != "93.184.216.34" || got[1].String() != "2606:4700:4700::1111" {
		t.Fatalf("unique sorted destination addresses = %v", got)
	}
}
