package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

type VerificationMailer interface {
	SendVerification(context.Context, string, string, string, string) (string, error)
}
type HumanVerifier interface {
	Verify(context.Context, string, string) error
}
type EmailAuthConfig struct {
	OwnerEmail       string
	ResendAPIKey     string
	MailFrom         string
	TurnstileSiteKey string
	TurnstileSecret  string
}

func (s *Server) SetEmailAuthentication(c EmailAuthConfig) error {
	var ownerEmail string
	var mailer VerificationMailer
	var verifier HumanVerifier
	if c.OwnerEmail != "" {
		email, err := normalizeAuthEmail(c.OwnerEmail)
		if err != nil {
			return fmt.Errorf("invalid O_AUTH_OWNER_EMAIL")
		}
		ownerEmail = email
	}
	if (c.ResendAPIKey == "") != (c.MailFrom == "") {
		return errors.New("O_RESEND_API_KEY and O_AUTH_MAIL_FROM must be configured together")
	}
	if (c.TurnstileSiteKey == "") != (c.TurnstileSecret == "") {
		return errors.New("O_TURNSTILE_SITE_KEY and O_TURNSTILE_SECRET_KEY must be configured together")
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if c.ResendAPIKey != "" {
		if _, err := mail.ParseAddress(c.MailFrom); err != nil {
			return errors.New("O_AUTH_MAIL_FROM must be a valid sender address")
		}
		mailer = &resendVerificationMailer{client: client, key: c.ResendAPIKey, from: c.MailFrom}
	}
	if c.TurnstileSecret != "" {
		origin, err := url.Parse(s.frontendOrigin)
		if err != nil || origin.Hostname() == "" {
			return errors.New("a frontend origin is required for Turnstile")
		}
		verifier = &turnstileHumanVerifier{client: client, secret: c.TurnstileSecret, hostname: origin.Hostname()}

	}
	s.authOwnerEmail = ownerEmail
	s.authMailer = mailer
	s.authHumanVerifier = verifier
	s.authTurnstileSiteKey = c.TurnstileSiteKey
	return nil
}

type resendVerificationMailer struct {
	client    *http.Client
	key, from string
}

func (m *resendVerificationMailer) SendVerification(ctx context.Context, email, code, purpose, id string) (string, error) {
	subject := "O · 验证邮箱"
	if purpose == "reset" {
		subject = "O · 重置密码"
	}
	payload, err := json.Marshal(map[string]any{"from": m.from, "to": []string{email}, "subject": subject, "text": "你的 O 验证码是：" + code + "\n验证码 10 分钟内有效，只能使用一次。如果不是你本人操作，请忽略此邮件。"})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "o-auth/"+id)
	resp, err := m.client.Do(req)
	if err != nil {
		return "", errors.New("email provider request failed; delivery is unconfirmed")
	}
	defer resp.Body.Close()
	var sent struct {
		ID string `json:"id"`
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("email provider did not accept the request")
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&sent); err != nil || sent.ID == "" {
		return "", errors.New("email provider receipt was invalid")
	}
	// Read back the provider's durable email record, rather than equating its
	// POST response with delivery to the recipient's mailbox.
	read, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.resend.com/emails/"+url.PathEscape(sent.ID), nil)
	if err != nil {
		return "", err
	}
	read.Header.Set("Authorization", "Bearer "+m.key)
	persisted, err := m.client.Do(read)
	if err != nil {
		return "", errors.New("email provider receipt could not be read back")
	}
	defer persisted.Body.Close()
	var record struct {
		ID      string   `json:"id"`
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
	}
	if persisted.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(persisted.Body, 65536)).Decode(&record) != nil || record.ID != sent.ID || len(record.To) != 1 || record.To[0] != email || record.Subject != subject {
		return "", errors.New("email provider receipt did not match the verification request")
	}
	expectedFrom, err := mail.ParseAddress(m.from)
	if err != nil {
		return "", errors.New("invalid configured email sender")
	}
	actualFrom, err := mail.ParseAddress(record.From)
	if err != nil || actualFrom.Address != expectedFrom.Address {
		return "", errors.New("email provider sender did not match")
	}
	return sent.ID, nil
}

type turnstileHumanVerifier struct {
	client           *http.Client
	secret, hostname string
}

func (v *turnstileHumanVerifier) Verify(ctx context.Context, token, action string) error {
	if token == "" || len(token) > 2048 {
		return errors.New("请完成人机验证")
	}
	form := url.Values{"secret": {v.secret}, "response": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.client.Do(req)
	if err != nil {
		return errors.New("人机验证服务暂时不可用，请重试")
	}
	defer resp.Body.Close()
	var result struct {
		Success  bool   `json:"success"`
		Hostname string `json:"hostname"`
		Action   string `json:"action"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&result) != nil {
		return errors.New("人机验证服务返回了无效结果")
	}
	if !result.Success || !strings.EqualFold(result.Hostname, v.hostname) || result.Action != action {
		return errors.New("人机验证失效，请重新验证")
	}
	return nil
}
