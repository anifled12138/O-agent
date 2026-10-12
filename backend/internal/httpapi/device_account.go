package httpapi

import (
	"net/http"
	"runtime"

	"axiom.local/agent/internal/deviceaccount"
)

func (s *Server) SetDeviceAccount(service *deviceaccount.Service) { s.deviceAccount = service }
func (s *Server) SetDeviceAccountStartupError(err error)          { s.deviceAccountStartupError = err }
func (s *Server) deviceAccountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/account", func(w http.ResponseWriter, r *http.Request) {
		if s.deviceAccountStartupError != nil {
			write(w, 200, deviceaccount.Status{State: "unavailable", Error: "本地账户状态无法恢复，远程连接已停止。本地 Agent 可以继续使用；请先在云端撤销旧设备，再修复本地账户数据并重启。"})
			return
		}
		if s.deviceAccount == nil {
			write(w, 404, map[string]string{"error": "local account management is unavailable"})
			return
		}
		write(w, 200, s.deviceAccount.Status())
	})
	mux.HandleFunc("POST /api/v1/account/login", func(w http.ResponseWriter, r *http.Request) {
		if s.deviceAccount == nil {
			write(w, 404, map[string]string{"error": "local account management is unavailable"})
			return
		}
		var input struct {
			ServerURL  string `json:"serverUrl"`
			DeviceName string `json:"deviceName"`
		}
		if !decode(w, r, &input) {
			return
		}
		status, err := s.deviceAccount.Begin(r.Context(), input.ServerURL, input.DeviceName, runtime.GOOS)
		if err != nil {
			write(w, 400, map[string]string{"error": err.Error()})
			return
		}
		write(w, 202, status)
	})
	mux.HandleFunc("POST /api/v1/account/logout", func(w http.ResponseWriter, r *http.Request) {
		if s.deviceAccount == nil {
			write(w, 404, map[string]string{"error": "local account management is unavailable"})
			return
		}
		status, err := s.deviceAccount.Logout(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		write(w, 200, status)
	})
}
