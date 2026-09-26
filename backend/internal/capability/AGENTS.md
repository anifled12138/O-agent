# Ephemeral capability registry

This package defines and validates Agent-created JavaScript capability fragments, their schemas, scopes, evidence, and invocation registry.

- Enforce contract tier, scope, input/output schema, and runtime limits before execution; user/conversation ownership must be checked on every lookup.
- Programs are untrusted. Execute through `scriptruntime`; do not add an in-process evaluator or bypass its limits.
- Fragments are currently runtime-scoped state. Do not describe them as durable across restart unless persistence is added.
- From `backend/`, run `go test ./internal/capability` and `go test ./...` for shared contract changes.
