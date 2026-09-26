# Component host and lifecycle

This package registers backend components, resolves dependencies, exposes typed services, and coordinates initialization and shutdown.

- Keep dependency validation deterministic and reject missing dependencies or cycles before partial startup.
- Propagate component initialization and shutdown failures; avoid lifecycle hooks that report success without doing work.
- Keep product/domain logic outside the generic component host.
- From `backend/`, run `go test ./internal/core` and `go test ./...` for lifecycle changes.
