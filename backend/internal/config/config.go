package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	Addr                   string
	DataDir                string
	WorkspaceRoot          string
	AgentTempDir           string
	FrontendOrigin         string
	AgentMaxConcurrentRuns int
	AgentMaxTokensPerRun   int
	AgentMaxModelCalls     int
	AgentMaxRunDuration    time.Duration
	LoadError              error
}

func Load() Config {
	config := Config{
		Addr:                   env("O_ADDR", "AXIOM_ADDR", "127.0.0.1:9171"),
		DataDir:                env("O_DATA_DIR", "AXIOM_DATA_DIR", filepath.Join("..", "data")),
		WorkspaceRoot:          env("O_WORKSPACE_ROOT", "AXIOM_WORKSPACE_ROOT", filepath.Clean("..")),
		AgentTempDir:           env("O_AGENT_TEMP_DIR", "AXIOM_AGENT_TEMP_DIR", filepath.Join(os.TempDir(), "Axiom", "agent-runs")),
		FrontendOrigin:         env("O_FRONTEND_ORIGIN", "AXIOM_FRONTEND_ORIGIN", "http://127.0.0.1:3000"),
		AgentMaxConcurrentRuns: 3,
		AgentMaxTokensPerRun:   60000,
		AgentMaxModelCalls:     64,
		AgentMaxRunDuration:    30 * time.Minute,
	}
	if value, err := intSetting("O_AGENT_MAX_CONCURRENT_RUNS", "AXIOM_AGENT_MAX_CONCURRENT_RUNS", config.AgentMaxConcurrentRuns); err != nil {
		config.LoadError = err
	} else {
		config.AgentMaxConcurrentRuns = value
	}
	if value, err := intSetting("O_AGENT_MAX_TOKENS_PER_RUN", "AXIOM_AGENT_MAX_TOKENS_PER_RUN", config.AgentMaxTokensPerRun); err != nil {
		if config.LoadError == nil {
			config.LoadError = err
		}
	} else {
		config.AgentMaxTokensPerRun = value
	}
	if value, err := intSetting("O_AGENT_MAX_MODEL_CALLS", "AXIOM_AGENT_MAX_MODEL_CALLS", config.AgentMaxModelCalls); err != nil {
		if config.LoadError == nil {
			config.LoadError = err
		}
	} else {
		config.AgentMaxModelCalls = value
	}
	if value, err := durationSetting("O_AGENT_MAX_RUN_DURATION", "AXIOM_AGENT_MAX_RUN_DURATION", config.AgentMaxRunDuration); err != nil {
		if config.LoadError == nil {
			config.LoadError = err
		}
	} else {
		config.AgentMaxRunDuration = value
	}
	return config
}

func (c Config) Validate() error {
	if c.LoadError != nil {
		return c.LoadError
	}
	if c.AgentMaxConcurrentRuns < 1 || c.AgentMaxConcurrentRuns > 32 {
		return fmt.Errorf("O_AGENT_MAX_CONCURRENT_RUNS must be between 1 and 32")
	}
	if c.AgentMaxTokensPerRun < 1000 || c.AgentMaxTokensPerRun > 2_000_000 {
		return fmt.Errorf("O_AGENT_MAX_TOKENS_PER_RUN must be between 1000 and 2000000")
	}
	if c.AgentMaxModelCalls < 1 || c.AgentMaxModelCalls > 256 {
		return fmt.Errorf("O_AGENT_MAX_MODEL_CALLS must be between 1 and 256")
	}
	if c.AgentMaxRunDuration < time.Minute || c.AgentMaxRunDuration > 24*time.Hour {
		return fmt.Errorf("O_AGENT_MAX_RUN_DURATION must be between 1m and 24h")
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

func durationSetting(name, legacyName string, fallback time.Duration) (time.Duration, error) {
	value := env(name, legacyName, "")
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
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
