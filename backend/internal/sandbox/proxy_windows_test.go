//go:build windows

package sandbox

import (
	"reflect"
	"testing"
)

func TestParseWindowsProxyServer(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{name: "single proxy", input: "127.0.0.1:7897", want: map[string]string{"HTTP_PROXY": "http://127.0.0.1:7897", "HTTPS_PROXY": "http://127.0.0.1:7897"}},
		{name: "per protocol", input: "http=127.0.0.1:7897;https=https://proxy.example:8443;socks=socks.example:1080", want: map[string]string{"HTTP_PROXY": "http://127.0.0.1:7897", "HTTPS_PROXY": "https://proxy.example:8443", "ALL_PROXY": "socks5://socks.example:1080"}},
		{name: "ignore unrelated protocol", input: "ftp=proxy.example:21;http=proxy.example:8080", want: map[string]string{"HTTP_PROXY": "http://proxy.example:8080"}},
		{name: "malformed", input: "http=", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseWindowsProxyServer(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseWindowsProxyServer() error = %v, wantErr %v", err, test.wantErr)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseWindowsProxyServer() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestWindowsProxyOverrideToNoProxy(t *testing.T) {
	got := windowsProxyOverrideToNoProxy("localhost;*.example.com;<local>;10.*")
	want := "localhost,.example.com,localhost,127.0.0.1,::1,10.*"
	if got != want {
		t.Fatalf("windowsProxyOverrideToNoProxy() = %q, want %q", got, want)
	}
}

func TestProxyNeedsLoopbackExemption(t *testing.T) {
	for _, test := range []struct {
		name        string
		environment map[string]string
		want        bool
	}{
		{name: "ipv4 loopback", environment: map[string]string{"HTTP_PROXY": "http://127.0.0.1:7897"}, want: true},
		{name: "localhost", environment: map[string]string{"HTTPS_PROXY": "http://localhost:8080"}, want: true},
		{name: "ipv6 loopback", environment: map[string]string{"ALL_PROXY": "socks5://[::1]:1080"}, want: true},
		{name: "remote proxy", environment: map[string]string{"HTTPS_PROXY": "http://proxy.example:8080"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := proxyNeedsLoopbackExemption(test.environment); got != test.want {
				t.Fatalf("proxyNeedsLoopbackExemption() = %v, want %v", got, test.want)
			}
		})
	}
}
