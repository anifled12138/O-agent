//go:build linux

package coretools

import (
	"bufio"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/browsercontrol"
)

func TestBrowserRequestCancellationStopsBlockedPipeRead(t *testing.T) {
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	defer requestReader.Close()
	defer responseWriter.Close()
	requestLine := make(chan struct{})
	go func() {
		_, _ = bufio.NewReader(requestReader).ReadString('\n')
		close(requestLine)
	}()
	sessionContext, stopSession := context.WithCancel(context.Background())
	defer stopSession()
	ctx, cancel := context.WithCancel(context.Background())
	session := &linuxBrowserSession{
		cancel: stopSession, stdin: requestWriter, response: responseReader,
		stdout: bufio.NewReader(responseReader), done: make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		_, err := session.request(ctx, browsercontrol.Request{Action: "inspect"})
		result <- err
	}()
	select {
	case <-requestLine:
	case <-time.After(time.Second):
		t.Fatal("browser worker did not receive the request")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("request error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled browser request remained blocked on the response pipe")
	}
	if sessionContext.Err() != context.Canceled {
		t.Fatalf("browser worker context = %v, want canceled", sessionContext.Err())
	}
}

func TestParseLinuxMemAvailable(t *testing.T) {
	available, err := parseLinuxMemAvailable(strings.NewReader("MemTotal: 4000000 kB\nMemAvailable: 2048000 kB\n"))
	if err != nil || available != 2<<30 {
		t.Fatalf("available=%d err=%v, want 2 GiB", available, err)
	}
	for _, input := range []string{"MemTotal: 1000 kB\n", "MemAvailable: -1 kB\n", "MemAvailable: not-a-number kB\n", "MemAvailable: 1 MB\n"} {
		if _, err := parseLinuxMemAvailable(strings.NewReader(input)); err == nil {
			t.Errorf("invalid meminfo %q was accepted", input)
		}
	}
}

func TestNormalizeBrowserHostsExactPublicNames(t *testing.T) {
	hosts, err := normalizeBrowserHosts([]string{"Example.com", "example.com.", "2001:db8::1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2001:db8::1", "example.com"}
	if !reflect.DeepEqual(hosts, want) {
		t.Fatalf("normalized hosts = %#v, want %#v", hosts, want)
	}
	for _, invalid := range []string{"", "example.com:443", "https://example.com", "a..example.com", "not a host"} {
		if _, err := normalizeBrowserHosts([]string{invalid}); err == nil {
			t.Errorf("invalid allow-list host %q was accepted", invalid)
		}
	}
}

func TestParseBrowserURLRejectsNonHTTPAndEmbeddedCredentials(t *testing.T) {
	for _, invalid := range []string{"", "file:///etc/passwd", "ftp://example.com/file", "https://user:secret@example.com/"} {
		if _, err := parseBrowserURL(invalid); err == nil {
			t.Errorf("invalid browser URL %q was accepted", invalid)
		}
	}
	if parsed, err := parseBrowserURL("https://example.com/path"); err != nil || parsed.Hostname() != "example.com" {
		t.Fatalf("valid browser URL did not parse: parsed=%v err=%v", parsed, err)
	}
}
