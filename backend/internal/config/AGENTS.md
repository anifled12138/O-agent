# Host configuration

This package parses environment configuration and resolves the Host address, data directory, workspace root, and frontend origin.

- Keep defaults, validation, legacy variable compatibility, and README documentation in sync.
- Resolve and validate paths before exposing them to storage or workspace brokers; never log credentials.
- Configuration errors should fail startup with an actionable message rather than silently selecting a different security boundary.
- From `backend/`, run `go test ./internal/config` and `go test ./...` for configuration changes.
