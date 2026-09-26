# MCP lifecycle rules

This package owns the MCP stdio/HTTP client connections, protocol handshake and tool discovery, plus persisted server configuration.

- The `mcp-go` SDK handles protocol messages; this package owns application lifecycle, persistence, observed health, and rollback.
- Develop from `backend/`; use `go test ./internal/mcp` for focused manager/client changes and `go test ./...` for catalog/API changes.
- Keep configured, enabled, running, and healthy states distinct; never expose environment values or credentials in errors or logs.

- Distinguish configured, enabled, running, and healthy MCP servers.
- Never report an MCP server as enabled/active solely because its config says
  so; a live client and successful handshake are required for active status.
- Persist configuration errors must be returned. Start/stop failures must roll
  configuration and clients back to the previous consistent state.
- Startup restoration failures must remain visible in catalog status and logs.
- MCP environment variables may contain credentials. Persist configuration with
  owner-only file permissions and atomic write/read-back; never include values
  in logs, plugin catalogs, or usage metrics.
- Starting a configured command is a local code-execution boundary. Preserve the
  explicit user consent checks at API/UI paths and report startup or shutdown
  compensation failures.
