package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Addr                   string
	DataDir                string
	WorkspaceRoot          string
	AgentTempDir           string
	FrontendOrigin         string
	AgentMaxModelCalls int
	LoadError          error
}

func Load() Config {
	config := Config{
		Addr:                   env("O_ADDR", "AXIOM_ADDR", "127.0.0.1:9171"),
		DataDir:                env("O_DATA_DIR", "AXIOM_DATA_DIR", filepath.Join("..", "data")),
		WorkspaceRoot:          env("O_WORKSPACE_ROOT", "AXIOM_WORKSPACE_ROOT", filepath.Clean("..")),
		AgentTempDir:           env("O_AGENT_TEMP_DIR", "AXIOM_AGENT_TEMP_DIR", filepath.Join(os.TempDir(), "Axiom", "agent-runs")),
		FrontendOrigin:         env("O_FRONTEND_ORIGIN", "AXIOM_FRONTEND_ORIGIN", "http://127.0.0.1:3000"),
		AgentMaxModelCalls: 300,
	}
	if value, err := intSetting("O_AGENT_MAX_MODEL_CALLS", "AXIOM_AGENT_MAX_MODEL_CALLS", config.AgentMaxModelCalls); err != nil {
		config.LoadError = err
	} else {
		config.AgentMaxModelCalls = value
	}
	return config
}

func (c Config) Validate() error {
	if c.LoadError != nil {
		return c.LoadError
	}
	if c.AgentMaxModelCalls < 0 {
		return fmt.Errorf("O_AGENT_MAX_MODEL_CALLS must be 0 (unlimited) or greater")
	}
	return nil
}

func intSetting(name, legacyName string, fallback int) (int, error) {
	value := env(name, legacyName, "")
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func env(name, legacyName, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	if value := os.Getenv(legacyName); value != "" {
		return value
	}
	return fallback
}
