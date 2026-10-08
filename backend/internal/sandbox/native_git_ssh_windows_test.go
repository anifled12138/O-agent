//go:build windows

package sandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestNativeGitNeedsSSHAgentForOfflineSSHSigning(t *testing.T) {
	tests := []struct {
		name    string
		scope   *nativeRepositoryScope
		network bool
		want    bool
	}{
		{name: "nil repository", scope: nil, network: true, want: false},
		{name: "https remote without signing", scope: &nativeRepositoryScope{RemoteURLs: []string{"https://github.com/o/r.git"}}, network: true, want: false},
		{name: "ssh remote offline without signing", scope: &nativeRepositoryScope{RemoteURLs: []string{"git@github.com:o/r.git"}}, network: false, want: false},
		{name: "ssh remote online", scope: &nativeRepositoryScope{RemoteURLs: []string{"git@github.com:o/r.git"}}, network: true, want: true},
		{name: "ssh signing while offline", scope: &nativeRepositoryScope{IdentitySigningFormat: "ssh", IdentitySigningKey: "key.pub"}, network: false, want: true},
		{name: "repository-local SSH signing while offline", scope: &nativeRepositoryScope{HasSSHSigningConfig: true}, network: false, want: true},
		{name: "openpgp signing", scope: &nativeRepositoryScope{IdentitySigningFormat: "openpgp", IdentitySigningKey: "key"}, network: false, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nativeGitNeedsSSHAgent(test.scope, test.network); got != test.want {
				t.Fatalf("nativeGitNeedsSSHAgent() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDiscoverNativeRepositoryScopeDetectsRepositoryLocalSSHSigning(t *testing.T) {
	gitPath, err := exec.LookPath("git.exe")
	if err != nil {
		t.Skip("Git is not installed on this Windows host")
	}
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", repository}, {"-C", repository, "config", "gpg.format", "ssh"}, {"-C", repository, "config", "user.signingkey", "keys/repository-signing.pub"}} {
		if output, err := exec.Command(gitPath, args...).CombinedOutput(); err != nil {
			t.Fatalf("configure temporary Git repository with %q: %v: %s", args, err, output)
		}
	}
	scope, err := discoverNativeRepositoryScope(context.Background(), repository, []string{repository}, false)
	if err != nil {
		t.Fatalf("discover local SSH signing configuration: %v", err)
	}
	if scope == nil || !scope.HasSSHSigningConfig {
		t.Fatalf("repository-local SSH signing config was not detected: %#v", scope)
	}
	if !nativeGitNeedsSSHAgent(scope, false) {
		t.Fatal("offline command with repository-local SSH signing config does not request the command-scoped agent")
	}
}

func TestIsNativeSSHRemoteURL(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{value: "git@github.com:owner/repo.git", valid: true},
		{value: "ssh://git@github.com/owner/repo.git", valid: true},
		{value: "ssh://github.com:2222/owner/repo.git", valid: true},
		{value: "ssh://[2001:db8::1]/owner/repo.git", valid: true},
		{value: "git@internal_host:owner/repo.git", valid: true},
		{value: "C:\\work\\repo", valid: false},
		{value: "owner/repo", valid: false},
		{value: "ssh://user:password@github.com/owner/repo.git", valid: false},
		{value: "ssh://github.com/owner/repo.git?token=secret", valid: false},
		{value: "ssh://-oProxyCommand=whoami@github.com/owner/repo.git", valid: false},
		{value: "-oProxyCommand=whoami:owner/repo.git", valid: false},
		{value: "-oProxyCommand=whoami@github.com:owner/repo.git", valid: false},
		{value: "git@-oProxyCommand=whoami:owner/repo.git", valid: false},
		{value: "git@github.com:/absolute/repo.git", valid: false},
		{value: "git@github.com:owner/repo.git\nmalicious", valid: false},
	} {
		t.Run(test.value, func(t *testing.T) {
			if got := isNativeSSHRemoteURL(test.value); got != test.valid {
				t.Fatalf("isNativeSSHRemoteURL(%q) = %v, want %v", test.value, got, test.valid)
			}
		})
	}
}

func TestSandboxAccountNamesAreStablePerWindowsUserSID(t *testing.T) {
	firstOffline, firstOnline := sandboxAccountNames("S-1-5-21-100-200-300-1001")
	repeatOffline, repeatOnline := sandboxAccountNames("S-1-5-21-100-200-300-1001")
	otherOffline, otherOnline := sandboxAccountNames("S-1-5-21-100-200-300-1002")
	if firstOffline != repeatOffline || firstOnline != repeatOnline {
		t.Fatal("the same Windows owner SID did not produce stable sandbox account names")
	}
	if firstOffline == otherOffline || firstOnline == otherOnline {
		t.Fatal("different Windows owner SIDs share a sandbox account name")
	}
	if firstOffline == firstOnline || len(firstOffline) > 20 || len(firstOnline) > 20 {
		t.Fatalf("sandbox account names violate local-account constraints: offline=%q online=%q", firstOffline, firstOnline)
	}
}

func TestNativeGitCredentialHelperUsesCommandScopedExecutable(t *testing.T) {
	runnerPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	commandID := "0123456789abcdef0123456789abcdef"
	pipeName := `\\.\pipe\OAgentCredential-` + commandID
	environment := map[string]string{"PATH": `C:\Windows\System32`, "GIT_CONFIG_COUNT": "0"}
	if err := prepareNativeGitCredentialHelper(environment, scratch, runnerPath, pipeName, commandID); err != nil {
		t.Fatal(err)
	}
	helperPath := filepath.Join(scratch, "git-helper", "git-credential-oagent-"+commandID+".exe")
	if helperID, ok := nativeGitCredentialHelperID(helperPath); !ok || helperID != commandID {
		t.Fatalf("generated helper executable does not identify its command lease: id=%q ok=%v", helperID, ok)
	}
	helperArgs, ok := nativeGitCredentialHelperArgs(helperPath, []string{"get"})
	wantArgs := []string{"--pipe", pipeName, "--command-id", commandID, "get"}
	if !ok || strings.Join(helperArgs, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("copied helper executable did not resolve to its lease IPC arguments: args=%q ok=%v", helperArgs, ok)
	}
	if environment["GIT_CONFIG_COUNT"] != "3" || environment["GIT_CONFIG_VALUE_1"] != "oagent-"+commandID || !strings.HasPrefix(environment["PATH"], filepath.Join(scratch, "git-helper")) {
		t.Fatalf("Git environment does not select the command-scoped executable: %#v", environment)
	}
	sourceHash, err := sha256File(runnerPath)
	if err != nil {
		t.Fatal(err)
	}
	helperHash, err := sha256File(helperPath)
	if err != nil {
		t.Fatal(err)
	}
	if sourceHash != helperHash {
		t.Fatal("command-scoped credential executable differs from the trusted runner")
	}
	for _, invalid := range []string{
		filepath.Join(filepath.Dir(helperPath), "git-credential-oagent-not-hex.exe"),
		filepath.Join(filepath.Dir(helperPath), "git-credential-oagent-0123456789abcdef0123456789abcdef.cmd"),
		filepath.Join(filepath.Dir(helperPath), "git-credential-other-0123456789abcdef0123456789abcdef.exe"),
	} {
		if id, ok := nativeGitCredentialHelperID(invalid); ok {
			t.Fatalf("invalid helper executable %q resolved to command %q", invalid, id)
		}
	}
}

func TestNativeSSHAgentBridgeAuthenticatesAndRelaysSelectedIdentity(t *testing.T) {
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
	upstreamPipe := `\\.\pipe\OAgentTestSSHHost-` + commandID
	upstreamName, err := windows.UTF16PtrFromString(upstreamPipe)
	if err != nil {
		t.Fatal(err)
	}
	upstreamHandle, err := createNativeSSHAgentServerPipe(upstreamName, owner.User.Sid.String(), logonSID.String(), true)
	if err != nil {
		t.Fatalf("create isolated fake Host agent pipe: %v", err)
	}
	upstreamDone := make(chan error, 1)
	keyBlob := appendSSHAgentString(nil, []byte("ssh-ed25519"))
	keyBlob = appendSSHAgentString(keyBlob, []byte("selected-test-key"))
	go func() {
		upstreamDone <- serveTestSSHAgent(upstreamHandle, keyBlob)
	}()

	commandPipe := `\\.\pipe\OAgentSSHAgent-` + commandID
	bridge, err := startNativeSSHAgentBridge(commandPipe, upstreamPipe, commandID, owner.User.Sid.String(), owner.User.Sid.String(), logonSID, [][]byte{keyBlob})
	if err != nil {
		_ = windows.CloseHandle(upstreamHandle)
		t.Fatalf("start command-scoped SSH agent broker: %v", err)
	}
	defer func() {
		if stopErr := bridge.stopAndWait(); stopErr != nil {
			t.Errorf("stop command-scoped SSH agent broker: %v", stopErr)
		}
		clear(keyBlob)
	}()

	client, err := openNativeSSHAgentPipe(commandPipe, 2*time.Second)
	if err != nil {
		t.Fatalf("connect to command-scoped SSH agent broker: %v", err)
	}
	if err := writeSSHAgentFrame(client, []byte{sshAgentRequestIdentities}); err != nil {
		t.Fatal(err)
	}
	identities, err := readSSHAgentFrame(client)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := sshAgentPublicKeys(identities)
	clear(identities)
	if err != nil || len(keys) != 1 || string(keys[0]) != string(keyBlob) {
		clearSSHAgentKeys(keys)
		t.Fatalf("command broker did not return the selected Host identity: keys=%d err=%v", len(keys), err)
	}
	clearSSHAgentKeys(keys)

	if err := writeSSHAgentFrame(client, sshAgentSignMessage(keyBlob)); err != nil {
		t.Fatal(err)
	}
	signature, err := readSSHAgentFrame(client)
	if err != nil || len(signature) == 0 || signature[0] != sshAgentSignResponse {
		t.Fatalf("command broker did not relay the selected-key signature: response=%v err=%v", signature, err)
	}
	clear(signature)
	_ = client.Close()
	idleClient, err := openNativeSSHAgentPipe(commandPipe, 2*time.Second)
	if err != nil {
		t.Fatalf("connect idle client before lease cleanup: %v", err)
	}
	if err := bridge.stopAndWait(); err != nil {
		t.Fatalf("command-scoped SSH agent broker cleanup failed: %v", err)
	}
	if err := idleClient.Close(); err != nil {
		t.Errorf("close idle command-side SSH agent client: %v", err)
	}
	select {
	case err := <-upstreamDone:
		if err != nil && !errors.Is(err, windows.ERROR_BROKEN_PIPE) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.EOF) {
			t.Fatalf("fake Host agent pipe returned an unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake Host agent connection did not close with the command lease")
	}
}

func serveTestSSHAgent(handle windows.Handle, key []byte) error {
	connectErr := windows.ConnectNamedPipe(handle, nil)
	if errors.Is(connectErr, windows.ERROR_PIPE_CONNECTED) {
		connectErr = nil
	}
	if connectErr != nil {
		_ = windows.CloseHandle(handle)
		return connectErr
	}
	connection := os.NewFile(uintptr(handle), "oagent-test-host-ssh-agent")
	defer connection.Close()
	for {
		request, err := readSSHAgentFrame(connection)
		if err != nil {
			return err
		}
		var response []byte
		switch {
		case len(request) == 1 && request[0] == sshAgentRequestIdentities:
			response = sshAgentIdentitiesMessage(key)
		case len(request) > 0 && request[0] == sshAgentSignRequest:
			response = []byte{sshAgentSignResponse, 0, 0, 0, 3, 's', 'i', 'g'}
		default:
			response = []byte{sshAgentFailure}
		}
		clear(request)
		writeErr := writeSSHAgentFrame(connection, response)
		clear(response)
		if writeErr != nil {
			return writeErr
		}
	}
}

func TestPrepareNativeSSHAgentEnvironmentUsesPublicIdentityOnly(t *testing.T) {
	hostHome := t.TempDir()
	t.Setenv("USERPROFILE", hostHome)
	scratch := filepath.Join(t.TempDir(), "command-scratch")
	commandHome := filepath.Join(scratch, "home")
	if err := os.MkdirAll(commandHome, 0o700); err != nil {
		t.Fatal(err)
	}
	knownHosts := []byte("github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample\n")
	if err := os.MkdirAll(filepath.Join(hostHome, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostHome, ".ssh", "known_hosts"), knownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	keyBlob := appendSSHAgentString(nil, []byte("ssh-ed25519"))
	keyBlob = appendSSHAgentString(keyBlob, []byte("public-key-material"))
	publicLine := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(keyBlob) + " test-key"
	privatePath := filepath.Join(hostHome, ".ssh", "id_ed25519")
	if err := os.WriteFile(privatePath, []byte("must never be read or copied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privatePath+".pub", []byte(publicLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"USERPROFILE":      commandHome,
		"GIT_CONFIG_COUNT": "0",
		"PATH":             "C:\\Windows\\System32",
	}
	pipe := `\\.\pipe\OAgentSSHAgent-command-id`
	if err := prepareNativeSSHAgentEnvironment(environment, scratch, pipe, [][]byte{keyBlob}, "ssh", privatePath, "true", "false"); err != nil {
		t.Fatal(err)
	}
	sshDir := filepath.Join(commandHome, ".ssh")
	config, err := os.ReadFile(filepath.Join(sshDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	configText := string(config)
	if !strings.Contains(configText, "StrictHostKeyChecking yes") || !strings.Contains(configText, "ForwardAgent no") || !strings.Contains(configText, `IdentityAgent //./pipe/OAgentSSHAgent-command-id`) {
		t.Fatalf("generated SSH config is missing required restrictions: %s", configText)
	}
	if !strings.Contains(configText, filepath.Join(sshDir, "identity-01.pub")) {
		t.Fatalf("generated SSH config does not reference the loaded public identity: %s", configText)
	}
	commandKnownHosts, err := os.ReadFile(filepath.Join(sshDir, "known_hosts"))
	if err != nil || string(commandKnownHosts) != string(knownHosts) {
		t.Fatalf("Host known_hosts was not copied into command scratch: contents=%q err=%v", commandKnownHosts, err)
	}
	identityBytes, err := os.ReadFile(filepath.Join(sshDir, "identity-01.pub"))
	identityFields := strings.Fields(string(identityBytes))
	if err != nil || len(identityFields) < 2 || identityFields[0] != "ssh-ed25519" || identityFields[1] != base64.StdEncoding.EncodeToString(keyBlob) {
		t.Fatalf("generated identity file is not the loaded public key: contents=%q err=%v", identityBytes, err)
	}
	privateScratchPath := filepath.Join(sshDir, filepath.Base(privatePath))
	if _, err := os.Stat(privateScratchPath); !os.IsNotExist(err) {
		t.Fatalf("Host private-key path was copied into scratch: stat err=%v", err)
	}
	if environment["GIT_CONFIG_COUNT"] != "5" || environment["GIT_CONFIG_VALUE_1"] != "ssh" || environment["GIT_CONFIG_KEY_2"] != "user.signingkey" || environment["GIT_CONFIG_KEY_3"] != "commit.gpgsign" || environment["GIT_CONFIG_VALUE_3"] != "true" || environment["GIT_CONFIG_KEY_4"] != "tag.gpgsign" || environment["GIT_CONFIG_VALUE_4"] != "false" {
		t.Fatalf("selected Host signing identity was not scoped to Git: %#v", environment)
	}
	signingBytes, err := os.ReadFile(environment["GIT_CONFIG_VALUE_2"])
	signingFields := strings.Fields(string(signingBytes))
	if err != nil || len(signingFields) < 2 || signingFields[0] != "ssh-ed25519" || signingFields[1] != base64.StdEncoding.EncodeToString(keyBlob) {
		t.Fatalf("Git signing identity is not a public key in command scratch: contents=%q err=%v", signingBytes, err)
	}
}

func TestPrepareNativeSSHAgentEnvironmentDoesNotParseOpenPGPSigningKeyAsSSH(t *testing.T) {
	hostHome := t.TempDir()
	t.Setenv("USERPROFILE", hostHome)
	scratch := filepath.Join(t.TempDir(), "command-scratch")
	commandHome := filepath.Join(scratch, "home")
	if err := os.MkdirAll(commandHome, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"USERPROFILE":      commandHome,
		"GIT_CONFIG_COUNT": "0",
		"PATH":             "C:\\Windows\\System32",
	}
	keyBlob := appendSSHAgentString(nil, []byte("ssh-ed25519"))
	keyBlob = appendSSHAgentString(keyBlob, []byte("public-key-material"))
	err := prepareNativeSSHAgentEnvironment(environment, scratch, `\\.\pipe\OAgentSSHAgent-command-id`, [][]byte{keyBlob}, "openpgp", "not-an-ssh-public-key-path", "true", "true")
	if err != nil {
		t.Fatalf("OpenPGP signing identity was incorrectly parsed as SSH: %v", err)
	}
	if environment["GIT_CONFIG_COUNT"] != "1" || environment["GIT_CONFIG_KEY_0"] != "core.sshCommand" {
		t.Fatalf("SSH setup changed unrelated OpenPGP signing configuration: %#v", environment)
	}
}

func TestPrepareNativeSSHSigningEnvironmentPreservesSigningWhenAgentIsUnavailable(t *testing.T) {
	hostHome := t.TempDir()
	t.Setenv("USERPROFILE", hostHome)
	scratch := filepath.Join(t.TempDir(), "command-scratch")
	commandHome := filepath.Join(scratch, "home")
	if err := os.MkdirAll(commandHome, 0o700); err != nil {
		t.Fatal(err)
	}
	keyBlob := appendSSHAgentString(nil, []byte("ssh-ed25519"))
	keyBlob = appendSSHAgentString(keyBlob, []byte("selected-public-key"))
	publicPath := filepath.Join(hostHome, ".ssh", "signing.pub")
	if err := os.MkdirAll(filepath.Dir(publicPath), 0o700); err != nil {
		t.Fatal(err)
	}
	publicLine := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(keyBlob) + " host-signing-key"
	if err := os.WriteFile(publicPath, []byte(publicLine+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"USERPROFILE":         commandHome,
		"HOME":                commandHome,
		"PATH":                `C:\Windows\System32`,
		"SYSTEMROOT":          `C:\Windows`,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   nativeGitNullPath,
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "safe.directory",
		"GIT_CONFIG_VALUE_0":  filepath.Join(scratch, "repo"),
	}
	missingAgentPipe := `\\.\pipe\OAgentSSHAgent-0123456789abcdef0123456789abcdef`
	if err := prepareNativeSSHSigningEnvironment(environment, scratch, missingAgentPipe, publicPath, "true", "false"); err != nil {
		t.Fatal(err)
	}
	if environment["SSH_AUTH_SOCK"] != missingAgentPipe {
		t.Fatalf("unavailable command lease was not isolated from the Host agent: SSH_AUTH_SOCK=%q", environment["SSH_AUTH_SOCK"])
	}
	if environment["GIT_CONFIG_COUNT"] != "5" || environment["GIT_CONFIG_KEY_1"] != "gpg.format" || environment["GIT_CONFIG_VALUE_1"] != "ssh" ||
		environment["GIT_CONFIG_KEY_2"] != "user.signingkey" || environment["GIT_CONFIG_KEY_3"] != "commit.gpgsign" || environment["GIT_CONFIG_VALUE_3"] != "true" ||
		environment["GIT_CONFIG_KEY_4"] != "tag.gpgsign" || environment["GIT_CONFIG_VALUE_4"] != "false" {
		t.Fatalf("SSH signing policy was not preserved in command-scoped Git config: %#v", environment)
	}
	signingPath := environment["GIT_CONFIG_VALUE_2"]
	if !pathWithin(scratch, signingPath) {
		t.Fatalf("signing public key escaped command scratch: %q", signingPath)
	}
	contents, err := os.ReadFile(signingPath)
	if err != nil || !strings.HasPrefix(string(contents), publicLine) {
		t.Fatalf("selected public signing identity was not copied into scratch: contents=%q err=%v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(commandHome, ".ssh", "signing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Host private signing key was copied into command scratch: %v", err)
	}
	if gitPath, err := exec.LookPath("git.exe"); err == nil {
		for key, want := range map[string]string{
			"gpg.format":      "ssh",
			"commit.gpgsign":  "true",
			"tag.gpgsign":     "false",
			"user.signingkey": signingPath,
		} {
			command := exec.Command(gitPath, "config", "--get", key)
			command.Dir = scratch
			command.Env = environmentEntries(environment)
			output, err := command.CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != want {
				t.Fatalf("Git did not consume command-scoped %s=%q (output=%q err=%v)", key, want, output, err)
			}
		}
	}
}

func TestUnavailableSSHSigningKeyDoesNotBlockOrdinaryGitOrDisableSigning(t *testing.T) {
	hostHome := t.TempDir()
	t.Setenv("USERPROFILE", hostHome)
	scratch := filepath.Join(t.TempDir(), "command-scratch")
	commandHome := filepath.Join(scratch, "home")
	if err := os.MkdirAll(commandHome, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"USERPROFILE": commandHome, "GIT_CONFIG_COUNT": "0"}
	missingAgentPipe := `\\.\pipe\OAgentSSHAgent-0123456789abcdef0123456789abcdef`
	missingPublicKey := filepath.Join(hostHome, ".ssh", "missing-signing.pub")
	if err := prepareNativeSSHSigningEnvironment(environment, scratch, missingAgentPipe, missingPublicKey, "true", "true"); err != nil {
		t.Fatalf("unavailable selected signing key blocked ordinary command setup: %v", err)
	}
	if environment["GIT_CONFIG_COUNT"] != "4" || environment["GIT_CONFIG_KEY_0"] != "gpg.format" || environment["GIT_CONFIG_VALUE_0"] != "ssh" ||
		environment["GIT_CONFIG_KEY_1"] != "user.signingkey" || environment["GIT_CONFIG_KEY_2"] != "commit.gpgsign" || environment["GIT_CONFIG_VALUE_2"] != "true" ||
		environment["GIT_CONFIG_KEY_3"] != "tag.gpgsign" || environment["GIT_CONFIG_VALUE_3"] != "true" {
		t.Fatalf("missing key caused signing settings to be dropped: %#v", environment)
	}
	if _, err := os.Stat(environment["GIT_CONFIG_VALUE_1"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing selected key unexpectedly exists in command scratch: %v", err)
	}
}
func TestDiscoverNativeRepositoryScopeAcceptsDOSPathAlias(t *testing.T) {
	gitPath, err := exec.LookPath("git.exe")
	if err != nil {
		t.Skip("Git is not installed")
	}
	directory := filepath.Join(t.TempDir(), "repository with a long directory name")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	length, err := windows.GetShortPathName(name, &buffer[0], uint32(len(buffer)))
	if err != nil || length == 0 || length >= uint32(len(buffer)) {
		t.Fatalf("read DOS directory alias: length=%d err=%v", length, err)
	}
	alias := windows.UTF16ToString(buffer[:length])
	longPath, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if strings.EqualFold(alias, longPath) {
		t.Skip("this filesystem does not provide a DOS 8.3 alias")
	}
	if output, err := exec.Command(gitPath, "init", directory).CombinedOutput(); err != nil {
		t.Fatalf("initialize alias-backed Git repository: %v: %s", err, output)
	}
	scope, err := discoverNativeRepositoryScope(context.Background(), alias, []string{longPath}, false)
	if err != nil || scope == nil {
		t.Fatalf("authorized DOS path alias was rejected: scope=%v err=%v", scope, err)
	}
	volume, index, err := nativeFileIdentity(longPath)
	if err != nil {
		t.Fatal(err)
	}
	actualVolume, actualIndex, err := nativeFileIdentity(scope.Workspace)
	if err != nil || actualVolume != volume || actualIndex != index {
		t.Fatalf("repository scope changed filesystem identity: volume=%d index=%d err=%v", actualVolume, actualIndex, err)
	}
	if !pathWithin(longPath, scope.Workspace) {
		t.Fatalf("normalized scope escaped the authorized directory: %s", scope.Workspace)
	}
}
