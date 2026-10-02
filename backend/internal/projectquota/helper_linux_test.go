//go:build linux

package projectquota

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelperAcceptsOnlyGeneratedTaskWorkspacePaths(t *testing.T) {
	valid := []string{
		".o-projects/execution-0123456789abcdef01234567",
		".o-projects/scratch-0123456789abcdef01234567",
	}
	for _, path := range valid {
		if !validHelperWorkspacePath(path) {
			t.Errorf("rejected generated workspace path %q", path)
		}
	}
	invalid := []string{
		".o-projects/execution-0123456789abcdef0123456",
		".o-projects/execution-0123456789ABCDEF01234567",
		".o-projects/execution-0123456789abcdef01234567/child",
		".o-projects/other-0123456789abcdef01234567",
		".o-projects/../workspace",
		"/var/lib/o-agent/workspaces/.o-projects/execution-0123456789abcdef01234567",
	}
	for _, path := range invalid {
		if validHelperWorkspacePath(path) {
			t.Errorf("accepted non-task or non-canonical workspace path %q", path)
		}
	}
}

func TestQuotaHelperConfigRequiresExactPositiveRuntimeBounds(t *testing.T) {
	valid := []string{"/run/o-agent/quota.sock", "/var/lib/o-agent/workspaces", "1001", "1001", "8589934592"}
	socketPath, workspaceRoot, uid, gid, maximum, err := QuotaHelperConfig(valid)
	if err != nil || socketPath != valid[0] || workspaceRoot != valid[1] || uid != 1001 || gid != 1001 || maximum != 8589934592 {
		t.Fatalf("valid helper config = %q %q %d %d %d, %v", socketPath, workspaceRoot, uid, gid, maximum, err)
	}
	for _, args := range [][]string{
		{},
		{"/run/o-agent/quota.sock", "/var/lib/o-agent/workspaces", "0", "1001", "8589934592"},
		{"/run/o-agent/quota.sock", "/var/lib/o-agent/workspaces", "1001", "0", "8589934592"},
		{"/run/o-agent/quota.sock", "/var/lib/o-agent/workspaces", "1001", "1001", "0"},
		{"/run/o-agent/quota.sock", "/var/lib/o-agent/workspaces", "1001", "1001", "8589934592", "extra"},
	} {
		if _, _, _, _, _, err := QuotaHelperConfig(args); err == nil {
			t.Errorf("accepted invalid quota helper arguments %q", args)
		}
	}
}

func TestQuotaHelperRejectsUnauthorizedPeerBeforeParsingOperation(t *testing.T) {
	request := `{"action":"health"}`
	response := quotaHelperExchange(t, request, uint32(os.Getuid()+1))
	if !strings.Contains(response.Error, "not authorized") || response.Mount != nil || response.Quota != nil {
		t.Fatalf("unauthorized peer was not rejected before operation handling: %+v", response)
	}
}

func TestQuotaHelperRejectsPathsBeforeAnyQuotaMutation(t *testing.T) {
	request := `{"action":"apply","path":"../../etc","projectId":1234,"limitBytes":4096}`
	response := quotaHelperExchange(t, request, uint32(os.Getuid()))
	if !strings.Contains(response.Error, "outside the task quota namespace") || response.Quota != nil {
		t.Fatalf("non-task path was not rejected by helper protocol: %+v", response)
	}
}

func quotaHelperExchange(t *testing.T, body string, allowedUID uint32) quotaHelperResponse {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "helper.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	workspaceRoot := t.TempDir()
	finished := make(chan error, 1)
	go func() { finished <- serveQuotaHelperConnection(server, workspaceRoot, allowedUID, 1<<20) }()
	if _, err := client.Write([]byte(body + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response quotaHelperResponse
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	return response
}

func TestQuotaHelperRejectsOversizedOrUnknownJSONWithoutApplying(t *testing.T) {
	for _, body := range []string{
		`{"action":"apply","path":".o-projects/scratch-0123456789abcdef01234567","projectId":1,"limitBytes":1,"unexpected":"x"}`,
		strings.Repeat("x", maxQuotaHelperRequestBytes+1),
	} {
		response := quotaHelperExchange(t, body, uint32(os.Getuid()))
		if response.Error == "" || response.Quota != nil {
			t.Fatalf("invalid helper request was not rejected: %+v", response)
		}
	}
}
