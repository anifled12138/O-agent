package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type authRoundTrip func(*http.Request) (*http.Response, error)

func (f authRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func providerResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestEmailProviderRequiresDurableMatchingReceipt(t *testing.T) {
	for _, match := range []bool{true, false} {
		calls := 0
		client := &http.Client{Transport: authRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Fatal("missing provider credential")
			}
			if r.Method == http.MethodPost {
				if r.URL.String() != "https://api.resend.com/emails" || r.Header.Get("Idempotency-Key") != "o-auth/challenge" {
					t.Fatal("provider POST contract mismatch")
				}
				return providerResponse(200, `{"id":"email-id"}`), nil
			}
			if r.URL.String() != "https://api.resend.com/emails/email-id" {
				t.Fatal("receipt was not read from the authoritative endpoint")
			}
			to := "owner@example.com"
			if !match {
				to = "different@example.com"
			}
			return providerResponse(200, `{"id":"email-id","from":"O <auth@example.com>","to":["`+to+`"],"subject":"O · 验证邮箱"}`), nil
		})}
		mailer := resendVerificationMailer{client: client, key: "test-key", from: "O <auth@example.com>"}
		id, err := mailer.SendVerification(context.Background(), "owner@example.com", "12345678", "register", "challenge")
		if calls != 2 {
			t.Fatal("provider acknowledgement lacked readback")
		}
		if match && (err != nil || id != "email-id") {
			t.Fatalf("valid receipt rejected %v", err)
		}
		if !match && (err == nil || id != "") {
			t.Fatal("mismatched receipt declared accepted")
		}
	}
}
func TestTurnstileRequiresExpectedHostnameAndAction(t *testing.T) {
	for _, tc := range []struct {
		host, action string
		valid        bool
	}{{"o.example", "register", true}, {"attacker.example", "register", false}, {"o.example", "login", false}} {
		verifier := turnstileHumanVerifier{secret: "test-secret", hostname: "o.example", client: &http.Client{Transport: authRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://challenges.cloudflare.com/turnstile/v0/siteverify" {
				t.Fatal("wrong verification endpoint")
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("secret") != "test-secret" || r.Form.Get("response") != "test-token" {
				t.Fatal("server verification omitted proof")
			}
			return providerResponse(200, `{"success":true,"hostname":"`+tc.host+`","action":"`+tc.action+`"}`), nil
		})}}
		err := verifier.Verify(context.Background(), "test-token", "register")
		if (err == nil) != tc.valid {
			t.Fatalf("hostname/action validation failed %+v %v", tc, err)
		}
	}
}
func TestEmailAuthConfigurationFailurePreservesPreviousConfiguration(t *testing.T) {
	s := &Server{frontendOrigin: "https://o.example"}
	valid := EmailAuthConfig{OwnerEmail: "owner@example.com", ResendAPIKey: "test-key", MailFrom: "auth@example.com"}
	if err := s.SetEmailAuthentication(valid); err != nil {
		t.Fatal(err)
	}
	before := s.authMailer
	invalid := valid
	invalid.OwnerEmail = "different@example.com"
	invalid.TurnstileSecret = "missing-site-key"
	if err := s.SetEmailAuthentication(invalid); err == nil {
		t.Fatal("invalid paired settings accepted")
	}
	if s.authOwnerEmail != "owner@example.com" || s.authMailer != before {
		t.Fatal("failed settings mutation left partial state")
	}
	if err := s.SetEmailAuthentication(EmailAuthConfig{}); err != nil {
		t.Fatal(err)
	}
	if s.authOwnerEmail != "" || s.authMailer != nil || s.authHumanVerifier != nil || s.authTurnstileSiteKey != "" {
		t.Fatal("empty configuration retained stale runtime state")
	}
}
