package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/auth"
	"axiom.local/agent/internal/bootstrap"
	"axiom.local/agent/internal/core"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evalharness"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/storage"
)

const cookieName = "axiom_session"

type contextKey string

const userKey contextKey = "user"

type Server struct {
	auth           *auth.Service
	providers      *provider.Service
	agent          *agent.Service
	evolution      *evolution.Service
	evals          *evalharness.Service
	bootstrap      *bootstrap.Service
	forge          *pluginforge.Service
	store          *storage.Store
	plugins        *core.Manager
	frontendOrigin string
	secureCookies  bool
}

func New(authService *auth.Service, providerService *provider.Service, agentService *agent.Service, evolutionService *evolution.Service, evalService *evalharness.Service, bootstrapService *bootstrap.Service, forgeService *pluginforge.Service, store *storage.Store, plugins *core.Manager, origin string) *Server {
	return &Server{auth: authService, providers: providerService, agent: agentService, evolution: evolutionService, evals: evalService, bootstrap: bootstrapService, forge: forgeService, store: store, plugins: plugins, frontendOrigin: strings.TrimRight(origin, "/"), secureCookies: strings.HasPrefix(origin, "https://")}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("POST /api/v1/auth/register", s.register)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.Handle("POST /api/v1/auth/logout", s.requireUser(http.HandlerFunc(s.logout)))
	mux.Handle("GET /api/v1/auth/me", s.requireUser(http.HandlerFunc(s.me)))
	mux.Handle("GET /api/v1/system/plugins", s.requireUser(http.HandlerFunc(s.pluginList)))
	mux.Handle("GET /api/v1/providers", s.requireUser(http.HandlerFunc(s.providerList)))
	mux.Handle("POST /api/v1/providers", s.requireUser(http.HandlerFunc(s.providerCreate)))
	mux.Handle("PUT /api/v1/providers/{id}", s.requireUser(http.HandlerFunc(s.providerUpdate)))
	mux.Handle("POST /api/v1/providers/{id}/test", s.requireUser(http.HandlerFunc(s.providerTest)))
	mux.Handle("GET /api/v1/conversations", s.requireUser(http.HandlerFunc(s.conversationList)))
	mux.Handle("POST /api/v1/conversations", s.requireUser(http.HandlerFunc(s.conversationCreate)))
	mux.Handle("GET /api/v1/conversations/{id}", s.requireUser(http.HandlerFunc(s.conversationGet)))
	mux.Handle("GET /api/v1/conversations/{id}/trace", s.requireUser(http.HandlerFunc(s.conversationTrace)))
	mux.Handle("POST /api/v1/conversations/{id}/messages", s.requireUser(http.HandlerFunc(s.messageCreate)))
	mux.Handle("GET /api/v1/evolution/generations", s.requireUser(http.HandlerFunc(s.generationList)))
	mux.Handle("POST /api/v1/evolution/generations/candidates", s.requireUser(http.HandlerFunc(s.generationCreateCandidate)))
	mux.Handle("POST /api/v1/evolution/generations/{id}/promote", s.requireUser(http.HandlerFunc(s.generationPromote)))
	mux.Handle("GET /api/v1/evolution/challenges", s.requireUser(http.HandlerFunc(s.challengeList)))
	mux.Handle("POST /api/v1/evolution/challenges", s.requireUser(http.HandlerFunc(s.challengeCreate)))
	mux.Handle("POST /api/v1/evolution/challenges/{id}/bootstrap", s.requireUser(http.HandlerFunc(s.challengeBootstrap)))
	mux.Handle("GET /api/v1/evolution/experiments", s.requireUser(http.HandlerFunc(s.experimentList)))
	mux.Handle("POST /api/v1/evolution/experiments", s.requireUser(http.HandlerFunc(s.experimentStart)))
	mux.Handle("GET /api/v1/evolution/experiments/{id}", s.requireUser(http.HandlerFunc(s.experimentGet)))
	mux.Handle("GET /api/v1/plugin-forge/projects", s.requireUser(http.HandlerFunc(s.forgeProjectList)))
	mux.Handle("POST /api/v1/plugin-forge/projects", s.requireUser(http.HandlerFunc(s.forgeProjectCreate)))
	mux.Handle("POST /api/v1/plugin-forge/projects/{id}/{action}", s.requireUser(http.HandlerFunc(s.forgeProjectAction)))
	mux.Handle("GET /api/v1/plugin-runtime/installations", s.requireUser(http.HandlerFunc(s.runtimeInstallationList)))
	mux.Handle("GET /api/v1/plugin-runtime/capabilities", s.requireUser(http.HandlerFunc(s.runtimeCapabilityList)))
	mux.Handle("GET /api/v1/plugin-runtime/surfaces", s.requireUser(http.HandlerFunc(s.runtimeSurfaceList)))
	mux.Handle("GET /api/v1/plugin-runtime/migration", s.requireUser(http.HandlerFunc(s.runtimeMigrationStatus)))
	mux.Handle("GET /api/v1/plugin-runtime/ui/{plugin}", s.requireUser(http.HandlerFunc(s.runtimeUIGet)))
	mux.Handle("POST /api/v1/plugin-runtime/ui/{plugin}/call", s.requireUser(http.HandlerFunc(s.runtimeUICall)))
	mux.Handle("POST /api/v1/plugin-runtime/ui/{plugin}/services/{service}/call", s.requireUser(http.HandlerFunc(s.runtimeUIServiceCall)))
	mux.Handle("POST /api/v1/plugin-runtime/ui/{plugin}/legacy-invoke", s.requireUser(http.HandlerFunc(s.runtimeLegacyUIInvoke)))
	mux.Handle("POST /api/v1/plugin-runtime/capabilities/{id}/invoke", s.requireUser(http.HandlerFunc(s.runtimeInvoke)))
	mux.Handle("GET /api/v1/plugin-assets/{release}/{path...}", s.requireUser(http.HandlerFunc(s.pluginAsset)))
	mux.Handle("GET /api/v2/ui/slots", s.requireUser(http.HandlerFunc(s.runtimeUISlots)))
	mux.Handle("POST /api/v2/ui/plugins/{plugin}/call", s.requireUser(http.HandlerFunc(s.runtimeUICall)))
	mux.Handle("POST /api/v2/ui/plugins/{plugin}/services/{service}/call", s.requireUser(http.HandlerFunc(s.runtimeUIServiceCall)))
	mux.Handle("GET /api/v2/plugin-assets/{release}/{path...}", s.requireUser(http.HandlerFunc(s.pluginAsset)))
	mux.Handle("GET /api/v2/agent/runs/{id}/trace", s.requireUser(http.HandlerFunc(s.conversationTrace)))
	return s.recoverer(s.cors(s.logging(mux)))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK
	if err := s.store.Ping(r.Context()); err != nil {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}
	write(w, code, map[string]any{"status": status, "plugins": s.plugins.Snapshots()})
}

