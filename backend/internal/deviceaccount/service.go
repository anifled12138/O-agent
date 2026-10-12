// Package deviceaccount owns optional local sign-in and the remote worker lifecycle.
package deviceaccount

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/secure"
)

const settingKey = "device_account_v1"

type Settings interface {
	RuntimeSetting(context.Context, string) (string, error)
	SetRuntimeSetting(context.Context, string, string) error
}
type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}
type Binding struct {
	ServerURL  string `json:"serverUrl"`
	NodeID     string `json:"nodeId"`
	Credential string `json:"credential"`
	User       User   `json:"user"`
}

func (b Binding) OutboxKey() string {
	h := sha256.Sum256([]byte(b.ServerURL + ":" + b.NodeID))
	return hex.EncodeToString(h[:16])
}

type pending struct {
	DeviceCode      string    `json:"deviceCode"`
	VerificationURL string    `json:"verificationUrl"`
	ExpiresAt       time.Time `json:"expiresAt"`
}
type cancellation struct {
	ServerURL  string `json:"serverUrl"`
	DeviceCode string `json:"deviceCode"`
}
type durable struct {
	Cancellations []cancellation `json:"cancellations,omitempty"`
	ClientID      string         `json:"clientId"`
	ServerURL     string         `json:"serverUrl"`
	DeviceName    string         `json:"deviceName"`
	Binding       *Binding       `json:"binding,omitempty"`
	Pending       *pending       `json:"pending,omitempty"`
	Revocations   []Binding      `json:"revocations,omitempty"`
}
type envelope struct {
	Cipher []byte `json:"cipher"`
	Nonce  []byte `json:"nonce"`
}
type Status struct {
	State              string     `json:"state"`
	ServerURL          string     `json:"serverUrl"`
	DeviceName         string     `json:"deviceName"`
	User               *User      `json:"user,omitempty"`
	NodeID             string     `json:"nodeId,omitempty"`
	VerificationURL    string     `json:"verificationUrl,omitempty"`
	ExpiresAt          *time.Time `json:"expiresAt,omitempty"`
	LastConnected      *time.Time `json:"lastConnected,omitempty"`
	Error              string     `json:"error,omitempty"`
	PendingRevocations int        `json:"pendingRevocations"`
}

// Runner must stop all remote work before returning on context cancellation.
type Runner func(context.Context, Binding, func()) error
type Service struct {
	mu            sync.Mutex
	ops           sync.Mutex
	store         Settings
	vault         *secure.Vault
	data          durable
	state, reason string
	lastConnected *time.Time
	client        *http.Client
	runner        Runner
	rejected      func(error) bool
	wake          chan struct{}
	done          chan struct{}
	root          context.Context
	workerCancel  context.CancelFunc
	workerDone    chan struct{}
	closed        bool
	revokeNext    time.Time
	nextAttempt   time.Time
	retryDelay    time.Duration
}

func New(ctx context.Context, store Settings, vault *secure.Vault, runner Runner, rejected func(error) bool) (*Service, error) {
	s := &Service{store: store, vault: vault, runner: runner, rejected: rejected, state: "signed_out", wake: make(chan struct{}, 1), done: make(chan struct{}), root: ctx, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("account server redirects are not allowed")
	}}}
	if store == nil || vault == nil || runner == nil {
		return nil, domain.ErrInvalid
	}
	raw, err := store.RuntimeSetting(ctx, settingKey)
	if err == nil {
		var e envelope
		if err = json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, err
		}
		plain, err := vault.Open(e.Cipher, e.Nonce)
		if err != nil {
			return nil, fmt.Errorf("decrypt local account: %w", err)
		}
		defer clear(plain)
		if err = json.Unmarshal(plain, &s.data); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	} else {
		b := make([]byte, 32)
		if _, err = rand.Read(b); err != nil {
			return nil, err
		}
		name, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		s.data = durable{ClientID: hex.EncodeToString(b), DeviceName: name}
		if err = s.persist(ctx, s.data); err != nil {
			return nil, err
		}
	}
	if s.data.Binding != nil {
		s.state = "connecting"
	} else if s.data.Pending != nil {
		s.state = "awaiting_login"
	}
	go s.run()
	return s, nil
}

