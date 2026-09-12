package config

import "testing"

func TestLoadPrefersOEnvironment(t *testing.T) {
	t.Setenv("O_ADDR", "127.0.0.1:9000")
	t.Setenv("AXIOM_ADDR", "127.0.0.1:9001")
	if got := Load().Addr; got != "127.0.0.1:9000" {
		t.Fatalf("O_ADDR was not preferred: %q", got)
	}
}

func TestLoadAcceptsLegacyEnvironment(t *testing.T) {
	t.Setenv("O_ADDR", "")
	t.Setenv("AXIOM_ADDR", "127.0.0.1:9001")
	if got := Load().Addr; got != "127.0.0.1:9001" {
		t.Fatalf("legacy AXIOM_ADDR was not accepted: %q", got)
	}
}
