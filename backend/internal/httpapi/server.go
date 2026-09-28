package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/bootstrap"
	"axiom.local/agent/internal/core"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evalharness"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/mcp"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/plugins"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/storage"
	"axiom.local/agent/internal/websearch"
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
	webSearch      *websearch.Service
	frontendOrigin string
}

func New(workspaceID string, providerService *provider.Service, agentService *agent.Service, evolutionService *evolution.Service, evalService *evalharness.Service, bootstrapService *bootstrap.Service, forgeService *pluginforge.Service, store *storage.Store, plugins *core.Manager, origin string, searchServices ...*websearch.Service) *Server {
	var searchService *websearch.Service
	if len(searchServices) > 0 {
		searchService = searchServices[0]
	}
	return &Server{workspaceID: workspaceID, providers: providerService, agent: agentService, evolution: evolutionService, evals: evalService, bootstrap: bootstrapService, forge: forgeService, store: store, plugins: plugins, webSearch: searchService, frontendOrigin: strings.TrimRight(origin, "/")}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/system/plugins", s.pluginList)
	mux.HandleFunc("GET /api/v1/plugins", s.unifiedPluginList)
	mux.HandleFunc("POST /api/v1/plugins/{id}/toggle", s.unifiedPluginToggle)
	mux.HandleFunc("POST /api/v1/plugins/reload", s.unifiedPluginReload)
	mux.HandleFunc("GET /api/v1/plugins/web-search/settings", s.webSearchSettings)
	mux.HandleFunc("PUT /api/v1/plugins/web-search/settings", s.webSearchSaveSettings)
	mux.HandleFunc("DELETE /api/v1/plugins/web-search/settings", s.webSearchClearSettings)
	mux.HandleFunc("POST /api/v1/plugins/web-search/test", s.webSearchTest)
	mux.HandleFunc("POST /api/v1/plugins/mcp", s.unifiedPluginAddMCP)
	mux.HandleFunc("DELETE /api/v1/plugins/mcp/{id}", s.unifiedPluginRemoveMCP)
	mux.HandleFunc("GET /api/v1/provider-kinds", s.providerKinds)
	mux.HandleFunc("GET /api/v1/providers", s.providerList)
	mux.HandleFunc("POST /api/v1/providers", s.providerCreate)
	mux.HandleFunc("PUT /api/v1/providers/{id}", s.providerUpdate)
	mux.HandleFunc("POST /api/v1/providers/{id}/test", s.providerTest)
	mux.HandleFunc("GET /api/v1/providers/{id}/models", s.providerModels)
	mux.HandleFunc("POST /api/v1/providers/probe-models", s.providerProbeModels)
	mux.HandleFunc("GET /api/v1/projects", s.projectList)
	mux.HandleFunc("POST /api/v1/projects", s.projectCreate)
	mux.HandleFunc("POST /api/v1/projects/git-clone", s.projectGitClone)
	mux.HandleFunc("POST /api/v1/system/select-directory", s.systemSelectDirectory)
	mux.HandleFunc("GET /api/v1/projects/{id}", s.projectGet)
	mux.HandleFunc("PUT /api/v1/projects/{id}", s.projectUpdate)
	mux.HandleFunc("PATCH /api/v1/projects/{id}", s.projectUpdate)
	mux.HandleFunc("DELETE /api/v1/projects/{id}", s.projectDelete)
	mux.HandleFunc("GET /api/v1/conversations", s.conversationList)
	mux.HandleFunc("POST /api/v1/conversations", s.conversationCreate)
	mux.HandleFunc("GET /api/v1/conversations/{id}", s.conversationGet)
	mux.HandleFunc("PATCH /api/v1/conversations/{id}", s.conversationUpdate)
	mux.HandleFunc("PUT /api/v1/conversations/{id}", s.conversationUpdate)
	mux.HandleFunc("PUT /api/v1/conversations/{id}/permissions", s.conversationPermissionUpdate)
	mux.HandleFunc("GET /api/v1/conversations/{id}/approvals", s.conversationApprovals)
	mux.HandleFunc("GET /api/v1/observability/tools", s.toolUsageMetrics)
	mux.HandleFunc("POST /api/v1/conversations/{id}/generate-title", s.conversationGenerateTitle)
	mux.HandleFunc("GET /api/v1/conversations/{id}/trace", s.conversationTrace)
	mux.HandleFunc("GET /api/v1/conversations/{id}/turns", s.conversationTurns)
	mux.HandleFunc("POST /api/v1/conversations/{id}/messages", s.messageCreate)
	mux.HandleFunc("POST /api/v1/agent/turns/{id}/cancel", s.turnCancel)
	mux.HandleFunc("POST /api/v1/agent/approvals/{id}/decision", s.approvalDecision)
	mux.HandleFunc("GET /api/v1/capabilities/fragments", s.fragmentList)
	mux.HandleFunc("GET /api/v1/capabilities/capsules", s.capsuleList)
	mux.HandleFunc("POST /api/v1/capabilities/capsules/{id}/verify", s.capsuleVerify)
	mux.HandleFunc("POST /api/v1/capabilities/capsules/{id}/promote", s.capsulePromote)
	mux.HandleFunc("GET /api/v1/capabilities/promotions", s.promotionList)
	mux.HandleFunc("POST /api/v1/capabilities/promotions/{id}/retry", s.promotionRetry)
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
	mux.HandleFunc("GET /api/v1/plugin-forge/storage", s.forgeStorageUsage)
	mux.HandleFunc("POST /api/v1/plugin-forge/projects/{id}/releases/{release}/unusable", s.forgeReleaseUnusable)
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
	mux.HandleFunc("POST /api/v2/agent/conversations/{id}/cancel", s.conversationCancel)
	mux.HandleFunc("POST /api/v2/agent/turns/{id}/cancel", s.turnCancel)
	mux.HandleFunc("GET /api/v2/agent/conversations/{id}/inbox", s.inboxList)
	mux.HandleFunc("POST /api/v2/agent/conversations/{id}/inbox", s.inboxQueue)
	mux.HandleFunc("POST /api/v2/agent/turns/{id}/retry", s.turnRetry)
	mux.HandleFunc("POST /api/v2/agent/turns/{id}/branch", s.turnBranch)
	mux.HandleFunc("POST /api/v2/agent/turns/{id}/fork", s.turnFork)
	mux.HandleFunc("GET /api/v2/agent/turns/{id}/events", s.turnEvents)
	return s.recoverer(s.cors(s.logging(mux)))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK
	var agentHealth any
	if err := s.store.Ping(r.Context()); err != nil {
		status = "degraded"
		code = http.StatusServiceUnavailable
	} else if s.agent != nil {
		runtimeStatus, err := s.agent.RuntimeHealth(r.Context(), s.workspaceID)
		if err != nil {
			status = "degraded"
			code = http.StatusServiceUnavailable
		} else {
			agentHealth = map[string]any{
				"activeRuns":             runtimeStatus.ActiveRuns,
				"queuedInputs":           runtimeStatus.QueuedInputs,
				"outstandingEvaluations": runtimeStatus.OutstandingEvaluations,
				"maxModelCalls":          runtimeStatus.MaxModelCalls,
			}
		}
	}
	write(w, code, map[string]any{"status": status, "plugins": s.plugins.Snapshots(), "agent": agentHealth})
}

