package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/storage"
	"golang.org/x/crypto/argon2"
)

const sessionDuration = 30 * 24 * time.Hour

type Service struct{ store *storage.Store }

func New(store *storage.Store) *Service { return &Service{store: store} }

func (s *Service) Register(ctx context.Context, email, password, name string) (domain.User, string, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	name = strings.TrimSpace(name)
	if !strings.Contains(email, "@") || len(password) < 10 || name == "" {
		return domain.User{}, "", domain.ErrInvalid
	}
	hash, err := hashPassword(password)
	if err != nil {
		return domain.User{}, "", err
	}
	u := domain.User{ID: newID(), Email: email, DisplayName: name, CreatedAt: time.Now().UTC()}
	if err = s.store.CreateUser(ctx, u, hash); err != nil {
		return domain.User{}, "", err
	}
	token, err := s.newSession(ctx, u.ID)
	return u, token, err
}

func (s *Service) Login(ctx context.Context, email, password string) (domain.User, string, error) {
	u, hash, err := s.store.UserByEmail(ctx, strings.TrimSpace(email))
	if err != nil {
		return domain.User{}, "", domain.ErrUnauthorized
	}
	ok, err := verifyPassword(password, hash)
	if err != nil || !ok {
		return domain.User{}, "", domain.ErrUnauthorized
	}
	token, err := s.newSession(ctx, u.ID)
	return u, token, err
}

func (s *Service) User(ctx context.Context, token string) (domain.User, error) {
	if token == "" {
		return domain.User{}, domain.ErrUnauthorized
	}
	return s.store.UserBySession(ctx, tokenHash(token))
}
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.store.RevokeAuthSession(ctx, tokenHash(token))
}

func (s *Service) newSession(ctx context.Context, userID string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	return token, s.store.CreateAuthSession(ctx, tokenHash(token), userID, time.Now().UTC().Add(sessionDuration))
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func newID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("argon2id$v=19$m=65536,t=3,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func verifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false, errors.New("invalid password encoding")
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false, err
	}
	if _, err := strconv.Atoi(strings.TrimPrefix(parts[1], "v=")); err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, err
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}
