package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func (s *Server) SetDeviceAuthorizationVault(v *secure.Vault) { s.deviceVault = v }
func (s *Server) deviceAuthorizationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/device/start", s.deviceAuthorizationStart)
	mux.HandleFunc("POST /api/v1/auth/device/poll", s.deviceAuthorizationPoll)
	mux.HandleFunc("POST /api/v1/auth/device/cancel", s.deviceAuthorizationCancel)
	mux.HandleFunc("GET /api/v1/auth/device/approval", s.deviceAuthorizationInfo)
	mux.HandleFunc("POST /api/v1/auth/device/approve", s.deviceAuthorizationApprove)
	mux.HandleFunc("POST /api/v1/nodes/self/revoke", s.deviceSelfRevoke)
}
func randomDeviceToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func (s *Server) deviceAuthorizationEnabled() bool {
	return s.authBootstrapToken != "" && s.deviceVault != nil
}
func (s *Server) deviceAuthorizationStart(w http.ResponseWriter, r *http.Request) {
	if !s.deviceAuthorizationEnabled() {
		write(w, 503, map[string]string{"error": "此服务器尚未启用设备账户登录，请更新并配置云端服务"})
		return
	}
	bucket := hashToken("device-login:" + loginRemoteAddress(r))
	now := time.Now().UTC()
	allowed, wait, err := s.store.AuthLoginAllowed(r.Context(), bucket, now)
	if err != nil {
		fail(w, err)
		return
	}
	if !allowed {
		writeLoginRateLimit(w, wait)
		return
	}
	var input struct {
		ClientID string `json:"clientId"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.ClientID) != 64 || strings.TrimSpace(input.Name) == "" || len(input.Name) > 100 || !validNodePlatform(input.Platform) {
		fail(w, domain.ErrInvalid)
		return
	}
	if _, err = hex.DecodeString(input.ClientID); err != nil {
		fail(w, domain.ErrInvalid)
		return
	}
	if _, err = s.store.RecordAuthLoginFailure(r.Context(), bucket, now); err != nil {
		fail(w, err)
		return
	}
	deviceCode, err := randomDeviceToken()
	if err != nil {
		fail(w, err)
		return
	}
	userCode, err := randomDeviceToken()
	if err != nil {
		fail(w, err)
		return
	}
	a := storage.DeviceAuthorization{DeviceHash: hashToken(deviceCode), UserHash: hashToken(userCode), ClientID: input.ClientID, Name: strings.TrimSpace(input.Name), Platform: input.Platform, ExpiresAt: now.Add(10 * time.Minute)}
	if err = s.store.CreateDeviceAuthorization(r.Context(), a); err != nil {
		fail(w, err)
		return
	}
	readBack, err := s.store.DeviceAuthorization(r.Context(), a.DeviceHash, false)
	if err != nil || readBack.ClientID != a.ClientID {
		fail(w, errors.Join(err, domain.ErrConflict))
		return
	}
	write(w, 201, map[string]any{"deviceCode": deviceCode, "verificationUrl": s.frontendOrigin + "/?device_login=" + userCode, "expiresAt": readBack.ExpiresAt})
}
func (s *Server) deviceAuthorizationUser(r *http.Request) (domain.User, error) {
	cookie, err := r.Cookie(authCookieName)
	if err != nil {
		return domain.User{}, domain.ErrUnauthorized
	}
	id, err := s.store.AuthSessionUser(r.Context(), hashToken(cookie.Value), time.Now().UTC())
	if err != nil || id != s.workspaceID {
		return domain.User{}, domain.ErrUnauthorized
	}
	user, _, err := s.store.AuthUserByID(r.Context(), id)
	return user, err
}
func (s *Server) deviceAuthorizationInfo(w http.ResponseWriter, r *http.Request) {
	if !s.deviceAuthorizationEnabled() {
		fail(w, domain.ErrUnauthorized)
		return
	}
	if _, err := s.deviceAuthorizationUser(r); err != nil {
		fail(w, err)
		return
	}
	a, err := s.store.DeviceAuthorization(r.Context(), hashToken(r.URL.Query().Get("code")), true)
	if err != nil {
		fail(w, err)
		return
	}
	if !a.ExpiresAt.After(time.Now()) {
		fail(w, domain.ErrUnauthorized)
		return
	}
	if a.UserID != "" {
		if _, err := s.activeDeviceAuthorization(r, a); err != nil {
			fail(w, err)
			return
		}
	}
	write(w, 200, map[string]any{"name": a.Name, "platform": a.Platform, "expiresAt": a.ExpiresAt, "approved": a.UserID != ""})
}
func (s *Server) deviceAuthorizationApprove(w http.ResponseWriter, r *http.Request) {
	if !s.deviceAuthorizationEnabled() {
		fail(w, domain.ErrUnauthorized)
		return
	}
	if r.Header.Get("Origin") != s.frontendOrigin {
		fail(w, domain.ErrUnauthorized)
		return
	}
	user, err := s.deviceAuthorizationUser(r)
	if err != nil {
		fail(w, err)
		return
	}
	var input struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &input) {
		return
	}
	a, err := s.store.DeviceAuthorization(r.Context(), hashToken(input.Code), true)
	if err != nil {
		fail(w, err)
		return
	}
	credential, err := randomDeviceToken()
	if err != nil {
		fail(w, err)
		return
	}
	cipher, nonce, err := s.deviceVault.Seal([]byte(credential))
	if err != nil {
		fail(w, err)
		return
	}
	nodeDigest := sha256.Sum256([]byte(user.ID + ":" + a.ClientID))
	nodeID := "node_device_" + hex.EncodeToString(nodeDigest[:16])
	a, err = s.store.ApproveDeviceAuthorization(r.Context(), a.UserHash, user.ID, nodeID, hashToken(credential), cipher, nonce, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	node, err := s.activeDeviceAuthorization(r, a)
	if err != nil || node.RevokedAt != nil {
		fail(w, errors.Join(err, domain.ErrConflict))
		return
	}
	write(w, 200, map[string]any{"status": "approved", "node": nodeResponse(node, time.Now().UTC())})
}
func (s *Server) deviceAuthorizationPoll(w http.ResponseWriter, r *http.Request) {
	if !s.deviceAuthorizationEnabled() {
		fail(w, domain.ErrUnauthorized)
		return
	}
	var input struct {
		DeviceCode string `json:"deviceCode"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.DeviceCode) < 32 {
		fail(w, domain.ErrUnauthorized)
		return
	}
	a, err := s.store.DeviceAuthorization(r.Context(), hashToken(input.DeviceCode), false)
	if err != nil {
		fail(w, err)
		return
	}
	if !a.ExpiresAt.After(time.Now()) {
		write(w, 410, map[string]string{"error": "设备登录已过期，请重新登录"})
		return
	}
	if a.UserID == "" {
		write(w, 202, map[string]string{"status": "pending"})
		return
	}
	node, err := s.store.ExecutionNode(r.Context(), a.UserID, a.NodeID)
	if err != nil || node.RevokedAt != nil {
		fail(w, domain.ErrUnauthorized)
		return
	}
	credential, err := s.deviceVault.Open(a.Cipher, a.Nonce)
	if err != nil {
		fail(w, err)
		return
	}
	defer clear(credential)
	bound, err := s.store.ExecutionNodeForToken(r.Context(), hashToken(string(credential)))
	if err != nil || bound.ID != node.ID {
		fail(w, domain.ErrUnauthorized)
		return
	}
	user, _, err := s.store.AuthUserByID(r.Context(), a.UserID)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	write(w, 200, map[string]any{"status": "authorized", "nodeId": node.ID, "credential": string(credential), "user": map[string]string{"id": user.ID, "email": user.Email, "displayName": user.DisplayName}})
}
func (s *Server) deviceSelfRevoke(w http.ResponseWriter, r *http.Request) {
	nodeID, _ := r.Context().Value(executionNodeContextKey{}).(string)
	if nodeID == "" {
		fail(w, domain.ErrUnauthorized)
		return
	}
	if err := s.store.RevokeExecutionNodeForToken(r.Context(), s.workspaceID, nodeID, hashToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), time.Now().UTC()); err != nil {
		fail(w, err)
		return
	}
	node, err := s.store.ExecutionNode(r.Context(), s.workspaceID, nodeID)
	if err != nil || node.RevokedAt == nil {
		fail(w, errors.Join(err, domain.ErrConflict))
		return
	}
	write(w, 200, map[string]string{"status": "revoked"})
}