type authInput struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var in authInput
	if !decode(w, r, &in) {
		return
	}
	u, token, err := s.auth.Register(r.Context(), in.Email, in.Password, in.DisplayName)
	if err != nil {
		fail(w, err)
		return
	}
	s.setCookie(w, token)
	write(w, http.StatusCreated, u)
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in authInput
	if !decode(w, r, &in) {
		return
	}
	u, token, err := s.auth.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		fail(w, err)
		return
	}
	s.setCookie(w, token)
	write(w, http.StatusOK, u)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	cookie, _ := r.Cookie(cookieName)
	if cookie != nil {
		_ = s.auth.Logout(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) { write(w, http.StatusOK, currentUser(r)) }
func (s *Server) pluginList(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, s.plugins.Snapshots())
}

func (s *Server) providerList(w http.ResponseWriter, r *http.Request) {
	items, err := s.providers.List(r.Context(), currentUser(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (s *Server) providerCreate(w http.ResponseWriter, r *http.Request) {
	var in provider.Input
	if !decode(w, r, &in) {
		return
	}
	p, err := s.providers.Create(r.Context(), currentUser(r).ID, in)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, p)
}
func (s *Server) providerUpdate(w http.ResponseWriter, r *http.Request) {
	var in provider.Input
	if !decode(w, r, &in) {
		return
	}
	p, err := s.providers.Update(r.Context(), currentUser(r).ID, r.PathValue("id"), in)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, p)
}
func (s *Server) providerTest(w http.ResponseWriter, r *http.Request) {
	if err := s.providers.Test(r.Context(), currentUser(r).ID, r.PathValue("id")); err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) conversationList(w http.ResponseWriter, r *http.Request) {
	items, err := s.agent.List(r.Context(), currentUser(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (s *Server) conversationCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title      string `json:"title"`
		ProviderID string `json:"providerId"`
	}
	if !decode(w, r, &in) {
		return
	}
	c, err := s.agent.Create(r.Context(), currentUser(r).ID, in.Title, in.ProviderID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, c)
}
func (s *Server) conversationGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.agent.Get(r.Context(), currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, c)
}

func (s *Server) conversationTrace(w http.ResponseWriter, r *http.Request) {
	events, err := s.agent.Trace(r.Context(), currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, events)
}
func (s *Server) messageCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	m, err := s.agent.Turn(r.Context(), currentUser(r).ID, r.PathValue("id"), in.Content)
	if err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusCreated, m)
}

