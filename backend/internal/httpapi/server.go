package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/bootstrap"
	"axiom.local/agent/internal/core"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evalharness"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/storage"
)

type Server struct {
	workspaceID    string
	providers      *provider.Service
	agent          *agent.Service
	evolution      *evolution.Service
	evals          *evalharness.Service
	bootstrap      *bootstrap.Service
	forge          *pluginforge.Service
	store          *storage.Store
	plugins        *core.Manager
	frontendOrigin string
}

func New(workspaceID string, providerService *provider.Service, agentService *agent.Service, evolutionService *evolution.Service, evalService *evalharness.Service, bootstrapService *bootstrap.Service, forgeService *pluginforge.Service, store *storage.Store, plugins *core.Manager, origin string) *Server {
	return &Server{workspaceID: workspaceID, providers: providerService, agent: agentService, evolution: evolutionService, evals: evalService, bootstrap: bootstrapService, forge: forgeService, store: store, plugins: plugins, frontendOrigin: strings.TrimRight(origin, "/")}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/system/plugins", s.pluginList)
	mux.HandleFunc("GET /api/v1/provider-kinds", s.providerKinds)
	mux.HandleFunc("GET /api/v1/providers", s.providerList)
	mux.HandleFunc("POST /api/v1/providers", s.providerCreate)
	mux.HandleFunc("PUT /api/v1/providers/{id}", s.providerUpdate)
	mux.HandleFunc("POST /api/v1/providers/{id}/test", s.providerTest)
	mux.HandleFunc("GET /api/v1/conversations", s.conversationList)
	mux.HandleFunc("POST /api/v1/conversations", s.conversationCreate)
	mux.HandleFunc("GET /api/v1/conversations/{id}", s.conversationGet)
	mux.HandleFunc("GET /api/v1/conversations/{id}/trace", s.conversationTrace)
	mux.HandleFunc("GET /api/v1/conversations/{id}/turns", s.conversationTurns)
	mux.HandleFunc("POST /api/v1/conversations/{id}/messages", s.messageCreate)
	mux.HandleFunc("POST /api/v1/agent/turns/{id}/cancel", s.turnCancel)
	mux.HandleFunc("GET /api/v1/evolution/generations", s.generationList)
	mux.HandleFunc("POST /api/v1/evolution/generations/candidates", s.generationCreateCandidate)
	mux.HandleFunc("POST /api/v1/evolution/generations/{id}/promote", s.generationPromote)
	mux.HandleFunc("GET /api/v1/evolution/challenges", s.challengeList)
	mux.HandleFunc("POST /api/v1/evolution/challenges", s.challengeCreate)
	mux.HandleFunc("POST /api/v1/evolution/challenges/{id}/bootstrap", s.challengeBootstrap)
	mux.HandleFunc("GET /api/v1/evolution/experiments", s.experimentList)
	mux.HandleFunc("POST /api/v1/evolution/experiments", s.experimentStart)
	mux.HandleFunc("GET /api/v1/evolution/experiments/{id}", s.experimentGet)
	mux.HandleFunc("GET /api/v1/plugin-forge/projects", s.forgeProjectList)
	mux.HandleFunc("POST /api/v1/plugin-forge/projects", s.forgeProjectCreate)
	mux.HandleFunc("GET /api/v1/plugin-forge/projects/{id}/source-tree", s.forgeSourceTree)
	mux.HandleFunc("GET /api/v1/plugin-forge/projects/{id}/source", s.forgeSourceRead)
	mux.HandleFunc("GET /api/v1/plugin-forge/projects/{id}/diff", s.forgeSourceDiff)
	mux.HandleFunc("POST /api/v1/plugin-forge/projects/{id}/patch", s.forgeSourcePatch)
	mux.HandleFunc("POST /api/v1/plugin-forge/projects/{id}/{action}", s.forgeProjectAction)
	mux.HandleFunc("GET /api/v1/plugin-runtime/installations", s.runtimeInstallationList)
	mux.HandleFunc("GET /api/v1/plugin-runtime/capabilities", s.runtimeCapabilityList)
	mux.HandleFunc("GET /api/v1/plugin-runtime/surfaces", s.runtimeSurfaceList)
	mux.HandleFunc("GET /api/v1/plugin-runtime/migration", s.runtimeMigrationStatus)
	mux.HandleFunc("GET /api/v1/plugin-runtime/ui/{plugin}", s.runtimeUIGet)
	mux.HandleFunc("POST /api/v1/plugin-runtime/ui/{plugin}/call", s.runtimeUICall)
	mux.HandleFunc("POST /api/v1/plugin-runtime/ui/{plugin}/services/{service}/call", s.runtimeUIServiceCall)
	mux.HandleFunc("POST /api/v1/plugin-runtime/ui/{plugin}/legacy-invoke", s.runtimeLegacyUIInvoke)
	mux.HandleFunc("POST /api/v1/plugin-runtime/capabilities/{id}/invoke", s.runtimeInvoke)
	mux.HandleFunc("GET /api/v1/plugin-assets/{release}/{path...}", s.pluginAsset)
	mux.HandleFunc("GET /api/v2/ui/slots", s.runtimeUISlots)
	mux.HandleFunc("POST /api/v2/ui/plugins/{plugin}/call", s.runtimeUICall)
	mux.HandleFunc("POST /api/v2/ui/plugins/{plugin}/services/{service}/call", s.runtimeUIServiceCall)
	mux.HandleFunc("GET /api/v2/plugin-assets/{release}/{path...}", s.pluginAsset)
	mux.HandleFunc("GET /api/v2/agent/runs/{id}/trace", s.conversationTrace)
	mux.HandleFunc("POST /api/v2/agent/conversations/{id}/turns", s.turnSubmit)
	mux.HandleFunc("GET /api/v2/agent/turns/{id}/events", s.turnEvents)
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

func (s *Server) pluginList(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, s.plugins.Snapshots())
}

