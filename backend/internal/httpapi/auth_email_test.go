package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

type authTestMailer struct {
	code, id, purpose string
	count             int
	err               error
}

func (m *authTestMailer) SendVerification(_ context.Context, _ string, code, purpose, id string) (string, error) {
	m.code, m.id, m.purpose = code, id, purpose
	m.count++
	if m.err != nil {
		return "", m.err
	}
	return "provider-" + id, nil
}

type authTestHuman struct{}

func (authTestHuman) Verify(_ context.Context, token, action string) error {
	if token != "human-"+action {
		return errors.New("请完成人机验证")
	}
	return nil
}
func emailAuthTestServer(t *testing.T, dir string) (*storage.Store, *Server, *authTestMailer) {
	t.Helper()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.EnsureLocalWorkspaceOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{workspaceID: owner, store: store, frontendOrigin: "https://o.example"}
	s.SetRemoteAuthentication("01234567890123456789012345678901")
	if err := s.SetEmailAuthentication(EmailAuthConfig{OwnerEmail: "owner@example.com", ResendAPIKey: "test-provider-key", MailFrom: "O <auth@example.com>"}); err != nil {
		t.Fatal(err)
	}
	mailer := &authTestMailer{}
	s.authMailer = mailer
	return store, s, mailer
}
func authJSONRequest(t *testing.T, h http.Handler, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.RemoteAddr = "203.0.113.12:1234"
	r.Header.Set("Origin", "https://o.example")
	r.Header.Set("X-Forwarded-Proto", "https")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestEmailRegistrationSurvivesRestartAndCannotReplay(t *testing.T) {
	dir := t.TempDir()
	store, s, mailer := emailAuthTestServer(t, dir)
	h := s.Handler()
	wrong := authJSONRequest(t, h, "/api/v1/auth/register/start", map[string]string{"email": "outsider@example.com", "password": "a-long-test-password"}, nil)
	if wrong.Code != http.StatusAccepted || mailer.count != 0 {
		t.Fatalf("unexpected recipient result %d mail=%d", wrong.Code, mailer.count)
	}
	start := authJSONRequest(t, h, "/api/v1/auth/register/start", map[string]string{"email": "owner@example.com", "password": "a-long-test-password", "displayName": "Owner"}, nil)
	if start.Code != http.StatusAccepted {
		t.Fatalf("start %d: %s", start.Code, start.Body.String())
	}
	id, code, owner := mailer.id, mailer.code, s.workspaceID
	candidate, err := store.AuthEmailChallenge(context.Background(), id)
	if err != nil || candidate.State != "awaiting_code" || candidate.ProviderID == "" {
		t.Fatalf("challenge receipt not durable: %+v %v", candidate, err)
	}
	_, hash, err := store.AuthUserByID(context.Background(), owner)
	if err != nil || hash != "" {
		t.Fatal("registration changed credentials before verification")
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, s, _ = emailAuthTestServer(t, dir)
	defer store.Close()
	h = s.Handler()
	verify := authJSONRequest(t, h, "/api/v1/auth/register/verify", map[string]string{"challengeId": id, "code": code}, nil)
	if verify.Code != http.StatusOK {
		t.Fatalf("verify %d: %s", verify.Code, verify.Body.String())
	}
	user, hash, err := store.AuthUserByEmail(context.Background(), "owner@example.com")
	if err != nil || user.ID != owner || !verifyPassword("a-long-test-password", hash) {
		t.Fatal("registered credentials not read back")
	}
	cookie := verify.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly {
		t.Fatal("cloud cookie lacks security flags")
	}
	protected := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	r.AddCookie(cookie)
	h.ServeHTTP(protected, r)
	if protected.Code != http.StatusOK {
		t.Fatalf("registered session not consumed by protected runtime: %d", protected.Code)
	}
	replay := authJSONRequest(t, h, "/api/v1/auth/register/verify", map[string]string{"challengeId": id, "code": code}, nil)
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed proof=%d", replay.Code)
	}
	candidate, err = store.AuthEmailChallenge(context.Background(), id)
	if err != nil || candidate.State != "consumed" || candidate.CodeHash != "" || candidate.PasswordHash != "" {
		t.Fatal("proof or candidate password retained after consumption")
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, s, _ = emailAuthTestServer(t, dir)
	defer store.Close()
	session := httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil)
	r.AddCookie(cookie)
	s.Handler().ServeHTTP(session, r)
	if session.Code != http.StatusOK {
		t.Fatal("registered session did not survive runtime restart")
	}
}
func TestEmailPasswordRecoveryRevokesOldSessionsAndProof(t *testing.T) {
	store, s, mailer := emailAuthTestServer(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	oldHash, err := hashPassword("old-password-for-test")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetInitialCredentials(ctx, s.workspaceID, "owner@example.com", "Owner", oldHash); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateAuthSession(ctx, hashToken("old-cookie"), s.workspaceID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	start := authJSONRequest(t, h, "/api/v1/auth/password-reset/start", map[string]string{"email": "owner@example.com"}, nil)
	if start.Code != http.StatusAccepted {
		t.Fatalf("start %d %s", start.Code, start.Body.String())
	}
	verify := authJSONRequest(t, h, "/api/v1/auth/password-reset/verify", map[string]string{"challengeId": mailer.id, "code": mailer.code, "password": "new-password-for-test"}, nil)
	if verify.Code != http.StatusOK {
		t.Fatalf("reset %d %s", verify.Code, verify.Body.String())
	}
	_, stored, err := store.AuthUserByID(ctx, s.workspaceID)
	if err != nil || !verifyPassword("new-password-for-test", stored) || verifyPassword("old-password-for-test", stored) {
		t.Fatal("reset password not applied")
	}
	if _, err = store.AuthSessionUser(ctx, hashToken("old-cookie"), time.Now()); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("old session survived reset")
	}
	if err = store.CreateAuthSessionForCredentials(ctx, hashToken("inflight-old-login"), s.workspaceID, oldHash, time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("old password in-flight login obtained a new session")
	}
	replay := authJSONRequest(t, h, "/api/v1/auth/password-reset/verify", map[string]string{"challengeId": mailer.id, "code": mailer.code, "password": "second-password-for-test"}, nil)
	if replay.Code != http.StatusUnauthorized {
		t.Fatal("password reset proof reused")
	}
	login := authJSONRequest(t, h, "/api/v1/auth/login", map[string]string{"email": "owner@example.com", "password": "new-password-for-test"}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("new password login %d %s", login.Code, login.Body.String())
	}
	logoutAll := authJSONRequest(t, h, "/api/v1/auth/logout-all", map[string]string{}, login.Result().Cookies()[0])
	if logoutAll.Code != http.StatusOK {
		t.Fatal("logout-all failed")
	}
	if _, err = store.AuthSessionUser(ctx, hashToken(login.Result().Cookies()[0].Value), time.Now()); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("logout-all did not revoke consumed session")
	}
}
func TestEmailDeliveryFailureCannotInitializeAccount(t *testing.T) {
	store, s, mailer := emailAuthTestServer(t, t.TempDir())
	defer store.Close()
	mailer.err = errors.New("provider unavailable")
	start := authJSONRequest(t, s.Handler(), "/api/v1/auth/register/start", map[string]string{"email": "owner@example.com", "password": "a-long-test-password"}, nil)
	if start.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed send %d", start.Code)
	}
	c, err := store.AuthEmailChallenge(context.Background(), mailer.id)
	if err != nil || c.State != "delivery_unconfirmed" {
		t.Fatalf("failed provider state %+v %v", c, err)
	}
	verify := authJSONRequest(t, s.Handler(), "/api/v1/auth/register/verify", map[string]string{"challengeId": mailer.id, "code": mailer.code}, nil)
	if verify.Code != http.StatusUnauthorized {
		t.Fatalf("unconfirmed delivery authorized registration: %d", verify.Code)
	}
	_, hash, err := store.AuthUserByID(context.Background(), s.workspaceID)
	if err != nil || hash != "" {
		t.Fatal("failed mail changed account")
	}
}
func TestLocalUseRemainsUnauthenticatedAndRemoteRequiresSession(t *testing.T) {
	store, s, _ := emailAuthTestServer(t, t.TempDir())
	defer store.Close()
	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatal("cloud endpoint did not require login")
	}
	s.SetRemoteAuthentication("")
	local := httptest.NewRecorder()
	s.Handler().ServeHTTP(local, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	if local.Code != http.StatusOK {
		t.Fatal("local endpoint now requires login")
	}
	register := authJSONRequest(t, s.Handler(), "/api/v1/auth/register/start", map[string]string{"email": "owner@example.com", "password": "a-long-test-password"}, nil)
	if register.Code != http.StatusBadRequest {
		t.Fatal("local-only runtime accepted cloud registration")
	}
}
func TestTurnstileBlocksMutationAndAppearsAfterLoginFailures(t *testing.T) {
	store, s, mailer := emailAuthTestServer(t, t.TempDir())
	defer store.Close()
	s.authHumanVerifier = authTestHuman{}
	s.authTurnstileSiteKey = "site"
	denied := authJSONRequest(t, s.Handler(), "/api/v1/auth/register/start", map[string]string{"email": "owner@example.com", "password": "a-long-test-password"}, nil)
	if denied.Code != http.StatusForbidden || mailer.count != 0 {
		t.Fatal("invalid human proof caused a state-changing send")
	}
	hash, err := hashPassword("a-long-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetInitialCredentials(context.Background(), s.workspaceID, "owner@example.com", "Owner", hash); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		bad := authJSONRequest(t, s.Handler(), "/api/v1/auth/login", map[string]string{"email": "owner@example.com", "password": "wrong"}, nil)
		if bad.Code != http.StatusUnauthorized {
			t.Fatalf("login attempt %d=%d", i, bad.Code)
		}
	}
	missing := authJSONRequest(t, s.Handler(), "/api/v1/auth/login", map[string]string{"email": "owner@example.com", "password": "a-long-test-password"}, nil)
	if missing.Code != http.StatusForbidden {
		t.Fatal("failed logins did not trigger human verification")
	}
	good := authJSONRequest(t, s.Handler(), "/api/v1/auth/login", map[string]string{"email": "owner@example.com", "password": "a-long-test-password", "challengeToken": "human-login"}, nil)
	if good.Code != http.StatusOK {
		t.Fatalf("verified login failed %d %s", good.Code, good.Body.String())
	}
}