func (s *Server) forgeProjectList(w http.ResponseWriter, r *http.Request) {
	items, err := s.forge.ListProjects(r.Context(), currentUser(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) forgeProjectCreate(w http.ResponseWriter, r *http.Request) {
	var in pluginforge.CreateInput
	if !decode(w, r, &in) {
		return
	}
	project, err := s.forge.Create(r.Context(), currentUser(r).ID, in)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, project)
}

func (s *Server) forgeProjectAction(w http.ResponseWriter, r *http.Request) {
	userID := currentUser(r).ID
	projectID := r.PathValue("id")
	switch r.PathValue("action") {
	case "generate":
		project, err := s.forge.Generate(r.Context(), userID, projectID)
		s.writeForgeResult(w, project, nil, nil, err)
	case "build":
		project, release, err := s.forge.BuildAndTest(r.Context(), userID, projectID)
		s.writeForgeResult(w, project, &release, nil, err)
	case "request-approval":
		project, release, err := s.forge.RequestApproval(r.Context(), userID, projectID)
		s.writeForgeResult(w, project, &release, nil, err)
	case "approve":
		project, err := s.forge.Approve(r.Context(), userID, projectID)
		s.writeForgeResult(w, project, nil, nil, err)
	case "install":
		project, installation, err := s.forge.Install(r.Context(), userID, projectID)
		s.writeForgeResult(w, project, nil, &installation, err)
	case "deactivate":
		project, err := s.forge.Deactivate(r.Context(), userID, projectID)
		s.writeForgeResult(w, project, nil, nil, err)
	case "revise":
		project, err := s.forge.BeginRevision(r.Context(), userID, projectID)
		s.writeForgeResult(w, project, nil, nil, err)
	case "write-source":
		var in pluginforge.SourceFileInput
		if !decode(w, r, &in) {
			return
		}
		project, err := s.forge.WriteSourceFile(r.Context(), userID, projectID, in)
		s.writeForgeResult(w, project, nil, nil, err)
	case "rollback":
		var in struct {
			ReleaseID string `json:"releaseId"`
		}
		if !decode(w, r, &in) {
			return
		}
		project, installation, err := s.forge.Rollback(r.Context(), userID, projectID, in.ReleaseID)
		s.writeForgeResult(w, project, nil, &installation, err)
	default:
		write(w, http.StatusNotFound, map[string]string{"error": "unknown plugin action"})
	}
}

func (s *Server) writeForgeResult(w http.ResponseWriter, project pluginforge.Project, release *pluginforge.Release, installation *pluginforge.Installation, err error) {
	if err != nil {
		write(w, http.StatusConflict, map[string]any{"error": err.Error(), "project": project})
		return
	}
	write(w, http.StatusOK, map[string]any{"project": project, "release": release, "installation": installation})
}

func (s *Server) runtimeInstallationList(w http.ResponseWriter, r *http.Request) {
	items, err := s.forge.ListInstallations(r.Context(), currentUser(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) runtimeCapabilityList(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, s.forge.Capabilities(currentUser(r).ID))
}

func (s *Server) runtimeSurfaceList(w http.ResponseWriter, r *http.Request) {
	items, err := s.forge.SurfaceStates(r.Context(), currentUser(r).ID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) runtimeMigrationStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.forge.MigrationStatus(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, status)
}

func (s *Server) runtimeUIGet(w http.ResponseWriter, r *http.Request) {
	binding, ok := s.forge.UIBinding(currentUser(r).ID, r.PathValue("plugin"))
	if !ok {
		fail(w, domain.ErrNotFound)
		return
	}
	write(w, http.StatusOK, binding)
}

func (s *Server) runtimeUISlots(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, s.forge.UIBindings(currentUser(r).ID))
}

func (s *Server) runtimeUICall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Operation string          `json:"operation"`
		Input     json.RawMessage `json:"input"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.forge.UICall(r.Context(), currentUser(r).ID, r.PathValue("plugin"), in.Operation, in.Input)
	if err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusOK, map[string]any{"output": result})
}

func (s *Server) runtimeUIServiceCall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Input json.RawMessage `json:"input"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.forge.UIServiceCall(r.Context(), currentUser(r).ID, r.PathValue("plugin"), r.PathValue("service"), in.Input)
	if err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusOK, map[string]any{"output": result})
}