func (s *Server) providerList(w http.ResponseWriter, r *http.Request) {
	items, err := s.providers.List(r.Context(), s.workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (s *Server) providerKinds(w http.ResponseWriter, _ *http.Request) {
	write(w, http.StatusOK, provider.Kinds())
}
func (s *Server) providerCreate(w http.ResponseWriter, r *http.Request) {
	var in provider.Input
	if !decode(w, r, &in) {
		return
	}
	p, err := s.providers.Create(r.Context(), s.workspaceID, in)
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
	p, err := s.providers.Update(r.Context(), s.workspaceID, r.PathValue("id"), in)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, p)
}
func (s *Server) providerTest(w http.ResponseWriter, r *http.Request) {
	if err := s.providers.Test(r.Context(), s.workspaceID, r.PathValue("id")); err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) conversationList(w http.ResponseWriter, r *http.Request) {
	items, err := s.agent.List(r.Context(), s.workspaceID)
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
	c, err := s.agent.Create(r.Context(), s.workspaceID, in.Title, in.ProviderID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, c)
}
func (s *Server) conversationGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.agent.Get(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, c)
}

func (s *Server) conversationTrace(w http.ResponseWriter, r *http.Request) {
	events, err := s.agent.Trace(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, events)
}
func (s *Server) conversationTurns(w http.ResponseWriter, r *http.Request) {
	turns, err := s.agent.Turns(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, turns)
}
func (s *Server) turnCancel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.agent.Cancel(r.Context(), s.workspaceID, r.PathValue("id"), in.Reason); err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusAccepted, map[string]any{"ok": true})
}
func (s *Server) turnSubmit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	receipt, err := s.agent.Submit(r.Context(), s.workspaceID, r.PathValue("id"), in.Content)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusAccepted, receipt)
}
func (s *Server) turnEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		write(w, http.StatusInternalServerError, map[string]string{"error": "streaming unavailable"})
		return
	}
	afterValue := r.URL.Query().Get("after")
	if header := r.Header.Get("Last-Event-ID"); afterValue == "" && header != "" {
		afterValue = header
	}
	after, err := strconv.Atoi(afterValue)
	if afterValue != "" && (err != nil || after < 0) {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid event cursor"})
		return
	}
	turnID := r.PathValue("id")
	wake, unsubscribe := s.agent.SubscribeTurn(turnID)
	defer unsubscribe()
	events, err := s.agent.TurnEvents(r.Context(), s.workspaceID, turnID, after)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	terminal := false
	send := func(items []domain.TraceEvent) error {
		for _, event := range items {
			payload, marshalErr := json.Marshal(event)
			if marshalErr != nil {
				return marshalErr
			}
			if _, writeErr := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.Sequence, payload); writeErr != nil {
				return writeErr
			}
			after = event.Sequence
			terminal = event.Kind == "turn.completed" || event.Kind == "turn.failed" || event.Kind == "turn.cancelled" || event.Kind == "turn.needs_reconciliation"
		}
		flusher.Flush()
		return nil
	}
	if err := send(events); err != nil || terminal {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-wake:
			events, err := s.agent.TurnEvents(r.Context(), s.workspaceID, turnID, after)
			if err != nil || send(events) != nil || terminal {
				return
			}
		}
	}
}
func (s *Server) messageCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	m, err := s.agent.Turn(r.Context(), s.workspaceID, r.PathValue("id"), in.Content)
	if err != nil {
		if errors.Is(err, domain.ErrInvalid) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
			fail(w, err)
			return
		}
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusCreated, m)
}

