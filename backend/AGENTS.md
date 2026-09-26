# Backend rules

This tree contains the Go Host, domain/runtime services, provider adapters, persistence, and the local HTTP API.

- Run focused Go checks from `backend/` (for example `go test ./internal/agent`) and the full suite with `go test ./...`.
- Run `go vet ./...` for changes to interfaces, concurrency, or resource lifecycles.
- Keep package-local ownership and extra invariants in the nearest `internal/<package>/AGENTS.md`.

- Backend state is authoritative. Do not rely on localStorage or frontend-only
  configuration for model execution, permissions, budgets, or plugin status.
- Any mutation that spans storage and a runtime side effect must be transactional
  or compensating: preserve the old state, perform the effect, persist the new
  state, and roll back on failure.
- Do not ignore storage, migration, reload, activation, or shutdown errors when
  the API would otherwise report success.
- Keep domain validation limits and runtime enforcement in one shared constant
  or validate that they are identical in tests.