func (s *Server) deviceAuthorizationCancel(w http.ResponseWriter, r *http.Request) {
	if !s.deviceAuthorizationEnabled() {
		fail(w, domain.ErrUnauthorized)
		return
	}
	var input struct {
		DeviceCode string `json:"deviceCode"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.DeviceCode) < 32 {
		fail(w, domain.ErrUnauthorized)
		return
	}
	hash := hashToken(input.DeviceCode)
	deleted, err := s.store.DeletePendingDeviceAuthorization(r.Context(), hash)
	if err != nil {
		fail(w, err)
		return
	}
	if !deleted {
		a, err := s.store.DeviceAuthorization(r.Context(), hash, false)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			fail(w, err)
			return
		}
		if err == nil {
			if a.NodeID != "" {
				plain, err := s.deviceVault.Open(a.Cipher, a.Nonce)
				if err != nil {
					fail(w, err)
					return
				}
				tokenHash := hashToken(string(plain))
				clear(plain)
				if err = s.store.RevokeExecutionNodeForToken(r.Context(), a.UserID, a.NodeID, tokenHash, time.Now().UTC()); err != nil && !errors.Is(err, domain.ErrUnauthorized) {
					fail(w, err)
					return
				}
			}
			if err = s.store.DeleteDeviceAuthorization(r.Context(), hash); err != nil {
				fail(w, err)
				return
			}
		}
	}
	if _, err = s.store.DeviceAuthorization(r.Context(), hash, false); !errors.Is(err, domain.ErrNotFound) {
		fail(w, errors.Join(err, domain.ErrConflict))
		return
	}
	write(w, 200, map[string]string{"status": "cancelled"})
}

func (s *Server) activeDeviceAuthorization(r *http.Request, a storage.DeviceAuthorization) (storage.ExecutionNode, error) {
	plain, err := s.deviceVault.Open(a.Cipher, a.Nonce)
	if err != nil {
		return storage.ExecutionNode{}, err
	}
	defer clear(plain)
	node, err := s.store.ExecutionNodeForToken(r.Context(), hashToken(string(plain)))
	if err != nil {
		return node, err
	}
	if node.ID != a.NodeID || node.UserID != a.UserID {
		return node, domain.ErrUnauthorized
	}
	return node, nil
}
