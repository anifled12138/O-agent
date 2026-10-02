//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var linuxEgressProbeMu sync.Mutex
var linuxEgressProbeVerified bool

func requireLinuxIPAddressFilter(ctx context.Context) error {
	linuxEgressProbeMu.Lock()
	defer linuxEgressProbeMu.Unlock()
	if linuxEgressProbeVerified {
		return nil
	}
	if err := probeLinuxIPAddressFilter(ctx); err != nil {
		return fmt.Errorf("systemd cgroup IP egress filtering is not verified; refusing network access: %w", err)
	}
	linuxEgressProbeVerified = true
	return nil
}

func probeLinuxIPAddressFilter(ctx context.Context) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open loopback egress-filter probe listener: %w", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	bash, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash is required to verify the IP egress filter: %w", err)
	}
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		return fmt.Errorf("systemd-run is required to verify the IP egress filter: %w", err)
	}
	_, args, err := systemdScopeArgs(bash, []string{bash, "-c", fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d", port)}, 3*time.Second, true, nil)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, systemdRun, args...)
	command.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := command.Run()
	if ctx.Err() != nil || probeCtx.Err() != nil {
		return errors.Join(errors.New("IP egress filter probe did not reach a terminal result"), ctx.Err(), probeCtx.Err())
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1 {
		if runErr == nil {
			return fmt.Errorf("a cgroup with IPAddressDeny=any allowed the loopback probe (stdout=%q, stderr=%q)", stdout.String(), stderr.String())
		}
		return fmt.Errorf("a cgroup with IPAddressDeny=any was not proven to reject a loopback connection (exit=%v, stdout=%q, stderr=%q): %w", exitCode(runErr), stdout.String(), stderr.String(), runErr)
	}
	if !isBashIPFilterDenial(stderr.String()) {
		return fmt.Errorf("systemd scope failed without proving a kernel egress denial (stdout=%q, stderr=%q): %w", stdout.String(), stderr.String(), runErr)
	}
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		return fmt.Errorf("set IP egress probe listener deadline: %w", err)
	}
	connection, acceptErr := listener.Accept()
	if acceptErr == nil {
		_ = connection.Close()
		return errors.New("a cgroup with IPAddressDeny=any established a loopback connection")
	}
	if timeout, ok := acceptErr.(net.Error); !ok || !timeout.Timeout() {
		return fmt.Errorf("verify no loopback connection reached the egress probe listener: %w", acceptErr)
	}
	return nil
}

func isBashIPFilterDenial(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "bash:") {
			continue
		}
		if strings.Contains(line, "Permission denied") || strings.Contains(line, "Operation not permitted") {
			return true
		}
	}
	return false
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func prepareLinuxNetworkAllowlist(ctx context.Context, hosts []string) (string, []netip.Addr, func() error, error) {
	return prepareLinuxNetworkAllowlistWithResolver(ctx, hosts, net.DefaultResolver.LookupNetIP)
}

func prepareLinuxNetworkAllowlistWithResolver(ctx context.Context, hosts []string, lookup func(context.Context, string, string) ([]netip.Addr, error)) (string, []netip.Addr, func() error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if lookup == nil || len(hosts) == 0 || len(hosts) > maxEgressHosts {
		return "", nil, nil, fmt.Errorf("network access requires between 1 and %d exact destination hosts", maxEgressHosts)
	}
	uniqueHosts := make(map[string]struct{}, len(hosts))
	for _, raw := range hosts {
		host, err := normalizeEgressHostname(raw)
		if err != nil {
			return "", nil, nil, err
		}
		uniqueHosts[host] = struct{}{}
	}
	orderedHosts := make([]string, 0, len(uniqueHosts))
	for host := range uniqueHosts {
		orderedHosts = append(orderedHosts, host)
	}
	sort.Strings(orderedHosts)

	var allowed []netip.Addr
	var hostLines []string
	for _, host := range orderedHosts {
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addresses, err := lookup(lookupCtx, "ip", host)
		cancel()
		if err != nil {
			return "", nil, nil, fmt.Errorf("resolve requested network host %q: %w", host, err)
		}
		var safe []netip.Addr
		for _, address := range addresses {
			address = address.Unmap()
			if !isPublicEgressAddress(address) {
				continue
			}
			safe = append(safe, address)
			allowed = append(allowed, address)
		}
		if len(safe) == 0 {
			return "", nil, nil, fmt.Errorf("network host %q has no public unicast addresses", host)
		}
		sort.Slice(safe, func(i, j int) bool { return safe[i].Less(safe[j]) })
		for _, address := range safe {
			hostLines = append(hostLines, address.String()+" "+host)
		}
	}

	allowed = uniqueEgressAddresses(allowed)
	hostLines = append([]string{"127.0.0.1 localhost", "::1 localhost ip6-localhost ip6-loopback"}, hostLines...)
	directory, err := os.MkdirTemp("", "o-agent-egress-hosts-")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create private egress hosts directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", nil, nil, errors.Join(fmt.Errorf("restrict private egress hosts directory: %w", err), removeEgressHosts(directory))
	}
	filePath := filepath.Join(directory, "hosts")
	if err := os.WriteFile(filePath, []byte(strings.Join(hostLines, "\n")+"\n"), 0o400); err != nil {
		return "", nil, nil, errors.Join(fmt.Errorf("write private egress hosts map: %w", err), removeEgressHosts(directory))
	}
	cleanup := func() error { return removeEgressHosts(directory) }
	return filePath, allowed, cleanup, nil
}

func removeEgressHosts(directory string) error {
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove private egress hosts directory: %w", err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = errors.New("private egress hosts directory remains on host")
		}
		return err
	}
	return nil
}
