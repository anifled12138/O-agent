# Immutable capability capsules

This package stores and verifies content-addressed capsule definitions and source artifacts used by Agent-created reusable capabilities.

- Treat capsule identity and digest as immutable; verify digests and ownership when reading or invoking stored content.
- Keep filesystem writes atomic and surface corruption, permission, and persistence errors.
- Do not grant broader execution authority here; runtime limits and promotion belong to their respective services.
- From `backend/`, run `go test ./internal/capsule` and `go test ./...` for storage or digest changes.
