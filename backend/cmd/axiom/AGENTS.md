# Axiom Host entry point

This package is the composition root: it loads configuration, registers and starts backend components, wires the HTTP API, handles readiness, and shuts services down.

- Keep dependency order, startup recovery, readiness, and reverse-order shutdown explicit. Return initialization and shutdown errors; do not hide partial startup.
- Put domain behavior in `internal` services rather than handlers or component registration closures.
- Preserve local-only binding and the configured data/workspace boundaries.
- Develop from `backend/`; run focused checks with `go test ./cmd/axiom` and the full backend suite with `go test ./...`.
