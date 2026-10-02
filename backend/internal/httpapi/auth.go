package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
)

const authCookieName = "o_session"
const authSessionLifetime = 30 * 24 * time.Hour

func (s *Server) SetRemoteAuthentication(bootstrapToken string) {
	s.authBootstrapToken = bootstrapToken
}

func (s *Server) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/v1/auth/setup", s.authSetup)
	mux.HandleFunc("POST /api/v1/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.authLogout)
	mux.HandleFunc("GET /api/v1/auth/session", s.authSession)
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	_, hash, err := s.store.AuthUserByID(r.Context(), s.workspaceID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		fail(w, err)
		return
	}
	needsSetup := errors.Is(err, domain.ErrNotFound) || !strings.HasPrefix(hash, "pbkdf2-sha256$")
	write(w, http.StatusOK, map[string]bool{"authenticationRequired": s.authBootstrapToken != "", "setupRequired": needsSetup})
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBootstrap(r) {
		write(w, http.StatusUnauthorized, map[string]string{"error": "invalid bootstrap credential"})
		return
	}
	var input struct {
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	address, err := mail.ParseAddress(strings.TrimSpace(input.Email))
	if err != nil || address.Address != strings.TrimSpace(input.Email) || len(address.Address) > 254 || len(input.Password) < 12 || len(input.Password) > 1024 || len(strings.TrimSpace(input.DisplayName)) > 200 {
		write(w, http.StatusBadRequest, map[string]string{"error": "provide a valid email and a password of at least 12 characters"})
		return
	}
	display := strings.TrimSpace(input.DisplayName)
	if display == "" {
		display = address.Address
	}
	hash, err := hashPassword(input.Password)
	if err != nil {
		fail(w, err)
		return
	}
	// The first local owner is the only account this single-user runtime accepts.
	user, currentHash, err := s.store.AuthUserByID(r.Context(), s.workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	if strings.HasPrefix(currentHash, "pbkdf2-sha256$") {
		write(w, http.StatusConflict, map[string]string{"error": "initial account setup has already been completed"})
		return
	}
	if err := s.store.SetInitialCredentials(r.Context(), user.ID, strings.ToLower(address.Address), display, hash); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			write(w, http.StatusConflict, map[string]string{"error": "initial account setup has already been completed or email is already used"})
			return
		}
		fail(w, err)
		return
	}
	readBack, persistedHash, err := s.store.AuthUserByEmail(r.Context(), strings.ToLower(address.Address))
	if err != nil || persistedHash != hash || readBack.ID != user.ID {
		write(w, http.StatusInternalServerError, map[string]string{"error": "account setup could not be verified"})
		return
	}
	if err := s.issueSession(w, r, user.ID); err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"authenticated": true, "user": map[string]string{"id": readBack.ID, "email": readBack.Email, "displayName": readBack.DisplayName}})
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	loginNow := time.Now().UTC()
	bucketHash := hashToken("login-ip:" + loginRemoteAddress(r))
	allowed, retryAfter, err := s.store.AuthLoginAllowed(r.Context(), bucketHash, loginNow)
	if err != nil {
		fail(w, err)
		return
	}
	if !allowed {
		writeLoginRateLimit(w, retryAfter)
		return
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	user, stored, err := s.store.AuthUserByEmail(r.Context(), strings.ToLower(strings.TrimSpace(input.Email)))
	valid := false
	if err == nil && stored != "" && len(input.Password) <= 1024 {
		valid = verifyPassword(input.Password, stored)
	}
	if !valid || user.ID != s.workspaceID {
		blockedFor, recordErr := s.store.RecordAuthLoginFailure(r.Context(), bucketHash, time.Now().UTC())
		if recordErr != nil {
			fail(w, recordErr)
			return
		}
		if blockedFor > 0 {
			writeLoginRateLimit(w, blockedFor)
			return
		}
		write(w, http.StatusUnauthorized, map[string]string{"error": "email or password is incorrect"})
		return
	}
	if err := s.store.ClearAuthLoginFailures(r.Context(), bucketHash); err != nil {
		fail(w, err)
		return
	}
	if err := s.issueSession(w, r, user.ID); err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"authenticated": true, "user": map[string]string{"id": user.ID, "email": user.Email, "displayName": user.DisplayName}})
}