func (s *Server) pluginList(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, s.plugins.Snapshots())
}

func (s *Server) fragmentList(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, s.agent.Fragments(s.workspaceID, strings.TrimSpace(r.URL.Query().Get("conversationId"))))
}

func (s *Server) capsuleList(w http.ResponseWriter, _ *http.Request) {
	write(w, http.StatusOK, s.agent.Capsules())
}

func (s *Server) capsuleVerify(w http.ResponseWriter, r *http.Request) {
	report := s.agent.VerifyCapsule(r.Context(), r.PathValue("id"))
	write(w, http.StatusOK, report)
}

func (s *Server) capsulePromote(w http.ResponseWriter, r *http.Request) {
	job, err := s.agent.PromoteCapsule(s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusAccepted, job)
}

func (s *Server) promotionList(w http.ResponseWriter, _ *http.Request) {
	write(w, http.StatusOK, s.agent.Promotions(s.workspaceID))
}

func (s *Server) promotionRetry(w http.ResponseWriter, r *http.Request) {
	job, err := s.agent.RetryPromotion(s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusAccepted, job)
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

func (s *Server) providerModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.providers.ProviderModels(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) providerProbeModels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BaseURL string `json:"baseUrl"`
		APIKey  string `json:"apiKey"`
	}
	if !decode(w, r, &in) {
		return
	}
	models, err := s.providers.ProbeModels(r.Context(), in.BaseURL, in.APIKey)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) conversationList(w http.ResponseWriter, r *http.Request) {
	if s.agent != nil {
		items, err := s.agent.List(r.Context(), s.workspaceID)
		if err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, items)
		return
	}
	if s.store != nil {
		items, err := s.store.ListConversations(r.Context(), s.workspaceID)
		if err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, items)
		return
	}
	fail(w, errors.New("agent service not initialized"))
}
func (s *Server) projectList(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListProjects(r.Context(), s.workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, list)
}

