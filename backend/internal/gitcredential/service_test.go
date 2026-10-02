package gitcredential

import (
	"context"
	"errors"
	"testing"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func TestCredentialScopesAreRepositoryExactAndCaseSensitive(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, err := secure.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(context.Background(), store, vault)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(context.Background(), Credential{ID: "repo", Host: "GitHub.com", Repository: "Acme/Widget.git", Username: "git", Password: "token"}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		url  string
		want bool
	}{
		{"https://github.com/Acme/Widget", true},
		{"https://GITHUB.com/Acme/Widget.git", true},
		{"https://github.com/acme/Widget", false},
		{"https://github.com/Acme/Widget/child", false},
		{"https://github.com/Acme/Widget%2Fchild", false},
		{"http://github.com/Acme/Widget", false},
	} {
		_, _, ok := service.Lookup(test.url)
		if ok != test.want {
			t.Errorf("Lookup(%q) ok=%t, want %t", test.url, ok, test.want)
		}
	}
}

func TestCredentialSaveSurvivesServiceAndStoreRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secure.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(ctx, store, vault)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(ctx, Credential{ID: "gh", Host: "github.com", Repository: "acme/o-agent", Username: "git", Password: "secret-token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedStore.Close()
	reopenedVault, err := secure.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	reopenedService, err := New(ctx, reopenedStore, reopenedVault)
	if err != nil {
		t.Fatal(err)
	}
	user, password, ok := reopenedService.Lookup("https://github.com/acme/o-agent.git")
	if !ok || user != "git" || password != "secret-token" {
		t.Fatalf("durable credential was not consumed after restart: user=%q password=%q ok=%t", user, password, ok)
	}
	for _, summary := range reopenedService.List() {
		if summary.Username != "git" || summary.ID != "gh" {
			t.Fatalf("unexpected persisted credential summary: %+v", summary)
		}
	}
	if len(reopenedService.List()) != 1 {
		t.Fatalf("persisted credential list has wrong length: %+v", reopenedService.List())
	}
}

func TestCredentialSaveRollsBackWhenAuthoritativeReadbackFails(t *testing.T) {
	ctx := context.Background()
	store := &faultingSettingsStore{values: map[string]string{}, failReadAt: 3}
	vault, err := secure.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(ctx, store, vault)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Save(ctx, Credential{ID: "gh", Host: "github.com", Repository: "acme/o-agent", Username: "git", Password: "secret-token"}); err == nil {
		t.Fatal("Save reported success after authoritative read-back failed")
	}
	if len(service.List()) != 0 {
		t.Fatalf("failed Save changed in-memory state: %+v", service.List())
	}
	if _, _, ok := service.Lookup("https://github.com/acme/o-agent"); ok {
		t.Fatal("failed Save left the credential available to the execution path")
	}
	if _, err := store.RuntimeSetting(ctx, settingKey); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("failed Save did not durably roll back its setting: %v", err)
	}
}

func TestCredentialSaveVerifiesCommittedWriteAfterRequestCancellation(t *testing.T) {
	store := &faultingSettingsStore{values: map[string]string{}}
	vault, err := secure.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(context.Background(), store, vault)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	store.cancelAfterSet = cancel
	if _, err := service.Save(ctx, Credential{ID: "gh", Host: "github.com", Repository: "acme/o-agent", Username: "git", Password: "secret-token"}); err != nil {
		t.Fatalf("durable credential write was reported as a failed mutation after caller cancellation: %v", err)
	}
	if _, _, ok := service.Lookup("https://github.com/acme/o-agent"); !ok {
		t.Fatal("service did not publish the credential after verifying the committed setting")
	}
}

type faultingSettingsStore struct {
	values         map[string]string
	readCount      int
	failReadAt     int
	cancelAfterSet context.CancelFunc
}

func (s *faultingSettingsStore) RuntimeSetting(_ context.Context, key string) (string, error) {
	s.readCount++
	if s.readCount == s.failReadAt {
		return "", errors.New("injected authoritative read-back failure")
	}
	value, ok := s.values[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return value, nil
}

func (s *faultingSettingsStore) SetRuntimeSetting(_ context.Context, key, value string) error {
	s.values[key] = value
	if s.cancelAfterSet != nil {
		s.cancelAfterSet()
	}
	return nil
}

func (s *faultingSettingsStore) DeleteRuntimeSetting(_ context.Context, key string) error {
	delete(s.values, key)
	return nil
}
