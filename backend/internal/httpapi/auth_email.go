package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
)

const authMinimumPasswordLength = 15
const authCodeLifetime = 10 * time.Minute

func normalizeAuthEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	address, err := mail.ParseAddress(raw)
	if err != nil || address.Address != raw || len(raw) > 254 {
		return "", domain.ErrInvalid
	}
	return strings.ToLower(raw), nil
}
func validAuthPassword(password string) bool {
	return utf8.RuneCountInString(password) >= authMinimumPasswordLength && len(password) <= 1024
}

func newAuthID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
func (s *Server) authChallengeStart(w http.ResponseWriter, r *http.Request) {
	if s.authBootstrapToken == "" {
		write(w, http.StatusBadRequest, map[string]string{"error": "本地模式不需要注册或登录"})
		return
	}
	if s.authMailer == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "管理员尚未配置邮件发送服务"})
		return
	}
	var input struct {
		Email          string `json:"email"`
		Password       string `json:"password"`
		DisplayName    string `json:"displayName"`
		ChallengeToken string `json:"challengeToken"`
	}
	if !decode(w, r, &input) {
		return
	}
	purpose := "register"
	if strings.Contains(r.URL.Path, "password-reset") {
		purpose = "reset"
	}
	email, err := normalizeAuthEmail(input.Email)
	if err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "请输入有效邮箱"})
		return
	}
	if purpose == "register" && (!validAuthPassword(input.Password) || len(strings.TrimSpace(input.DisplayName)) > 200) {
		write(w, http.StatusBadRequest, map[string]string{"error": "密码至少 15 个字符、最多 1024 个 UTF-8 字节，名称不得超过 200 个字符"})
		return
	}

	// Rate limit both network source and recipient, including unknown recipients.
	for _, bucket := range []string{hashToken("auth-mail-ip:" + loginRemoteAddress(r)), hashToken("auth-mail-email:" + email)} {
		allowed, retry, err := s.store.AuthLoginAllowed(r.Context(), bucket, time.Now().UTC())
		if err != nil {
			fail(w, err)
			return
		}
		if !allowed {
			writeLoginRateLimit(w, retry)
			return
		}
		blocked, err := s.store.RecordAuthLoginFailure(r.Context(), bucket, time.Now().UTC())
		if err != nil {
			fail(w, err)
			return
		}
		if blocked > 0 {
			writeLoginRateLimit(w, blocked)
			return
		}
	}
	if s.authHumanVerifier != nil {
		if err := s.authHumanVerifier.Verify(r.Context(), input.ChallengeToken, purpose); err != nil {
			write(w, http.StatusForbidden, map[string]any{"error": err.Error(), "challengeRequired": true})
			return
		}
	}
	id, err := newAuthID()
	if err != nil {
		fail(w, err)
		return
	}
	user, current, err := s.store.AuthUserByID(r.Context(), s.workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	eligible := false
	if purpose == "register" {
		eligible = s.authOwnerEmail != "" && email == s.authOwnerEmail && !strings.HasPrefix(current, "pbkdf2-sha256$")
	} else {
		eligible = email == user.Email && strings.HasPrefix(current, "pbkdf2-sha256$")
	}
	if eligible {
		n, err := rand.Int(rand.Reader, big.NewInt(100000000))
		if err != nil {
			fail(w, err)
			return
		}
		code := fmt.Sprintf("%08d", n.Int64())
		now := time.Now().UTC()
		c := storage.AuthEmailChallenge{ID: id, Purpose: purpose, UserID: user.ID, Email: email, DisplayName: strings.TrimSpace(input.DisplayName), CodeHash: hashToken(id + ":" + code), CreatedAt: now, ExpiresAt: now.Add(authCodeLifetime)}
		if c.DisplayName == "" {
			c.DisplayName = email
		}
		if purpose == "register" {
			c.PasswordHash, err = hashPassword(input.Password)
			if err != nil {
				fail(w, err)
				return
			}
		}
		if err = s.store.PrepareAuthEmailChallenge(r.Context(), c); err != nil {
			if errors.Is(err, domain.ErrBusy) {
				w.Header().Set("Retry-After", "60")
				write(w, http.StatusTooManyRequests, map[string]string{"error": "请等待一分钟后再申请验证码"})
				return
			}
			fail(w, err)
			return
		}
		providerID, sendErr := s.authMailer.SendVerification(r.Context(), email, code, purpose, id)
		// Record an uncertain delivery even after cancellation or timeout. Never
		// make an unconfirmed provider result eligible to change credentials.
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		persistErr := s.store.SetAuthEmailDelivery(persistCtx, id, providerID, sendErr == nil)
		cancel()
		if persistErr != nil {
			fail(w, persistErr)
			return
		}
		if sendErr != nil {
			write(w, http.StatusServiceUnavailable, map[string]string{"error": "邮件发送结果未确认，请稍后重新申请验证码"})
			return
		}
	}
	// This deliberately does not assert inbox delivery or reveal the owner email.
	write(w, http.StatusAccepted, map[string]string{"challengeId": id, "status": "verification_requested", "message": "如果该邮箱可以用于此操作，将收到有效期 10 分钟的验证码。请检查收件箱和垃圾邮件。"})
}

