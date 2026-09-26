# Capsule promotion jobs

This package verifies a capsule, creates or updates a Plugin Forge reference project, and persists the asynchronous promotion job lifecycle.

- Job state must survive restart and reflect observed work; recoverable jobs must be reconciled rather than left indefinitely active.
- Promotion does not bypass plugin permission grants or installation authorization.
- Persist each transition and error before reporting it; retries must be idempotent for the same capsule digest.
- From `backend/`, run `go test ./internal/promotion` and `go test ./...` for changes.
