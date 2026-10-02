//go:build windows

package sandbox

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestValidNativeCommandID(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{value: "0123456789abcdef0123456789abcdef", valid: true},
		{value: "0123456789ABCDEF0123456789ABCDEF"},
		{value: "0123456789abcdef"},
		{value: "0123456789abcdef0123456789abcdeg"},
	} {
		if got := validNativeCommandID(test.value); got != test.valid {
			t.Fatalf("validNativeCommandID(%q) = %t, want %t", test.value, got, test.valid)
		}
	}
}

func TestWaitForNativeNamedPipePropagatesUnexpectedErrors(t *testing.T) {
	commandID, err := makeCommandID()
	if err != nil {
		t.Fatal(err)
	}
	missingPipe, err := windows.UTF16PtrFromString(`\\.\pipe\OAgentMissing-` + commandID)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitForNativeNamedPipe(missingPipe, 1); err != nil {
		t.Fatalf("missing pipe should be a retryable wait result: %v", err)
	}
	if err := waitForNativeNamedPipe(nil, 1); err == nil {
		t.Fatal("invalid WaitNamedPipeW input error was swallowed")
	}
}

type nativeGitCredentialBrokerStub struct{}

func (nativeGitCredentialBrokerStub) Lookup(string) (string, string, bool) {
	return "test-user", "test-secret", true
}

func TestNativeGitCredentialBrokerCancelsBlockedClientAndStopsIdempotently(t *testing.T) {
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	groups, err := windows.GetCurrentProcessToken().GetTokenGroups()
	if err != nil {
		t.Fatal(err)
	}
	logonSID := tokenLogonSID(groups)
	if logonSID == nil || !logonSID.IsValid() {
		t.Fatal("current test process has no valid logon SID")
	}
	commandID, err := makeCommandID()
	if err != nil {
		t.Fatal(err)
	}
	pipeName := `\\.\pipe\OAgentCredential-` + commandID
	stop, err := startNativeGitCredentialBroker(pipeName, commandID, owner.User.Sid.String(), owner.User.Sid.String(), logonSID, nativeGitCredentialBrokerStub{}, []string{"https://example.test/org/repo.git"})
	if err != nil {
		t.Fatalf("start command-scoped Git credential broker: %v", err)
	}
	client, err := openNativeSSHAgentPipe(pipeName, 2*time.Second)
	if err != nil {
		t.Fatalf("connect idle client to credential broker: %v", err)
	}
	stopErr := stop()
	if err := client.Close(); err != nil {
		t.Errorf("close idle credential pipe client: %v", err)
	}
	if stopErr != nil {
		t.Fatalf("stop credential broker with a blocked client: %v", stopErr)
	}
	if err := stop(); err != nil {
		t.Fatalf("repeated credential broker stop was not idempotent: %v", err)
	}
}
