package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Addr           string
	DataDir        string
	WorkspaceRoot  string
	FrontendOrigin string
}

func Load() Config {
	return Config{
		Addr:           env("O_ADDR", "AXIOM_ADDR", "127.0.0.1:8080"),
		DataDir:        env("O_DATA_DIR", "AXIOM_DATA_DIR", filepath.Join("..", "data")),
		WorkspaceRoot:  env("O_WORKSPACE_ROOT", "AXIOM_WORKSPACE_ROOT", filepath.Clean("..")),
		FrontendOrigin: env("O_FRONTEND_ORIGIN", "AXIOM_FRONTEND_ORIGIN", "http://127.0.0.1:3000"),
	}
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