func TestLegacyPasswordCredentialsLoginAfterHashUpgrade(t *testing.T) {
	store, s, _ := emailAuthTestServer(t, t.TempDir())
	defer store.Close()
	const legacyHash = "pbkdf2-sha256$310000$AAECAwQFBgcICQoLDA0ODw$1Ecwze8zai1SJcnoxFrFGeuO2EG+eikMayU+BC03mNg"
	if err := store.SetInitialCredentials(context.Background(), s.workspaceID, "owner@example.com", "Owner", legacyHash); err != nil {
		t.Fatal(err)
	}
	response := authJSONRequest(t, s.Handler(), "/api/v1/auth/login", map[string]string{"email": "owner@example.com", "password": "legacy-password-test"}, nil)
	if response.Code != http.StatusOK || len(response.Result().Cookies()) != 1 {
		t.Fatalf("legacy account lost login capability: %d", response.Code)
	}
	if _, err := store.AuthSessionUser(context.Background(), hashToken(response.Result().Cookies()[0].Value), time.Now()); err != nil {
		t.Fatal("legacy login session was not durable")
	}
	newHash, err := hashPassword("new-password-for-test")
	if err != nil || !strings.HasPrefix(newHash, "pbkdf2-sha256$600000$") || !verifyPassword("new-password-for-test", newHash) {
		t.Fatal("new password did not use the upgraded verifier")
	}
}
