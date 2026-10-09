//go:build linux

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"
)

// Network-only transitional scope. It imposes no CPU, memory or process-count limits.
func systemdScopeArgs(bwrap string, bwrapArgs []string, timeout time.Duration, filterNetwork bool, allowedIPs []netip.Addr) (unit string, args []string, err error) {
	if bwrap == "" || timeout <= 0 {
		return "", nil, fmt.Errorf("sandbox executable and positive timeout are required")
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, err
	}
	unit = "o-agent-" + hex.EncodeToString(nonce[:]) + ".scope"
	args = []string{
		"--user", "--scope", "--quiet", "--collect", "--unit", unit, "--slice", "o-agent.slice",
	}
	if filterNetwork {
		args = append(args, "--property=IPAddressDeny=any")
		for _, address := range uniqueEgressAddresses(allowedIPs) {
			if !isPublicEgressAddress(address) {
				return "", nil, fmt.Errorf("network egress allow-list contains a non-public address: %s", address)
			}
			prefix := 128
			if address.Is4() {
				prefix = 32
			}
			args = append(args, "--property=IPAddressAllow="+netip.PrefixFrom(address, prefix).String())
		}
	}
	args = append(args, "--", bwrap)
	args = append(args, bwrapArgs...)
	return unit, args, nil
}
