package sandbox

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const maxEgressHosts = 32

// These IANA special-purpose ranges are not ordinary public unicast targets.
// Keep translations, documentation, benchmarking, and transition mechanisms
// blocked so a whitelisted DNS name cannot route back to private IPv4 space.
var nonPublicEgressRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

func normalizeEgressHostname(raw string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/:@%\\\x00\r\n\t *?[]") {
		return "", fmt.Errorf("network destination %q is not an exact DNS hostname", raw)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "", fmt.Errorf("network destination %q must be a DNS hostname, not an IP literal", raw)
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("network destination %q has an invalid DNS label", raw)
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-') {
				return "", fmt.Errorf("network destination %q must use an ASCII DNS hostname (IDNA punycode is accepted)", raw)
			}
		}
	}
	return host, nil
}

func isPublicEgressAddress(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, blocked := range nonPublicEgressRanges {
		if blocked.Contains(address) {
			return false
		}
	}
	return true
}

func uniqueEgressAddresses(addresses []netip.Addr) []netip.Addr {
	seen := make(map[netip.Addr]struct{}, len(addresses))
	unique := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		unique = append(unique, address)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i].Less(unique[j]) })
	return unique
}