func (s *Server) projectCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name                string `json:"name"`
		Instructions        string `json:"instructions"`
		InstructionsEnabled bool   `json:"instructionsEnabled"`
		Workdir             string `json:"workdir"`
		RemoteRepoURL       string `json:"remoteRepoUrl"`
		RemoteBranch        string `json:"remoteBranch"`
	}
	if !decode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	proj := domain.Project{
		ID:                  "proj_" + hex.EncodeToString(raw),
		UserID:              s.workspaceID,
		Name:                name,
		Instructions:        strings.TrimSpace(in.Instructions),
		InstructionsEnabled: in.InstructionsEnabled,
		Workdir:             strings.TrimSpace(in.Workdir),
		RemoteRepoURL:       strings.TrimSpace(in.RemoteRepoURL),
		RemoteBranch:        strings.TrimSpace(in.RemoteBranch),
		CreatedAt:           time.Now().UTC(),
		UpdatedAt:           time.Now().UTC(),
	}
	if err := s.store.CreateProject(r.Context(), proj); err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, proj)
}

func (s *Server) projectGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	proj, err := s.store.Project(r.Context(), s.workspaceID, id)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, proj)
}

func (s *Server) projectUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.Project(r.Context(), s.workspaceID, id)
	if err != nil {
		fail(w, err)
		return
	}
	var in struct {
		Name                *string `json:"name"`
		Instructions        *string `json:"instructions"`
		InstructionsEnabled *bool   `json:"instructionsEnabled"`
		Workdir             *string `json:"workdir"`
		RemoteRepoURL       *string `json:"remoteRepoUrl"`
		RemoteBranch        *string `json:"remoteBranch"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Name != nil {
		existing.Name = strings.TrimSpace(*in.Name)
	}
	if in.Instructions != nil {
		existing.Instructions = strings.TrimSpace(*in.Instructions)
	}
	if in.InstructionsEnabled != nil {
		existing.InstructionsEnabled = *in.InstructionsEnabled
	}
	if in.Workdir != nil {
		existing.Workdir = strings.TrimSpace(*in.Workdir)
	}
	if in.RemoteRepoURL != nil {
		existing.RemoteRepoURL = strings.TrimSpace(*in.RemoteRepoURL)
	}
	if in.RemoteBranch != nil {
		existing.RemoteBranch = strings.TrimSpace(*in.RemoteBranch)
	}
	if err := s.store.UpdateProject(r.Context(), s.workspaceID, existing); err != nil {
		fail(w, err)
		return
	}
	updated, err := s.store.Project(r.Context(), s.workspaceID, id)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, updated)
}

func (s *Server) systemSelectDirectory(w http.ResponseWriter, r *http.Request) {
	var dirPath string
	var err error

	switch runtime.GOOS {
	case "windows":
		// PowerShell FolderBrowserDialog for native Windows folder picker
		psScript := `Add-Type -AssemblyName System.Windows.Forms; $f = New-Object System.Windows.Forms.FolderBrowserDialog; $f.Description = '选择项目目录'; if ($f.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $f.SelectedPath }`
		cmd := exec.CommandContext(r.Context(), "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
		out, runErr := cmd.Output()
		if runErr == nil {
			dirPath = strings.TrimSpace(string(out))
		} else {
			err = runErr
		}
	case "darwin":
		cmd := exec.CommandContext(r.Context(), "osascript", "-e", `POSIX path of (choose folder with prompt "选择项目目录")`)
		out, runErr := cmd.Output()
		if runErr == nil {
			dirPath = strings.TrimSpace(string(out))
		} else {
			err = runErr
		}
	default:
		cmd := exec.CommandContext(r.Context(), "zenity", "--file-selection", "--directory", "--title=选择项目目录")
		out, runErr := cmd.Output()
		if runErr == nil {
			dirPath = strings.TrimSpace(string(out))
		} else {
			err = runErr
		}
	}

	if err != nil && dirPath == "" {
		write(w, http.StatusOK, map[string]any{"path": "", "canceled": true})
		return
	}
	write(w, http.StatusOK, map[string]any{"path": dirPath, "canceled": dirPath == ""})
}

func (s *Server) projectGitClone(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID string `json:"projectId"`
		RepoURL   string `json:"repoUrl"`
		TargetDir string `json:"targetDir"`
		Branch    string `json:"branch"`
	}
	if !decode(w, r, &in) {
		return
	}
	repoURL := strings.TrimSpace(in.RepoURL)
	if repoURL == "" {
		fail(w, domain.ErrInvalid)
		return
	}
	targetDir := strings.TrimSpace(in.TargetDir)
	if targetDir == "" {
		base := path.Base(strings.TrimSuffix(repoURL, ".git"))
		if base == "" || base == "." || base == "/" {
			base = "repo"
		}
		targetDir = filepath.Join(s.agent.WorkspaceRoot(), base)
	}

	args := []string{"clone"}
	if in.Branch != "" {
		args = append(args, "-b", strings.TrimSpace(in.Branch))
	}
	args = append(args, repoURL, targetDir)

	cmd := exec.CommandContext(r.Context(), "git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		write(w, http.StatusBadRequest, map[string]any{
			"error":  "git clone 失败: " + strings.TrimSpace(string(out)),
			"output": string(out),
		})
		return
	}

	if in.ProjectID != "" {
		existing, pErr := s.store.Project(r.Context(), s.workspaceID, in.ProjectID)
		if pErr != nil {
			fail(w, fmt.Errorf("repository cloned to %s but project metadata could not be loaded: %w", targetDir, pErr))
			return
		}
		existing.Workdir = targetDir
		existing.RemoteRepoURL = repoURL
		if in.Branch != "" {
			existing.RemoteBranch = in.Branch
		}
		if pErr = s.store.UpdateProject(r.Context(), s.workspaceID, existing); pErr != nil {
			fail(w, fmt.Errorf("repository cloned to %s but project metadata could not be saved: %w", targetDir, pErr))
			return
		}
	}

	write(w, http.StatusOK, map[string]any{
		"ok":        true,
		"targetDir": targetDir,
		"output":    string(out),
	})
}

func (s *Server) projectDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConfirmDelete bool `json:"confirmDelete"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.ConfirmDelete {
		write(w, http.StatusBadRequest, map[string]string{"error": "explicitly confirm project removal"})
		return
	}
	id := r.PathValue("id")
	if err := s.store.DeleteProject(r.Context(), s.workspaceID, id); err != nil {
		fail(w, err)
		return
	}
	if _, err := s.store.Project(r.Context(), s.workspaceID, id); !errors.Is(err, domain.ErrNotFound) {
		if err == nil {
			err = errors.New("project removal was not observed")
		}
		fail(w, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
}

func (s *Server) conversationCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title             string                   `json:"title"`
		ProviderID        string                   `json:"providerId"`
		ProjectID         string                   `json:"projectId"`
		PermissionProfile domain.PermissionProfile `json:"permissionProfile"`
	}
	if !decode(w, r, &in) {
		return
	}
	c, err := s.agent.CreateWithProjectAndPermissionProfile(r.Context(), s.workspaceID, in.Title, in.ProviderID, in.ProjectID, in.PermissionProfile)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, c)
}
func (s *Server) conversationGet(w http.ResponseWriter, r *http.Request) {
	if s.agent != nil {
		c, err := s.agent.Get(r.Context(), s.workspaceID, r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, c)
		return
	}
	if s.store != nil {
		c, err := s.store.Conversation(r.Context(), s.workspaceID, r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, c)
		return
	}
	fail(w, errors.New("agent service not initialized"))
}

