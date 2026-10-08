package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
)

func assertPasswordLoginConsumed(t *testing.T, s *Server, password string) *http.Cookie {
	t.Helper()
	response := authJSONRequest(t, s.Handler(), "/api/v1/auth/login", map[string]string{"email": "owner@example.com", "password": password}, nil)
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 {
		t.Fatalf("password login failed: %d %s", response.Code, response.Body.String())
	}
	cookie := response.Result().Cookies()[0]
	user, err := s.store.AuthSessionUser(context.Background(), hashToken(cookie.Value), time.Now())
	if err != nil || user != s.workspaceID {
		t.Fatalf("login did not persist the session: user=%q err=%v", user, err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	r.AddCookie(cookie)
	protected := httptest.NewRecorder()
	s.Handler().ServeHTTP(protected, r)
	if protected.Code != http.StatusOK {
		t.Fatalf("execution API did not consume login: %d", protected.Code)
	}
	return cookie
}

func TestPasswordLengthsPersistAcrossRegistrationRecoveryAndRestart(t *testing.T) {
	cases := []struct{ name, initial, replacement string }{
		{"short_to_long", "x", strings.Repeat("a", 2048) + "TAIL"},
		{"long_to_short", strings.Repeat("a", 2048) + "TAIL", "x"},
		{"spaces_preserved_and_unicode", " x ", "\u5bc6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, s, mailer := emailAuthTestServer(t, dir)
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()
			ctx := context.Background()
			for _, blank := range []string{"", " \t\u3000"} {
				rejected := authJSONRequest(t, s.Handler(), "/api/v1/auth/register/start", map[string]string{"email": "owner@example.com", "password": blank}, nil)
				_, hash, err := store.AuthUserByID(ctx, s.workspaceID)
				if rejected.Code != http.StatusBadRequest || err != nil || hash != "" || mailer.count != 0 {
					t.Fatal("blank registration changed credentials or sent mail")
				}
			}
			start := authJSONRequest(t, s.Handler(), "/api/v1/auth/register/start", map[string]string{"email": "owner@example.com", "password": tc.initial}, nil)
			if start.Code != http.StatusAccepted {
				t.Fatalf("registration start: %d %s", start.Code, start.Body.String())
			}
			registered := authJSONRequest(t, s.Handler(), "/api/v1/auth/register/verify", map[string]string{"challengeId": mailer.id, "code": mailer.code}, nil)
			if registered.Code != http.StatusOK {
				t.Fatalf("registration verify: %d %s", registered.Code, registered.Body.String())
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			// Advance only this temporary fixture past the mail cooldown. The
			// production one-minute guard remains active across restarts.
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "axiom.db")))
			if err != nil {
				t.Fatal(err)
			}
			result, updateErr := db.ExecContext(ctx, "UPDATE auth_email_challenges SET created_at=? WHERE id=? AND state='consumed'", time.Now().UTC().Add(-2*time.Minute), mailer.id)
			closeErr := db.Close()
			if updateErr != nil || closeErr != nil {
				t.Fatalf("fixture time update: %v %v", updateErr, closeErr)
			}
			if count, err := result.RowsAffected(); err != nil || count != 1 {
				t.Fatalf("fixture time update rows: %d %v", count, err)
			}
			store, s, mailer = emailAuthTestServer(t, dir)
			_, initialHash, err := store.AuthUserByID(ctx, s.workspaceID)
			if err != nil || !verifyPassword(tc.initial, initialHash) {
				t.Fatal("registered password did not survive restart")
			}
			cookie := assertPasswordLoginConsumed(t, s, tc.initial)
			wrong := authJSONRequest(t, s.Handler(), "/api/v1/auth/login", map[string]string{"email": "owner@example.com", "password": tc.initial + "different-tail"}, nil)
			if wrong.Code != http.StatusUnauthorized {
				t.Fatal("password was truncated or suffix ignored")
			}
			start = authJSONRequest(t, s.Handler(), "/api/v1/auth/password-reset/start", map[string]string{"email": "owner@example.com"}, nil)
			if start.Code != http.StatusAccepted {
				t.Fatalf("recovery start: %d", start.Code)
			}
			for _, blank := range []string{"", " \t\u3000"} {
				rejected := authJSONRequest(t, s.Handler(), "/api/v1/auth/password-reset/verify", map[string]string{"challengeId": mailer.id, "code": mailer.code, "password": blank}, nil)
				_, hash, err := store.AuthUserByID(ctx, s.workspaceID)
				proof, proofErr := store.AuthEmailChallenge(ctx, mailer.id)
				if rejected.Code != http.StatusBadRequest || err != nil || hash != initialHash || proofErr != nil || proof.State != "awaiting_code" {
					t.Fatal("blank reset changed credentials or consumed proof")
				}
				if _, err := store.AuthSessionUser(ctx, hashToken(cookie.Value), time.Now()); err != nil {
					t.Fatal("rejected reset revoked an existing session")
				}
			}
			reset := authJSONRequest(t, s.Handler(), "/api/v1/auth/password-reset/verify", map[string]string{"challengeId": mailer.id, "code": mailer.code, "password": tc.replacement}, nil)
			if reset.Code != http.StatusOK {
				t.Fatalf("reset: %d %s", reset.Code, reset.Body.String())
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, s, _ = emailAuthTestServer(t, dir)
			_, replacementHash, err := store.AuthUserByID(ctx, s.workspaceID)
			if err != nil || !verifyPassword(tc.replacement, replacementHash) || verifyPassword(tc.initial, replacementHash) {
				t.Fatal("reset credentials did not survive restart")
			}
			if _, err := store.AuthSessionUser(ctx, hashToken(cookie.Value), time.Now()); !errors.Is(err, domain.ErrUnauthorized) {
				t.Fatal("old session survived reset and restart")
			}
			assertPasswordLoginConsumed(t, s, tc.replacement)
			wrong = authJSONRequest(t, s.Handler(), "/api/v1/auth/login", map[string]string{"email": "owner@example.com", "password": tc.replacement + "different-tail"}, nil)
			if wrong.Code != http.StatusUnauthorized {
				t.Fatal("reset password was truncated")
			}
		})
	}
}

func TestBootstrapSetupAcceptsShortAndLongPasswords(t *testing.T) {
	for _, password := range []string{"x", strings.Repeat("a", 2048) + "TAIL"} {
		t.Run(map[bool]string{true: "long", false: "short"}[len(password) > 1024], func(t *testing.T) {
			store, s, mailer := emailAuthTestServer(t, t.TempDir())
			defer store.Close()
			setup := func(candidate string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/setup", strings.NewReader(`{"email":"owner@example.com","password":"`+candidate+`"}`))
				r.Header.Set("Origin", "https://o.example")
				r.Header.Set("X-O-Bootstrap-Token", "01234567890123456789012345678901")
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				return w
			}
			for _, blank := range []string{"", "   "} {
				response := setup(blank)
				_, hash, err := store.AuthUserByID(context.Background(), s.workspaceID)
				if response.Code != http.StatusBadRequest || err != nil || hash != "" || len(response.Result().Cookies()) != 0 {
					t.Fatal("blank bootstrap setup changed credentials")
				}
			}
			response := setup(password)
			_, hash, err := store.AuthUserByID(context.Background(), s.workspaceID)
			if response.Code != http.StatusOK || err != nil || !verifyPassword(password, hash) || mailer.count != 0 {
				t.Fatal("bootstrap password was not persisted")
			}
			assertPasswordLoginConsumed(t, s, password)
		})
	}
}
