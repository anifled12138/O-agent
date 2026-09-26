# Plugin catalog rules

This package assembles the user-visible capability catalog and coordinates built-in tools, Skills, MCP servers, provider entries, and runtime switches.

- Develop from `backend/`; use `go test ./internal/plugins` for catalog and toggle changes and `go test ./...` for integration changes.
- Keep each runtime feature represented once. Catalog status must reflect authoritative persisted configuration and observed runtime health.
- On any toggle or reload failure, preserve the old effective state or expose an explicit error; never return success for a no-op.

- `enabled` means the feature affects runtime behavior now. If an item is only
  configured, installed, or unhealthy, expose that state instead.
- Every catalog toggle must either change authoritative state and read it back,
  or return an error. Never implement a successful no-op toggle.
- Persist user-visible core and skill switches. Test that they survive manager
  reconstruction.
- Do not publish duplicate catalog entries for one runtime feature.
