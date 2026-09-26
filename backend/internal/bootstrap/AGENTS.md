# Agent candidate bootstrap

This package asks a configured model for bounded Agent Definition candidates and submits validated candidates to Evolution.

- Model output is untrusted input: parse, normalize, and validate it through Evolution before storing it.
- Candidate generation must never install or promote a definition; promotion remains behind evaluation and explicit user action.
- Keep generation limits and schema defaults aligned with API descriptions and tests.
- From `backend/`, run `go test ./internal/bootstrap`; use `go test ./...` for cross-package changes.
