package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"axiom.local/agent/internal/agent"
	"axiom.local/agent/internal/auth"
	"axiom.local/agent/internal/config"
	"axiom.local/agent/internal/core"
	"axiom.local/agent/internal/httpapi"
	"axiom.local/agent/internal/pluginforge"
	"axiom.local/agent/internal/pluginruntime"
	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/secure"
	"axiom.local/agent/internal/storage"
)

func main() {
	if err := run(); err != nil {
		slog.Error("axiom stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	absoluteData, _ := filepath.Abs(cfg.DataDir)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	host := core.NewHost()
	plugins := core.NewManager(host)
	var store *storage.Store
	var forgeRepo *pluginforge.Repository
	var forgeRuntime *pluginruntime.Supervisor
	register(plugins, &core.Component{Info: core.Manifest{ID: "core.storage.sqlite", Version: "0.1.0", Description: "Local transactional state", Capabilities: []string{"storage.sql", "storage.migrations"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		var err error
		store, err = storage.Open(cfg.DataDir)
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
	register(plugins, &core.Component{Info: core.Manifest{ID: "core.auth.local", Version: "0.1.0", Description: "Local user identity and sessions", Requires: []string{"core.storage.sqlite"}, Capabilities: []string{"auth.register", "auth.session"}}, InitFn: func(ctx context.Context, h *core.Host) error {
		st, err := core.Service[*storage.Store](h, "storage")
		if err != nil {
			return err
		}
		return h.Provide("auth", auth.New(st))
	}})
	register(plugins, &core.Component{Info: core.Manifest{ID: "provider.openai-compatible", Version: "0.1.0", Description: "OpenAI-compatible model API adapter", Requires: []string{"core.storage.sqlite", "core.secrets.aesgcm"}, Capabilities: []string{"model.chat", "model.health"}}, InitFn: func(ctx context.Context, h *core.Host) error {
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
	register(plugins, &core.Component{Info: core.Manifest{ID: "runtime.agent.v1", Version: "0.1.0", Description: "Persistent provider-neutral reasoning loop", Requires: []string{"core.storage.sqlite", "provider.openai-compatible", "runtime.plugin-forge.v1"}, Capabilities: []string{"agent.conversation", "agent.turn", "agent.tools"}}, InitFn: func(ctx context.Context, h *core.Host) error {
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
		return h.Provide("agent", agent.New(st, providers, forge))
	}})
	register(plugins, &core.Component{Info: core.Manifest{ID: "runtime.plugin-forge.v1", Version: "0.1.0", Description: "User-controlled full-stack plugin forge and sidecar runtime", Requires: []string{"core.storage.sqlite"}, Capabilities: []string{"plugin.generate", "plugin.build", "plugin.approve", "plugin.install", "plugin.invoke"}}, InitFn: func(ctx context.Context, h *core.Host) error {
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
	register(plugins, &core.Component{Info: core.Manifest{ID: "transport.http.v1", Version: "0.1.0", Description: "Local product API", Requires: []string{"core.auth.local", "runtime.agent.v1", "runtime.plugin-forge.v1"}, Capabilities: []string{"transport.http"}}})
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
	authService, _ := core.Service[*auth.Service](host, "auth")
	providerService, _ := core.Service[*provider.Service](host, "providers")
	agentService, _ := core.Service[*agent.Service](host, "agent")
	forgeService, _ := core.Service[*pluginforge.Service](host, "plugin-forge")
	server := &http.Server{Addr: cfg.Addr, Handler: httpapi.New(authService, providerService, agentService, forgeService, store, plugins, cfg.FrontendOrigin).Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("axiom ready", "address", "http://"+cfg.Addr, "data", absoluteData)
		serverErrors <- server.ListenAndServe()
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

func register(manager *core.Manager, p core.Plugin) {
	if err := manager.Register(p); err != nil {
		panic(err)
	}
}
