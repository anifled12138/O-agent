package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/artifactstore"
	"axiom.local/agent/internal/bootstrap"
	"axiom.local/agent/internal/browsercontrol"
	"axiom.local/agent/internal/config"
	"axiom.local/agent/internal/core"
	"axiom.local/agent/internal/domain"
	"axiom.local/agent/internal/evalharness"
	"axiom.local/agent/internal/evolution"
	"axiom.local/agent/internal/execution"
	"axiom.local/agent/internal/gitcredential"
	"axiom.local/agent/internal/httpapi"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginruntime"
	pluginsInternal "axiom.local/agent/internal/plugins"
	"axiom.local/agent/internal/projectquota"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/sandbox"
	"axiom.local/agent/internal/scriptruntime"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
	"axiom.local/agent/internal/websearch"
)

func main() {
	if scriptruntime.IsWorker() {
		os.Exit(scriptruntime.RunWorker(os.Stdin, os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == "--browser-worker" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "browser worker requires one trusted browser executable path")
			os.Exit(2)
		}
		if err := browsercontrol.RunWorker(os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		if err := runBackupCommand(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--git-credential-helper" {
		if err := sandbox.RunGitCredentialHelper(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("O stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	absoluteData, _ := filepath.Abs(cfg.DataDir)
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	ctx, stopSignals := signal.NotifyContext(rootCtx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if os.Getenv("O_DESKTOP_STDIN") == "1" {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			cancelRoot()
		}()
	}
	host := core.NewHost()
	plugins := core.NewManager(host)
	var store *storage.Store
	var workspaceID string
	var forgeRepo *pluginforge.Repository
	var forgeRuntime *pluginruntime.Supervisor
	var evalService *evalharness.Service
	var agentService *agent.Service
	register(plugins, &core.Component{Info: core.Manifest{ID: "core.storage.sqlite", Version: "0.1.0", Description: "Local transactional state", Capabilities: []string{"storage.sql", "storage.migrations"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		var err error
		store, err = storage.Open(cfg.DataDir)
		if err != nil {
			return err
		}
		workspaceID, err = store.EnsureLocalWorkspaceOwner(ctx)
		if err != nil {
			return err
		}
		return h.Provide("storage", store)
	}, StopFn: func(context.Context) error { return store.Close() }})
	register(plugins, &core.Component{Info: core.Manifest{ID: "core.secrets.aesgcm", Version: "0.1.0", Description: "Local API credential vault", Requires: []string{"core.storage.sqlite"}, Capabilities: []string{"secrets.seal", "secrets.open"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		vault, err := secure.Open(cfg.DataDir)
		if err != nil {
			return err
		}
		return h.Provide("vault", vault)
	}})
	register(plugins, &core.Component{Info: core.Manifest{ID: "provider.gateway.v1", Version: "0.2.0", Description: "Provider-neutral model gateway and protocol adapters", Requires: []string{"core.storage.sqlite", "core.secrets.aesgcm"}, Capabilities: []string{"model.chat", "model.tools", "model.health", "model.protocols"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		st, err := core.Service[*storage.Store](h, "storage")
		if err != nil {
			return err
		}
		vault, err := core.Service[*secure.Vault](h, "vault")
		if err != nil {
			return err
		}
		return h.Provide("providers", provider.New(st, vault))
	}})
	register(plugins, &core.Component{Info: core.Manifest{ID: "runtime.evolution.v1", Version: "0.1.0", Description: "Versioned Agent Definitions and generations", Requires: []string{"core.storage.sqlite"}, Capabilities: []string{"agent.definition", "agent.generation", "frontier.challenge"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		st, err := core.Service[*storage.Store](h, "storage")
		if err != nil {
			return err
		}
		return h.Provide("evolution", evolution.New(st))
	}})
	register(plugins, &core.Component{Info: core.Manifest{ID: "runtime.agent.v1", Version: "0.2.0", Description: "Generation-pinned provider-neutral Agent runtime", Requires: []string{"core.storage.sqlite", "provider.gateway.v1", "runtime.plugin-forge.v1", "runtime.evolution.v1"}, Capabilities: []string{"agent.conversation", "agent.turn", "agent.tools", "agent.definition-runtime"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		st, err := core.Service[*storage.Store](h, "storage")
		if err != nil {
			return err
		}
		providers, err := core.Service[*provider.Service](h, "providers")
		if err != nil {
			return err
		}
		forge, err := core.Service[*pluginforge.Service](h, "plugin-forge")
		if err != nil {
			return err
		}
		evolutionService, err := core.Service[*evolution.Service](h, "evolution")
		if err != nil {
			return err
		}
		recovered, err := st.RecoverInterruptedAgentTurns(ctx)
		if err != nil {
			return err
		}
		if recovered > 0 {
			slog.Warn("recovered interrupted agent turns", "count", recovered)
		}
		agentService, err = agent.New(ctx, st, providers, forge, evolutionService, cfg.WorkspaceRoot, cfg.DataDir, cfg.AgentTempDir, agent.RunLimits{MaxModelCalls: cfg.AgentMaxModelCalls})
		if err != nil {
			return err
		}
		return h.Provide("agent", agentService)
	}})
	register(plugins, &core.Component{Info: core.Manifest{ID: "runtime.plugin-forge.v1", Version: "0.1.0", Description: "User-controlled full-stack plugin forge and sidecar runtime", Requires: []string{"core.storage.sqlite"}, Capabilities: []string{"plugin.generate", "plugin.build", "plugin.install", "plugin.invoke"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		var err error
		forgeRepo, err = pluginforge.OpenRepository(cfg.DataDir)
		if err != nil {
			return err
		}
		forgeRuntime, err = pluginruntime.NewWithData(cfg.WorkspaceRoot, cfg.DataDir)
		if err != nil {
			_ = forgeRepo.Close()
			return err
		}
		forge := pluginforge.NewService(forgeRepo, forgeRuntime, cfg.DataDir, cfg.WorkspaceRoot)
		if restoreErr := forge.Restore(ctx); restoreErr != nil {
			slog.Warn("some plugins could not be restored", "error", restoreErr)
		}
		return h.Provide("plugin-forge", forge)
	}, StopFn: func(context.Context) error {
		return errors.Join(forgeRuntime.Close(), forgeRepo.Close())
	}})
	register(plugins, &core.Component{Info: core.Manifest{ID: "runtime.eval-harness.v1", Version: "0.1.0", Description: "Paired A/B evaluation gate for Agent generations", Requires: []string{"core.storage.sqlite", "runtime.agent.v1", "runtime.evolution.v1"}, Capabilities: []string{"eval.paired", "eval.report", "agent.promotion-gate"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		st, err := core.Service[*storage.Store](h, "storage")
		if err != nil {
			return err
		}
		evolutionService, err := core.Service[*evolution.Service](h, "evolution")
		if err != nil {
			return err
		}
		runtimeAgent, err := core.Service[*agent.Service](h, "agent")
		if err != nil {
			return err
		}
		evalService = evalharness.New(st, evolutionService, runtimeAgent)
		return h.Provide("eval-harness", evalService)
	}, StopFn: func(context.Context) error {
		if evalService != nil {
			evalService.Close()
		}
		return nil
	}})
	register(plugins, &core.Component{Info: core.Manifest{ID: "runtime.bootstrap.v1", Version: "0.1.0", Description: "Model-assisted bounded Agent Definition candidate generation", Requires: []string{"provider.gateway.v1", "runtime.evolution.v1"}, Capabilities: []string{"agent.self-bootstrap", "agent.candidate-generation"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		evolutionService, err := core.Service[*evolution.Service](h, "evolution")
		if err != nil {
			return err
		}
		providers, err := core.Service[*provider.Service](h, "providers")
		if err != nil {
			return err
		}
		return h.Provide("bootstrap", bootstrap.New(evolutionService, providers))
	}})
	register(plugins, &core.Component{Info: core.Manifest{ID: "transport.http.v1", Version: "0.2.0", Description: "Local product API", Requires: []string{"runtime.agent.v1", "runtime.plugin-forge.v1", "runtime.eval-harness.v1", "runtime.bootstrap.v1"}, Capabilities: []string{"transport.http"}}})
	if err := plugins.StartAll(ctx); err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := plugins.StopAll(shutdownCtx); err != nil {
			slog.Error("plugin shutdown failed", "error", err)
		}
	}()
	if _, err := store.ReconcileExpiredExecutionTaskLeases(ctx, time.Now().UTC()); err != nil {
		return err
	}
	go reconcileExecutionTaskLeases(ctx, store)
	providerService, _ := core.Service[*provider.Service](host, "providers")
	vaultService, err := core.Service[*secure.Vault](host, "vault")
	if err != nil {
		return err
	}
	searchService, err := websearch.New(ctx, store, vaultService)
	if err != nil {
		return err
	}
	gitCredentialService, err := gitcredential.New(ctx, store, vaultService)
	if err != nil {
		return err
	}
	agentService, err = core.Service[*agent.Service](host, "agent")
	if err != nil {
		return err
	}
	agentService.SetGitCredentialBroker(gitCredentialService)
	if cfg.ExecutionRole == "cloud" {
		quotaClient, err := projectquota.NewHelperClient(cfg.QuotaHelperSocket)
		if err != nil {
			return fmt.Errorf("configure cloud workspace quota helper: %w", err)
		}
		quotaHealth, err := quotaClient.Health(ctx)
		if err != nil {
			return fmt.Errorf("verify cloud workspace project quota helper and mount: %w", err)
		}
		if err := agentService.ConfigureWorkspaceQuota(quotaClient, cfg.CloudWorkspaceQuota); err != nil {
			return fmt.Errorf("configure cloud workspace hard quota: %w", err)
		}
		degradedWorkspaces, err := agentService.ReconcileWorkspaceQuotas(ctx)
		if err != nil {
			return fmt.Errorf("reconcile durable cloud workspace quotas: %w", err)
		}
		if degradedWorkspaces > 0 {
			slog.Warn("cloud workspaces with missing or mismatched kernel quotas were marked degraded", "count", degradedWorkspaces)
		}
		slog.Info("cloud workspace project quota mount prerequisites verified", "filesystem", quotaHealth.Filesystem, "mountPoint", quotaHealth.MountPoint, "perTaskHardLimitVerified", quotaHealth.HardLimitVerified)
	}
	evolutionService, _ := core.Service[*evolution.Service](host, "evolution")
	evalHarnessService, _ := core.Service[*evalharness.Service](host, "eval-harness")
	bootstrapService, _ := core.Service[*bootstrap.Service](host, "bootstrap")
	forgeService, _ := core.Service[*pluginforge.Service](host, "plugin-forge")
	unifiedPlugins, err := pluginsInternal.NewManagerWithCorePlugins(cfg.WorkspaceRoot, pluginsInternal.CorePluginRegistration{
		Name: "web_search", DisplayName: "联网搜索", Tool: searchService.Tool(), Enabled: false,
		Metadata: func() map[string]string {
			settings := searchService.Settings()
			return map[string]string{"provider": settings.Provider, "configured": strconv.FormatBool(settings.Configured), "keyHint": settings.KeyHint}
		},
	})
	if err != nil {
		return err
	}
	if providerService != nil {
		unifiedPlugins.SetProviderLister(func(ctx context.Context) ([]domain.Provider, error) {
			return providerService.List(ctx, workspaceID)
		})
	}
	agentService.SetPlugins(unifiedPlugins)
	var artifactService *artifactstore.Service
	if cfg.ArtifactStoreBackend == "s3" {
		artifactService, err = artifactstore.NewWithS3(cfg.DataDir, store, artifactstore.S3Config{
			Endpoint: cfg.ArtifactS3Endpoint, Region: cfg.ArtifactS3Region, Bucket: cfg.ArtifactS3Bucket,
			AccessKeyID: cfg.ArtifactS3AccessKeyID, SecretAccessKey: cfg.ArtifactS3SecretKey,
			SessionToken: cfg.ArtifactS3SessionToken, Prefix: cfg.ArtifactS3Prefix, PathStyle: cfg.ArtifactS3PathStyle,
			EnableDirectUpload: cfg.ArtifactS3DirectUpload,
		})
	} else {
		artifactService, err = artifactstore.New(cfg.DataDir, store)
	}
	if err != nil {
		return err
	}
	agentService.SetArtifactStore(artifactService)
	if cfg.ExecutionRole == "cloud" {
		go reapCloudScratchWorkspaces(ctx, agentService, cfg.CloudWorkspaceRetention)
	}
	agentService.ResumeInterruptedTurns(workspaceID)
	agentService.ResumeQueuedInputs(workspaceID)
	if cfg.ExecutionRole == "cloud" {
		cloudNode, cloudCredentialHash, err := store.EnsureCloudExecutionNode(ctx, workspaceID, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("initialize cloud execution node: %w", err)
		}
		cloudWorker, err := execution.NewCloudWorker(store, agentService, workspaceID, cloudNode.ID)
		if err != nil {
			return fmt.Errorf("initialize cloud task worker: %w", err)
		}
		if err := cloudWorker.SetConcurrency(cfg.CloudWorkerConcurrency); err != nil {
			return fmt.Errorf("configure cloud task concurrency: %w", err)
		}
		if err := cloudWorker.SetDiskAdmissionBudget(agentService.WorkspaceRoot(), cfg.CloudWorkspaceQuota, cfg.CloudWorkspaceDiskReserve); err != nil {
			return fmt.Errorf("configure cloud task disk admission: %w", err)
		}
		go cloudWorker.Run(ctx)
		go heartbeatCloudExecutionNode(ctx, store, cloudCredentialHash)
	}
	if cfg.ExecutionRole == "local" && cfg.NodeControlURL != "" {
		localExecutor, err := execution.NewLocalAgentExecutor(agentService, providerService, workspaceID, cfg.NodeProviderID)
		if err != nil {
			return fmt.Errorf("initialize local execution node Agent runtime: %w", err)
		}
		localWorker, err := execution.NewNodeWorker(cfg.NodeControlURL, cfg.NodeCredential, runtime.GOOS, []string{"agent-runtime", "conversation-transcript", "sandboxed-tools", "safe-handoff"}, localExecutor)
		if err != nil {
			return fmt.Errorf("initialize local execution node control connection: %w", err)
		}
		if err := localWorker.SetConcurrency(cfg.NodeWorkerConcurrency); err != nil {
			return fmt.Errorf("configure local execution node concurrency: %w", err)
		}
		outboxDir, err := filepath.Abs(filepath.Join(cfg.DataDir, "node-task-outbox"))
		if err != nil {
			return fmt.Errorf("resolve local execution outbox path: %w", err)
		}
		if err := localWorker.SetOutboxDirectory(outboxDir); err != nil {
			return fmt.Errorf("configure durable local execution outbox: %w", err)
		}
		go func() {
			if err := localWorker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("local execution node stopped", "error", err)
			}
		}()
	}
	if err := evalHarnessService.Recover(ctx, workspaceID); err != nil {
		return err
	}
	api := httpapi.New(workspaceID, providerService, agentService, evolutionService, evalHarnessService, bootstrapService, forgeService, store, plugins, cfg.FrontendOrigin, searchService)
	api.SetArtifactStore(artifactService)
	api.SetRemoteAuthentication(cfg.AuthBootstrapToken)
	api.SetGitCredentials(gitCredentialService)
	server := &http.Server{Addr: cfg.Addr, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	serverErrors := make(chan error, 1)
	go func() {
		scheme := "http://"
		serve := server.ListenAndServe
		if cfg.TLSCertFile != "" {
			scheme = "https://"
			serve = func() error { return server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile) }
		}
		slog.Info("O ready", "address", scheme+cfg.Addr, "data", absoluteData)
		serverErrors <- serve()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func reapCloudScratchWorkspaces(ctx context.Context, service *agent.Service, retention time.Duration) {
	const interval = time.Hour
	if retention == 0 {
		slog.Info("new cloud scratch workspace cleanup is disabled; pending releases will still be resumed")
	}
	reap := func() {
		count, err := service.ReapCompletedScratchWorkspaces(ctx, time.Now().UTC(), retention)
		if err != nil {
			slog.Error("cloud scratch workspace cleanup left one or more workspaces for recovery", "error", err)
		}
		if count > 0 {
			slog.Info("cloud scratch workspaces safely released", "count", count)
		}
	}
	reap()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}

func reconcileExecutionTaskLeases(ctx context.Context, store *storage.Store) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count, err := store.ReconcileExpiredExecutionTaskLeases(ctx, time.Now().UTC())
			if err != nil {
				slog.Error("execution task lease reconciliation failed", "error", err)
				continue
			}
			if count > 0 {
				slog.Warn("expired execution tasks require reconciliation", "count", count)
			}
		}
	}
}

func heartbeatCloudExecutionNode(ctx context.Context, store *storage.Store, credentialHash string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		resources, resourceErr := execution.NodeResourceSnapshot()
		if resourceErr != nil {
			resources = storage.ExecutionNodeResources{}
			slog.Error("cloud execution node could not read local capacity; new node claims will pause", "error", resourceErr)
		}
		if _, err := store.HeartbeatExecutionNodeWithResources(ctx, credentialHash, "linux", []string{"agent-runtime", "cloud-execution", "sandbox"}, resources, time.Now().UTC()); err != nil {
			slog.Error("cloud execution node heartbeat failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func register(manager *core.Manager, p core.Plugin) {
	if err := manager.Register(p); err != nil {
		panic(err)
	}
}
