package pluginruntime

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginmanifest"
)

type testSecrets struct{}

func (testSecrets) ResolvePluginSecret(context.Context, string, string) ([]byte, error) {
	return []byte("resolved"), nil
}

func brokerRelease() pluginforge.Release {
	return pluginforge.Release{ID: "rel_broker", PluginID: "example.broker", Manifest: pluginmanifest.Manifest{Permissions: pluginmanifest.Permissions{
		Filesystem: pluginmanifest.FilesystemPermissions{Read: []string{"${workspace}"}, Write: []string{"${pluginData}"}},
		Network:    []string{"api.example.com"}, Secrets: []string{"example.token"}, Process: true,
	}}}
}

func TestFilesystemBrokerEnforcesScopeAndTraversal(t *testing.T) {
	workspace := t.TempDir()
	dataDir := filepath.Join(workspace, "work", "broker-test")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# test"), 0o600); err != nil {
		t.Fatal(err)
	}
	broker, err := NewResourceBroker(workspace, dataDir, testSecrets{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := broker.ReadFile(brokerRelease(), "${workspace}", "README.md")
	if err != nil || len(value) == 0 {
		t.Fatalf("expected brokered read: %v", err)
	}
	if _, err = broker.ReadFile(brokerRelease(), "${workspace}", "../secret.txt"); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if _, err = broker.ReadFile(brokerRelease(), "${pluginData}", "state.json"); err == nil {
		t.Fatal("read permission was inferred from write permission")
	}
}

func TestSecretAndProcessBrokersRequireExactGrants(t *testing.T) {
	workspace := t.TempDir()
	dataDir := filepath.Join(workspace, "work", "broker-test")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	commands := map[string]BrokerCommand{"trusted.inspect": func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}}
	broker, err := NewResourceBroker(workspace, dataDir, testSecrets{}, commands)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := broker.Secret(context.Background(), "user", brokerRelease(), "example.token"); err != nil || string(value) != "resolved" {
		t.Fatalf("secret grant failed: %v", err)
	}
	if _, err := broker.Secret(context.Background(), "user", brokerRelease(), "other.token"); err == nil {
		t.Fatal("ungranted secret was resolved")
	}
	if _, err := broker.Run(context.Background(), brokerRelease(), "arbitrary.exe", nil); err == nil {
		t.Fatal("arbitrary process execution was accepted")
	}
}

func TestNetworkBrokerRejectsNonPublicAddresses(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "169.254.1.1", "100.64.0.1", "::1", "fc00::1"}
	for _, raw := range blocked {
		if publicBrokerAddress(netip.MustParseAddr(raw)) {
			t.Fatalf("address %s should be blocked", raw)
		}
	}
	if !publicBrokerAddress(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("public address was blocked")
	}
}