func loginRemoteAddress(r *http.Request) string {
	address := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(address); err == nil {
		address = host
	}
	if address == "" {
		return "unknown"
	}
	return address
}

func writeLoginRateLimit(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	write(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed login attempts; try again after the stated delay"})
}

func (s *Server) authSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(authCookieName)
	if err != nil {
		write(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	userID, err := s.store.AuthSessionUser(r.Context(), hashToken(cookie.Value), time.Now().UTC())
	if err != nil || userID != s.workspaceID {
		write(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	user, _, err := s.store.AuthUserByID(r.Context(), userID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"authenticated": true, "user": map[string]string{"id": user.ID, "email": user.Email, "displayName": user.DisplayName}})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(authCookieName)
	if err == nil {
		if err := s.store.DeleteAuthSession(r.Context(), hashToken(cookie.Value)); err != nil {
			fail(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: "", Path: "/", HttpOnly: true, Secure: requestIsSecure(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	write(w, http.StatusOK, map[string]bool{"authenticated": false})
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID string) error {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	expires := time.Now().UTC().Add(authSessionLifetime)
	if err := s.store.CreateAuthSession(r.Context(), hashToken(token), userID, expires); err != nil {
		return err
	}
	if persisted, err := s.store.AuthSessionUser(r.Context(), hashToken(token), time.Now().UTC()); err != nil || persisted != userID {
		deleteErr := s.store.DeleteAuthSession(r.Context(), hashToken(token))
		if err != nil {
			return errors.Join(err, deleteErr)
		}
		return errors.Join(errors.New("new authentication session did not read back"), deleteErr)
	}
	http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: token, Path: "/", Expires: expires, MaxAge: int(authSessionLifetime.Seconds()), HttpOnly: true, Secure: requestIsSecure(r), SameSite: http.SameSiteStrictMode})
	return nil
}

func requestIsSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) authorizeBootstrap(r *http.Request) bool {
	provided := r.Header.Get("X-O-Bootstrap-Token")
	if s.authBootstrapToken == "" || len(provided) != len(s.authBootstrapToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.authBootstrapToken)) == 1
}

type authUserContextKey struct{}

func (s *Server) requireAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" || strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		// Public artifact downloads are bearer-authorized by a persisted,
		// expiring share token; every operation that creates or revokes one
		// remains behind the ordinary owner session check below.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/shared/artifacts/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/v1/nodes/heartbeat" || r.URL.Path == "/api/v1/nodes/connect" || strings.HasPrefix(r.URL.Path, "/api/v1/nodes/tasks/") {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if provided == "" || provided == r.Header.Get("Authorization") {
				write(w, http.StatusUnauthorized, map[string]string{"error": "node credential required"})
				return
			}
			node, err := s.store.ExecutionNodeForToken(r.Context(), hashToken(provided))
			if err != nil {
				write(w, http.StatusUnauthorized, map[string]string{"error": "node credential is invalid or revoked"})
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), executionNodeContextKey{}, node.ID)))
			return
		}
		if s.authBootstrapToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(authCookieName)
		if err != nil {
			write(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		userID, err := s.store.AuthSessionUser(r.Context(), hashToken(cookie.Value), time.Now().UTC())
		if err != nil || userID != s.workspaceID {
			write(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authUserContextKey{}, userID)))
	})
}

func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const iterations = 310000
	key := derivePasswordKey(password, salt, iterations)
	return "pbkdf2-sha256$310000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || parts[1] != "310000" {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) != 32 {
		return false
	}
	got := derivePasswordKey(password, salt, 310000)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func derivePasswordKey(password string, salt []byte, iterations int) []byte {
	mac := hmac.New(sha256.New, []byte(password))
	_, _ = mac.Write(salt)
	_, _ = mac.Write([]byte{0, 0, 0, 1})
	u := mac.Sum(nil)
	derived := append([]byte(nil), u...)
	for i := 1; i < iterations; i++ {
		mac.Reset()
		_, _ = mac.Write(u)
		u = mac.Sum(u[:0])
		for j := range derived {
			derived[j] ^= u[j]
		}
	}
	return derived
}