func (s *Server) conversationUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		Title     *string `json:"title"`
		ProjectID *string `json:"projectId"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Title != nil {
		if s.agent != nil {
			if _, err := s.agent.UpdateTitle(r.Context(), s.workspaceID, id, *in.Title); err != nil {
				fail(w, err)
				return
			}
		} else if s.store != nil {
			if err := s.store.UpdateConversationTitle(r.Context(), s.workspaceID, id, *in.Title); err != nil {
				fail(w, err)
				return
			}
		}
	}
	if in.ProjectID != nil {
		if s.agent != nil {
			if _, err := s.agent.UpdateProject(r.Context(), s.workspaceID, id, *in.ProjectID); err != nil {
				fail(w, err)
				return
			}
		} else if s.store != nil {
			if err := s.store.UpdateConversationProject(r.Context(), s.workspaceID, id, *in.ProjectID); err != nil {
				fail(w, err)
				return
			}
		}
	}
	if s.agent != nil {
		detail, err := s.agent.Get(r.Context(), s.workspaceID, id)
		if err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, detail)
		return
	}
	if s.store != nil {
		detail, err := s.store.Conversation(r.Context(), s.workspaceID, id)
		if err != nil {
			fail(w, err)
			return
		}
		write(w, http.StatusOK, detail)
		return
	}
	fail(w, errors.New("agent service not initialized"))
}

func (s *Server) conversationPermissionUpdate(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		fail(w, errors.New("agent service not initialized"))
		return
	}
	var in struct {
		Profile domain.PermissionProfile `json:"profile"`
	}
	if !decode(w, r, &in) {
		return
	}
	detail, err := s.agent.UpdatePermissionProfile(r.Context(), s.workspaceID, r.PathValue("id"), in.Profile)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, detail)
}

func (s *Server) conversationGenerateTitle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		ProviderID string `json:"providerId"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	detail, err := s.agent.GenerateTitle(r.Context(), s.workspaceID, id, in.ProviderID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, detail)
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
func (s *Server) conversationApprovals(w http.ResponseWriter, r *http.Request) {
	items, err := s.agent.PendingApprovals(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (s *Server) toolUsageMetrics(w http.ResponseWriter, r *http.Request) {
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 365 {
			fail(w, domain.ErrInvalid)
			return
		}
		days = parsed
	}
	items, err := s.agent.ToolUsageMetrics(r.Context(), s.workspaceID, days)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
}
func (s *Server) approvalDecision(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Choice string `json:"choice"`
	}
	if !decode(w, r, &in) {
		return
	}
	request, err := s.agent.ResolveApproval(r.Context(), s.workspaceID, r.PathValue("id"), in.Choice)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, request)
}
func (s *Server) turnCancel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	turn, err := s.agent.Cancel(r.Context(), s.workspaceID, r.PathValue("id"), in.Reason)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusAccepted, turn)
}