// persist seals secrets and requires durable read-back before the runtime consumes them.
func (s *Service) persist(ctx context.Context, d durable) error {
	plain, err := json.Marshal(d)
	if err != nil {
		return err
	}
	defer clear(plain)
	cipher, nonce, err := s.vault.Seal(plain)
	if err != nil {
		return err
	}
	body, err := json.Marshal(envelope{cipher, nonce})
	if err != nil {
		return err
	}
	if err = s.store.SetRuntimeSetting(ctx, settingKey, string(body)); err != nil {
		return err
	}
	got, err := s.store.RuntimeSetting(ctx, settingKey)
	if err != nil {
		return err
	}
	if got != string(body) {
		return errors.New("local account did not persist")
	}
	return nil
}
func (s *Service) Status() Status { s.mu.Lock(); defer s.mu.Unlock(); return s.statusLocked() }
func (s *Service) statusLocked() Status {
	r := Status{State: s.state, ServerURL: s.data.ServerURL, DeviceName: s.data.DeviceName, Error: s.reason, LastConnected: s.lastConnected, PendingRevocations: len(s.data.Revocations) + len(s.data.Cancellations)}
	if r.State == "connected" && (s.lastConnected == nil || time.Since(*s.lastConnected) > 90*time.Second) {
		r.State = "offline"
		r.Error = "暂未收到服务器心跳确认，正在恢复连接"
	}
	if b := s.data.Binding; b != nil {
		u := b.User
		r.User = &u
		r.NodeID = b.NodeID
	}
	if p := s.data.Pending; p != nil {
		t := p.ExpiresAt
		r.ExpiresAt = &t
		r.VerificationURL = p.VerificationURL
	}
	return r
}
func NormalizeServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return "", errors.New("请输入服务器地址，例如 https://anifled.icu")
	}
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", errors.New("公网账户登录需要 HTTPS")
	}
	u.Path = ""
	return u.String(), nil
}
func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) Begin(ctx context.Context, server, name, platform string) (Status, error) {
	s.ops.Lock()
	defer s.ops.Unlock()
	base, err := NormalizeServer(server)
	if err != nil {
		return Status{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return Status{}, domain.ErrInvalid
	}
	s.mu.Lock()
	d := s.data
	blocked := s.closed || d.Binding != nil
	if !blocked && d.Pending != nil {
		if d.ServerURL == base && d.DeviceName == name && d.Pending.ExpiresAt.After(time.Now()) {
			state := s.statusLocked()
			s.mu.Unlock()
			return state, nil
		}
		s.mu.Unlock()
		return Status{}, errors.New("请先取消当前登录，再发起新的登录")
	}
	for _, b := range d.Revocations {
		if b.ServerURL == base {
			blocked = true
		}
	}
	for _, p := range d.Cancellations {
		if p.ServerURL == base {
			blocked = true
		}
	}
	s.mu.Unlock()
	if blocked {
		return Status{}, errors.New("请先退出当前账户，等待服务器确认旧凭据已注销后再登录")
	}
	var p pending
	code, err := s.call(ctx, base, "/auth/device/start", "", map[string]string{"clientId": d.ClientID, "name": name, "platform": platform}, &p)
	if err != nil {
		return Status{}, err
	}
	if code != 201 || len(p.DeviceCode) < 32 || !p.ExpiresAt.After(time.Now()) {
		return Status{}, errors.New("服务器未返回有效的设备登录，请更新云端版本")
	}
	u, err := url.Parse(p.VerificationURL)
	if err != nil || u.Scheme+"://"+u.Host != base || u.User != nil {
		return Status{}, errors.New("服务器返回了不匹配的登录页面")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.data.Binding != nil || s.data.Pending != d.Pending {
		return Status{}, domain.ErrConflict
	}
	d = s.data
	d.ServerURL, d.DeviceName, d.Pending = base, name, &p
	if err = s.persist(ctx, d); err != nil {
		return Status{}, err
	}
	s.data = d
	s.state = "awaiting_login"
	s.reason = ""
	s.notify()
	return s.statusLocked(), nil
}
func (s *Service) Logout(ctx context.Context) (Status, error) {
	s.ops.Lock()
	defer s.ops.Unlock()
	s.mu.Lock()
	d := s.data
	d.Revocations = append([]Binding(nil), d.Revocations...)
	if d.Binding != nil {
		d.Revocations = append(d.Revocations, *d.Binding)
	}
	if d.Pending != nil {
		d.Cancellations = append(append([]cancellation(nil), d.Cancellations...), cancellation{d.ServerURL, d.Pending.DeviceCode})
	}
	d.Binding = nil
	d.Pending = nil
	if err := s.persist(ctx, d); err != nil {
		s.mu.Unlock()
		return Status{}, err
	}
	s.data = d
	s.state = "signed_out"
	s.reason = ""
	s.lastConnected = nil
	s.retryDelay = 0
	s.nextAttempt = time.Time{}
	cancel, done := s.workerCancel, s.workerDone
	if done != nil {
		s.state = "disconnecting"
	}
	if cancel != nil {
		cancel()
	}
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return s.Status(), errors.New("账户已退出，远程任务仍在停止，请稍后查看状态")
		}
	}
	s.notify()
	return s.Status(), nil
}
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	if s.workerCancel != nil {
		s.workerCancel()
	}
	s.mu.Unlock()
	s.notify()
	<-s.done
}
func (s *Service) run() {
	defer close(s.done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed || s.root.Err() != nil {
			s.mu.Lock()
			cancel, done := s.workerCancel, s.workerDone
			if cancel != nil {
				cancel()
			}
			s.mu.Unlock()
			if done != nil {
				<-done
			}
			return
		}
		s.revokePending()
		s.cancelPending()
		s.poll()
		s.ensureWorker()
		select {
		case <-s.root.Done():
		case <-s.wake:
		case <-ticker.C:
		}
	}
}
func (s *Service) poll() {
	s.mu.Lock()
	d := s.data
	expired := s.state == "login_expired"
	s.mu.Unlock()
	if d.Pending == nil || expired {
		return
	}
	p := d.Pending
	if !p.ExpiresAt.After(time.Now()) {
		s.mu.Lock()
		if s.data.Pending == p {
			s.state = "login_expired"
			s.reason = "登录已过期，请重新登录"
		}
		s.mu.Unlock()
		return
	}
	var reply struct {
		Status     string `json:"status"`
		NodeID     string `json:"nodeId"`
		Credential string `json:"credential"`
		User       User   `json:"user"`
	}
	code, err := s.call(s.root, d.ServerURL, "/auth/device/poll", "", map[string]string{"deviceCode": p.DeviceCode}, &reply)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Pending != p {
		return
	}
	if code == http.StatusNotFound || code == http.StatusGone || code == http.StatusUnauthorized {
		s.state = "login_expired"
		s.reason = "登录请求已失效，请取消后重新登录"
		return
	}
	if err != nil {
		s.reason = err.Error()
		return
	}
	if code == 202 {
		s.reason = ""
		return
	}
	if code != 200 || reply.Status != "authorized" || reply.NodeID == "" || reply.User.ID == "" || len(reply.Credential) < 32 {
		s.reason = "服务器没有返回有效的设备身份"
		return
	}
	d = s.data
	d.Binding = &Binding{ServerURL: d.ServerURL, NodeID: reply.NodeID, Credential: reply.Credential, User: reply.User}
	d.Pending = nil
	if err = s.persist(s.root, d); err != nil {
		s.reason = "设备身份无法保存：" + err.Error()
		return
	}
	s.data = d
	s.state = "connecting"
	s.reason = ""
}
func (s *Service) ensureWorker() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.data.Binding == nil || s.state == "login_required" || s.workerDone != nil || time.Now().Before(s.nextAttempt) {
		return
	}
	b := s.data.Binding
	ctx, cancel := context.WithCancel(s.root)
	done := make(chan struct{})
	s.workerCancel, s.workerDone = cancel, done
	go func() {
		defer close(done)
		err := s.runner(ctx, *b, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.data.Binding == b && ctx.Err() == nil {
				now := time.Now().UTC()
				s.lastConnected = &now
				s.state = "connected"
				s.retryDelay = 0
				s.nextAttempt = time.Time{}
				s.reason = ""
			}
		})
		s.mu.Lock()
		defer s.mu.Unlock()
		s.workerCancel = nil
		s.workerDone = nil
		if s.data.Binding != b {
			if s.data.Binding == nil && s.state == "disconnecting" {
				s.state = "signed_out"
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.state = "offline"
		if s.retryDelay == 0 {
			s.retryDelay = 2 * time.Second
		} else {
			s.retryDelay *= 2
			if s.retryDelay > 30*time.Second {
				s.retryDelay = 30 * time.Second
			}
		}
		s.nextAttempt = time.Now().Add(s.retryDelay)
		s.reason = "连接中断，正在重试"
		if err != nil && s.rejected != nil && s.rejected(err) {
			s.state = "login_required"
			s.reason = "设备凭据已失效，请退出后重新登录"
		}
	}()
}
func (s *Service) revokePending() {
	s.mu.Lock()
	if time.Now().Before(s.revokeNext) || len(s.data.Revocations) == 0 {
		s.mu.Unlock()
		return
	}
	b := s.data.Revocations[0]
	s.mu.Unlock()
	var result struct {
		Status string `json:"status"`
	}
	code, err := s.call(s.root, b.ServerURL, "/nodes/self/revoke", b.Credential, struct{}{}, &result)
	if err != nil || code != 200 || result.Status != "revoked" {
		s.mu.Lock()
		s.revokeNext = time.Now().Add(10 * time.Second)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Revocations) == 0 || s.data.Revocations[0].Credential != b.Credential {
		return
	}
	d := s.data
	d.Revocations = append([]Binding(nil), d.Revocations[1:]...)
	if err = s.persist(s.root, d); err != nil {
		s.reason = "撤销凭据的状态未能保存"
		return
	}
	s.data = d
}
func (s *Service) call(ctx context.Context, base, path, credential string, input, out any) (int, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/api/v1"+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return 0, errors.New("无法连接账户服务器，请检查地址和网络")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return res.StatusCode, fmt.Errorf("账户服务器返回 %d，请检查服务器版本、配置或重新登录", res.StatusCode)
	}
	body, err = io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return res.StatusCode, errors.New("读取账户服务器响应失败")
	}
	if err = json.Unmarshal(body, out); err != nil {
		return res.StatusCode, errors.New("账户服务器返回无效响应，请检查地址")
	}
	return res.StatusCode, nil
}

func (s *Service) cancelPending() {
	s.mu.Lock()
	if time.Now().Before(s.revokeNext) || len(s.data.Cancellations) == 0 {
		s.mu.Unlock()
		return
	}
	p := s.data.Cancellations[0]
	s.mu.Unlock()
	var reply struct {
		Status string `json:"status"`
	}
	code, err := s.call(s.root, p.ServerURL, "/auth/device/cancel", "", map[string]string{"deviceCode": p.DeviceCode}, &reply)
	if err != nil || code != 200 || reply.Status != "cancelled" {
		s.mu.Lock()
		s.revokeNext = time.Now().Add(10 * time.Second)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Cancellations) == 0 || s.data.Cancellations[0] != p {
		return
	}
	d := s.data
	d.Cancellations = append([]cancellation(nil), d.Cancellations[1:]...)
	if err = s.persist(s.root, d); err != nil {
		s.reason = "取消登录的状态未能保存"
		return
	}
	s.data = d
}
