# Shared domain contracts

This package contains the cross-service entities, status values, and errors used by storage, runtime services, and API responses.

- Keep this package independent of HTTP, database, and UI implementation details.
- Treat status names and JSON fields as external contracts; update consumers, documentation, and tests when changing them.
- Avoid duplicate booleans for distinct lifecycle states; model configured, running, healthy, incomplete, and terminal states precisely.
- From `backend/`, run `go test ./...` after changing shared types or status contracts.