func (s *Server) conversationCancel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	receipt, err := s.agent.CancelConversation(r.Context(), s.workspaceID, r.PathValue("id"), in.Reason)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, receipt)
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

func (s *Server) turnRetry(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content *string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	receipt, err := s.agent.Retry(r.Context(), s.workspaceID, r.PathValue("id"), in.Content)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusAccepted, receipt)
}

func (s *Server) turnBranch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content *string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	receipt, err := s.agent.BranchRetry(r.Context(), s.workspaceID, r.PathValue("id"), in.Content)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusAccepted, receipt)
}

func (s *Server) turnFork(w http.ResponseWriter, r *http.Request) {
	conversation, err := s.agent.ForkConversation(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, conversation)
}

func (s *Server) inboxQueue(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	item, err := s.agent.QueueInput(r.Context(), s.workspaceID, r.PathValue("id"), in.Content)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusAccepted, item)
}

func (s *Server) inboxList(w http.ResponseWriter, r *http.Request) {
	items, err := s.agent.Inbox(r.Context(), s.workspaceID, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, items)
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
			terminal = event.Kind == "turn.completed" || event.Kind == "turn.incomplete" || event.Kind == "turn.failed" || event.Kind == "turn.cancelled" || event.Kind == "turn.needs_reconciliation" || event.Kind == "turn.interrupted"
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

func (s *Server) forgeStorageUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.forge.StorageUsage(r.Context(), s.workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, usage)
}

