package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Addr           string
	DataDir        string
	FrontendOrigin string
}

func Load() Config {
	return Config{
		Addr:           env("AXIOM_ADDR", "127.0.0.1:8080"),
		DataDir:        env("AXIOM_DATA_DIR", filepath.Join("..", "data")),
		FrontendOrigin: env("AXIOM_FRONTEND_ORIGIN", "http://127.0.0.1:3000"),
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
