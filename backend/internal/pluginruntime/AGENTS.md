# Sandboxed plugin runtime

This package supervises installed plugin processes, brokers Host resources, pins releases to capability calls, and manages plugin surfaces.

- Plugin code is untrusted. Keep filesystem/network/process access behind brokers and preserve OS containment and per-release identity.
- Activation must reconcile observed process state with durable installation state; failures require rollback or a visible degraded state.
- Never execute a mutable project checkout as an installed release; run only verified immutable release artifacts.
- From `backend/`, run `go test ./internal/pluginruntime` and `go test ./...`; include Windows-specific checks for containment changes.
