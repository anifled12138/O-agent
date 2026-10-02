package projectpolicy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMeasureCountsGitIgnoredAndGeneratedFiles(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"src/main.go":            "source",
		".git/objects/mock":      "gitdata",
		"node_modules/pkg/cache": "ignored and generated",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Measure(root)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(len("source") + len("gitdata") + len("ignored and generated"))
	if got != want {
		t.Fatalf("Measure() = %d, want %d", got, want)
	}
}

func TestMeasureCountsSymlinkWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("outside data that must not be counted"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	got, err := Measure(root)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(len(target))
	if got != want {
		t.Fatalf("Measure() = %d, want symlink target path bytes %d (without following target)", got, want)
	}
}

func TestValidateRepositoryURL(t *testing.T) {
	tests := []struct {
		input, provider, normalized string
		valid                       bool
	}{
		{"https://github.com/acme/oagent", "github", "https://github.com/acme/oagent.git", true},
		{"https://gitee.com/acme/oagent.git", "gitee", "https://gitee.com/acme/oagent.git", true},
		{"https://user:secret@github.com/acme/oagent.git", "", "", false},
		{"ssh://git@github.com/acme/oagent.git", "", "", false},
		{"https://github.com/acme/oagent/tree/main", "", "", false},
		{"https://example.com/acme/oagent.git", "", "", false},
		{"https://github.com/acme%2Foagent.git", "", "", false},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			provider, normalized, err := ValidateRepositoryURL(test.input)
			if test.valid {
				if err != nil || provider != test.provider || normalized != test.normalized {
					t.Fatalf("ValidateRepositoryURL() = %q, %q, %v", provider, normalized, err)
				}
			} else if !errors.Is(err, ErrInvalidRepoURL) {
				t.Fatalf("ValidateRepositoryURL() error = %v, want ErrInvalidRepoURL", err)
			}
		})
	}
}

func TestTransferThresholdNeverActsAsFeatureLimit(t *testing.T) {
	if NeedsChunkedTransfer(DirectTransferBatchBytes) {
		t.Fatal("exactly 500 MB should remain eligible for a direct transfer batch")
	}
	if !NeedsChunkedTransfer(DirectTransferBatchBytes + 1) {
		t.Fatal("larger data should select incremental transfer without being rejected")
	}
}
