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
	for _, scenario := range []struct {
		name         string
		connect      bool
		partialFrame bool
	}{
		{name: "waiting_for_client"},
		{name: "idle_client", connect: true},
		{name: "partial_request", connect: true, partialFrame: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			commandID, err := makeCommandID()
			if err != nil {
				t.Fatal(err)
			}
			pipeName := `\\.\pipe\OAgentCredential-` + commandID
			stop, err := startNativeGitCredentialBroker(pipeName, commandID, owner.User.Sid.String(), owner.User.Sid.String(), logonSID, nativeGitCredentialBrokerStub{}, []string{"https://example.test/org/repo.git"})
			if err != nil {
				t.Fatalf("start command-scoped Git credential broker: %v", err)
			}
			t.Cleanup(func() {
				if err := stop(); err != nil {
					t.Errorf("stop credential broker during cleanup: %v", err)
				}
			})
			if scenario.connect {
				client, err := openNativeSSHAgentPipe(pipeName, 2*time.Second)
				if err != nil {
					t.Fatalf("connect client to credential broker: %v", err)
				}
				t.Cleanup(func() {
					if err := client.Close(); err != nil {
						t.Errorf("close credential pipe client: %v", err)
					}
				})
				if scenario.partialFrame {
					if _, err := client.Write([]byte{0, 0}); err != nil {
						t.Fatalf("write incomplete credential request: %v", err)
					}
				}
			}
			if err := stop(); err != nil {
				t.Fatalf("stop blocked credential broker: %v", err)
			}
			if err := stop(); err != nil {
				t.Fatalf("repeated credential broker stop was not idempotent: %v", err)
			}
			name, err := windows.UTF16PtrFromString(pipeName)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
			if err == nil {
				if closeErr := windows.CloseHandle(handle); closeErr != nil {
					t.Errorf("close unexpectedly surviving pipe: %v", closeErr)
				}
				t.Fatal("credential broker reported stopped but still accepts connections")
			}
			if err != windows.ERROR_FILE_NOT_FOUND {
				t.Fatalf("verify credential pipe removed: %v", err)
			}
		})
	}
}