func (s *Server) forgeReleaseUnusable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm bool   `json:"confirm"`
		Reason  string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		write(w, http.StatusBadRequest, map[string]string{"error": "explicitly confirm marking this release unusable"})
		return
	}
	release, err := s.forge.MarkReleaseUnusable(r.Context(), s.workspaceID, r.PathValue("id"), r.PathValue("release"), in.Reason)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, release)
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
	case "install":
		var in struct {
			ReleaseID          string `json:"releaseId"`
			ConfirmPermissions bool   `json:"confirmPermissions"`
		}
		if !decode(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.ReleaseID) == "" || !in.ConfirmPermissions {
			write(w, http.StatusBadRequest, map[string]string{"error": "select an exact release and confirm its declared permissions"})
			return
		}
		if _, err := s.forge.ApproveRelease(r.Context(), userID, projectID, in.ReleaseID); err != nil {
			fail(w, err)
			return
		}
		project, installation, err := s.forge.InstallRelease(r.Context(), userID, projectID, in.ReleaseID)
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
			ReleaseID          string `json:"releaseId"`
			ConfirmPermissions bool   `json:"confirmPermissions"`
		}
		if !decode(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.ReleaseID) == "" || !in.ConfirmPermissions {
			write(w, http.StatusBadRequest, map[string]string{"error": "select an exact release and confirm its declared permissions"})
			return
		}
		if _, err := s.forge.ApproveRelease(r.Context(), userID, projectID, in.ReleaseID); err != nil {
			fail(w, err)
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
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
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
	case errors.Is(err, domain.ErrBusy):
		write(w, http.StatusTooManyRequests, map[string]string{"error": "运行容量已满，请稍后重试；已排队的输入会在容量空闲时继续"})
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

func (s *Server) unifiedPluginList(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil || s.agent.Plugins() == nil {
		write(w, http.StatusOK, []any{})
		return
	}
	write(w, http.StatusOK, s.agent.Plugins().List())
}

func (s *Server) unifiedPluginToggle(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil || s.agent.Plugins() == nil {
		fail(w, errors.New("plugins manager not initialized"))
		return
	}
	id := r.PathValue("id")
	var req struct {
		Enabled         bool `json:"enabled"`
		ConfirmExternal bool `json:"confirmExternal"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.HasPrefix(id, "mcp:") && req.Enabled && !req.ConfirmExternal {
		write(w, http.StatusBadRequest, map[string]string{"error": "explicitly confirm starting this MCP server"})
		return
	}
	if id == "core:web_search" && req.Enabled && (s.webSearch == nil || !s.webSearch.Configured()) {
		write(w, http.StatusConflict, map[string]string{"error": "请先在联网搜索插件设置中保存 Exa API Key"})
		return
	}
	if err := s.agent.Plugins().Toggle(id, req.Enabled); err != nil {
		fail(w, err)
		return
	}
	for _, plugin := range s.agent.Plugins().List() {
		if plugin.ID != id {
			continue
		}
		expected := plugins.StatusDisabled
		if req.Enabled {
			expected = plugins.StatusEnabled
		}
		if plugin.Status != expected {
			write(w, http.StatusConflict, map[string]string{"error": "插件状态与请求不一致"})
			return
		}
		write(w, http.StatusOK, plugin)
		return
	}
	write(w, http.StatusConflict, map[string]string{"error": "插件状态读回失败"})
}

func (s *Server) unifiedPluginReload(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil || s.agent.Plugins() == nil {
		fail(w, errors.New("plugins manager not initialized"))
		return
	}
	if err := s.agent.Plugins().Reload(); err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, s.agent.Plugins().List())
}

func (s *Server) webSearchSettings(w http.ResponseWriter, _ *http.Request) {
	if s.webSearch == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "联网搜索插件尚未初始化"})
		return
	}
	write(w, http.StatusOK, s.webSearch.Settings())
}

func (s *Server) webSearchSaveSettings(w http.ResponseWriter, r *http.Request) {
	if s.webSearch == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "联网搜索插件尚未初始化"})
		return
	}
	var req struct {
		APIKey string `json:"apiKey"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.webSearch.SaveAPIKey(r.Context(), req.APIKey); err != nil {
		if errors.Is(err, domain.ErrInvalid) {
			write(w, http.StatusBadRequest, map[string]string{"error": "API Key 无效，请粘贴完整密钥（8 到 4096 个字符）"})
			return
		}
		fail(w, err)
		return
	}
	settings := s.webSearch.Settings()
	if !settings.Configured {
		write(w, http.StatusConflict, map[string]string{"error": "API Key 已提交，但配置读回未确认"})
		return
	}
	write(w, http.StatusOK, settings)
}

func (s *Server) webSearchClearSettings(w http.ResponseWriter, r *http.Request) {
	if s.webSearch == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "联网搜索插件尚未初始化"})
		return
	}
	if s.agent != nil && s.agent.Plugins() != nil && s.agent.Plugins().IsCoreToolEnabled("web_search") {
		write(w, http.StatusConflict, map[string]string{"error": "请先停用联网搜索插件，再移除密钥"})
		return
	}
	if err := s.webSearch.ClearAPIKey(r.Context()); err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, s.webSearch.Settings())
}

