//go:build linux

package browsercontrol

import "testing"

func TestLocalCDPURLRequiresLoopbackPageEndpoint(t *testing.T) {
	for _, raw := range []string{
		"ws://127.0.0.1:9222/devtools/page/target",
		"ws://[::1]:9222/devtools/page/target",
		"ws://localhost:9222/devtools/page/target",
	} {
		if _, err := localCDPURL(raw); err != nil {
			t.Errorf("local CDP URL %q rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"ws://example.com:9222/devtools/page/target",
		"wss://127.0.0.1:9222/devtools/page/target",
		"ws://127.0.0.1:9222/devtools/browser/target",
		"ws://127.0.0.1/devtools/page/target",
	} {
		if _, err := localCDPURL(raw); err == nil {
			t.Errorf("non-local CDP URL %q was accepted", raw)
		}
	}
}

func TestBrowserPressKeyAllowList(t *testing.T) {
	for _, key := range []string{"Enter", "Tab", "Escape", "Backspace", "Delete", "Space", "ArrowDown", "ArrowUp", "ArrowLeft", "ArrowRight"} {
		if !allowedBrowserKey(key) {
			t.Errorf("expected key %q to be supported", key)
		}
	}
	for _, key := range []string{"F12", "Control+L", ""} {
		if allowedBrowserKey(key) {
			t.Errorf("unexpected key %q was supported", key)
		}
	}
}