func (s *Server) forgeProjectList(w http.ResponseWriter, r *http.Request) {
	items, err := s.forge.ListProjects(r.Context(), s.workspaceID)
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
	project, err := s.forge.Create(r.Context(), s.workspaceID, in)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, project)
}

func (s *Server) forgeSourceTree(w http.ResponseWriter, r *http.Request) {
	tree, err := s.forge.SourceTree(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, tree)
}

func (s *Server) forgeSourceRead(w http.ResponseWriter, r *http.Request) {
	file, err := s.forge.ReadSourceFile(r.Context(), s.workspaceID, r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, file)
}

func (s *Server) forgeSourceDiff(w http.ResponseWriter, r *http.Request) {
	diff, err := s.forge.SourceDiff(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, diff)
}

func (s *Server) forgeSourcePatch(w http.ResponseWriter, r *http.Request) {
	var in pluginforge.PatchInput
	if !decode(w, r, &in) {
		return
	}
	project, diff, err := s.forge.ApplySourcePatch(r.Context(), s.workspaceID, r.PathValue("id"), in)
	if err != nil {
		write(w, http.StatusConflict, map[string]any{"error": err.Error(), "project": project})
		return
	}
	write(w, http.StatusOK, map[string]any{"project": project, "diff": diff})
}

func (s *Server) forgeProjectAction(w http.ResponseWriter, r *http.Request) {
	userID := s.workspaceID
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
	items, err := s.forge.ListInstallations(r.Context(), s.workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}

func (s *Server) runtimeCapabilityList(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, s.forge.Capabilities(s.workspaceID))
}

func (s *Server) runtimeSurfaceList(w http.ResponseWriter, r *http.Request) {
	items, err := s.forge.SurfaceStates(r.Context(), s.workspaceID)
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
	binding, ok := s.forge.UIBinding(s.workspaceID, r.PathValue("plugin"))
	if !ok {
		fail(w, domain.ErrNotFound)
		return
	}
	write(w, http.StatusOK, binding)
}

func (s *Server) runtimeUISlots(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, s.forge.UIBindings(s.workspaceID))
}

func (s *Server) runtimeUICall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Operation string          `json:"operation"`
		Input     json.RawMessage `json:"input"`
	}
	if !decode(w, r, &in) {
		return
	}
	result, err := s.forge.UICall(r.Context(), s.workspaceID, r.PathValue("plugin"), in.Operation, in.Input)
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
	result, err := s.forge.UIServiceCall(r.Context(), s.workspaceID, r.PathValue("plugin"), r.PathValue("service"), in.Input)
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
	result, err := s.forge.LegacyUICall(r.Context(), s.workspaceID, r.PathValue("plugin"), in.CapabilitySuffix, in.Input)
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
	result, err := s.forge.Invoke(r.Context(), s.workspaceID, r.PathValue("id"), in.Input)
	if err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusOK, map[string]any{"output": result})
}

func (s *Server) pluginAsset(w http.ResponseWriter, r *http.Request) {
	path, err := s.forge.Asset(r.Context(), s.workspaceID, r.PathValue("release"), r.PathValue("path"))
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

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")
		if origin != "" && origin != s.frontendOrigin {
			write(w, http.StatusForbidden, map[string]string{"error": "origin is not allowed"})
			return
		}
		if origin == s.frontendOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID")
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
		write(w, http.StatusBadRequest, map[string]string{"error": "输入无效"})
	case errors.Is(err, domain.ErrConflict):
		write(w, http.StatusConflict, map[string]string{"error": "资源已存在"})
	case errors.Is(err, domain.ErrUnauthorized):
		write(w, http.StatusUnauthorized, map[string]string{"error": "没有执行此操作的权限"})
	case errors.Is(err, domain.ErrNotFound):
		write(w, http.StatusNotFound, map[string]string{"error": "未找到资源"})
	default:
		slog.Error("request failed", "error", err)
		write(w, http.StatusInternalServerError, map[string]string{"error": "内部错误"})
	}
}