func (s *Server) runtimeLegacyUIInvoke(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CapabilitySuffix string          `json:"capabilitySuffix"`
		Input            json.RawMessage `json:"input"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.forge.LegacyUICall(r.Context(), currentUser(r).ID, r.PathValue("plugin"), in.CapabilitySuffix, in.Input)
	if err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusOK, map[string]any{"output": result})
}

func (s *Server) runtimeInvoke(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Input json.RawMessage `json:"input"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.forge.Invoke(r.Context(), currentUser(r).ID, r.PathValue("id"), in.Input)
	if err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusOK, map[string]any{"output": result})
}

func (s *Server) pluginAsset(w http.ResponseWriter, r *http.Request) {
	path, err := s.forge.Asset(r.Context(), currentUser(r).ID, r.PathValue("release"), r.PathValue("path"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline'; connect-src 'none'; img-src 'self' data:; frame-ancestors "+s.frontendOrigin)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if contentType := mime.TypeByExtension(filepath.Ext(path)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	file, err := os.Open(path)
	if err != nil {
		fail(w, domain.ErrNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		fail(w, domain.ErrNotFound)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil {
			fail(w, domain.ErrUnauthorized)
			return
		}
		u, err := s.auth.User(r.Context(), cookie.Value)
		if err != nil {
			fail(w, domain.ErrUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}
func currentUser(r *http.Request) domain.User { return r.Context().Value(userKey).(domain.User) }
func (s *Server) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies, MaxAge: int((30 * 24 * time.Hour).Seconds())})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")
		if origin != "" && origin != s.frontendOrigin {
			write(w, http.StatusForbidden, map[string]string{"error": "origin is not allowed"})
			return
		}
		if origin == s.frontendOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("request panic", "error", recovered)
				write(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalid):
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid input"})
	case errors.Is(err, domain.ErrConflict):
		write(w, http.StatusConflict, map[string]string{"error": "resource already exists"})
	case errors.Is(err, domain.ErrUnauthorized):
		write(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
	case errors.Is(err, domain.ErrNotFound):
		write(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		slog.Error("request failed", "error", err)
		write(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}