func (s *Server) authChallengeVerify(w http.ResponseWriter, r *http.Request) {
	if s.authBootstrapToken == "" {
		write(w, http.StatusBadRequest, map[string]string{"error": "本地模式不需要注册或登录"})
		return
	}
	var input struct {
		ChallengeID string `json:"challengeId"`
		Code        string `json:"code"`
		Password    string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	purpose := "register"
	if strings.Contains(r.URL.Path, "password-reset") {
		purpose = "reset"
	}
	if len(input.ChallengeID) != 48 || len(input.Code) != 8 || strings.Trim(input.Code, "0123456789") != "" {
		write(w, http.StatusBadRequest, map[string]string{"error": "请输入 8 位数字验证码"})
		return
	}
	hash := ""
	if purpose == "reset" {
		if !validAuthPassword(input.Password) {
			write(w, http.StatusBadRequest, map[string]string{"error": "密码至少 15 个字符、最多 1024 个 UTF-8 字节"})
			return
		}
		var err error
		hash, err = hashPassword(input.Password)
		if err != nil {
			fail(w, err)
			return
		}
	}
	challenge, err := s.store.AuthEmailChallenge(r.Context(), input.ChallengeID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			fail(w, domain.ErrUnauthorized)
		} else {
			fail(w, err)
		}
		return
	}
	if challenge.UserID != s.workspaceID || (purpose == "register" && challenge.Email != s.authOwnerEmail) {
		fail(w, domain.ErrUnauthorized)
		return
	}
	user, err := s.store.ConsumeAuthEmailChallenge(r.Context(), input.ChallengeID, purpose, hashToken(input.ChallengeID+":"+input.Code), hash, time.Now().UTC(), hashToken("login-ip:"+loginRemoteAddress(r)), hashToken("login-email:"+challenge.Email))
	if err != nil {
		if errors.Is(err, domain.ErrUnauthorized) {
			write(w, http.StatusUnauthorized, map[string]string{"error": "验证码错误、已过期或已使用，请重新申请"})
			return
		}
		fail(w, err)
		return
	}
	if user.ID != s.workspaceID {
		fail(w, domain.ErrUnauthorized)
		return
	}
	if purpose == "register" {
		if err = s.issueSession(w, r, user.ID, challenge.PasswordHash); err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, map[string]any{"authenticated": true, "user": map[string]string{"id": user.ID, "email": user.Email, "displayName": user.DisplayName}})
		return
	}
	clearAuthCookie(w, r)
	write(w, http.StatusOK, map[string]string{"status": "password_reset", "message": "密码已更新，原登录会话已撤销，请重新登录。"})
}

func clearAuthCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: "", Path: "/", HttpOnly: true, Secure: requestIsSecure(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
}
func (s *Server) authLogoutAll(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(authCookieName)
	if err != nil {
		fail(w, domain.ErrUnauthorized)
		return
	}
	user, err := s.store.AuthSessionUser(r.Context(), hashToken(cookie.Value), time.Now().UTC())
	if err != nil || user != s.workspaceID {
		fail(w, domain.ErrUnauthorized)
		return
	}
	if err = s.store.DeleteUserAuthSessions(r.Context(), user); err != nil {
		fail(w, err)
		return
	}
	clearAuthCookie(w, r)
	write(w, http.StatusOK, map[string]bool{"authenticated": false})
}