func (s *Server) webSearchTest(w http.ResponseWriter, r *http.Request) {
	if s.webSearch == nil {
		write(w, http.StatusServiceUnavailable, map[string]string{"error": "联网搜索插件尚未初始化"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	count, err := s.webSearch.Test(ctx)
	if err != nil {
		write(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	write(w, http.StatusOK, map[string]any{"passed": true, "resultCount": count, "settings": s.webSearch.Settings()})
}

func (s *Server) unifiedPluginAddMCP(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil || s.agent.Plugins() == nil {
		fail(w, errors.New("plugins manager not initialized"))
		return
	}
	var in struct {
		Config        mcp.ServerConfig `json:"config"`
		ConfirmLaunch bool             `json:"confirmLaunch"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.ConfirmLaunch {
		write(w, http.StatusBadRequest, map[string]string{"error": "explicitly confirm launching this MCP process"})
		return
	}
	cfg := in.Config
	if err := s.agent.Plugins().MCPManager().AddOrUpdateConfig(cfg); err != nil {
		fail(w, err)
		return
	}
	for _, plugin := range s.agent.Plugins().List() {
		if plugin.ID != "mcp:"+cfg.ID {
			continue
		}
		if cfg.Enabled && plugin.Status != plugins.StatusEnabled {
			write(w, http.StatusConflict, map[string]string{"error": "MCP 服务已保存但运行时未确认连接"})
			return
		}
		write(w, http.StatusOK, s.agent.Plugins().List())
		return
	}
	write(w, http.StatusConflict, map[string]string{"error": "MCP 配置读回失败"})
}

func (s *Server) unifiedPluginRemoveMCP(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil || s.agent.Plugins() == nil {
		fail(w, errors.New("plugins manager not initialized"))
		return
	}
	var in struct {
		ConfirmRemove bool `json:"confirmRemove"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.ConfirmRemove {
		write(w, http.StatusBadRequest, map[string]string{"error": "explicitly confirm MCP server removal"})
		return
	}
	qualifiedID := r.PathValue("id")
	id := strings.TrimPrefix(qualifiedID, "mcp:")
	if strings.TrimSpace(id) == "" {
		write(w, http.StatusBadRequest, map[string]string{"error": "invalid MCP server id"})
		return
	}
	found := false
	for _, cfg := range s.agent.Plugins().MCPManager().ListConfigs() {
		if cfg.ID == id {
			found = true
			break
		}
	}
	if !found {
		fail(w, domain.ErrNotFound)
		return
	}
	if err := s.agent.Plugins().MCPManager().Remove(id); err != nil {
		fail(w, err)
		return
	}
	for _, cfg := range s.agent.Plugins().MCPManager().ListConfigs() {
		if cfg.ID == id {
			write(w, http.StatusConflict, map[string]string{"error": "MCP 配置仍然存在，移除未完成"})
			return
		}
	}
	write(w, http.StatusOK, map[string]any{"removed": id})
}
