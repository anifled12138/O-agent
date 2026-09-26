# SQLite persistence

This package owns the SQLite schema, migrations, repositories, conversation records, Agent Turn journal, and durable trace events.

- The database is authoritative. Use transactions for linked records and return every query, migration, and commit error.
- Preserve append-only event history and explicit terminal/recovery states; never replay unknown external tool effects automatically.
- Persist approval requests, turn pause state, and approval trace events in one
  transaction. Resolve requests with a pending-only compare-and-swap, and read
  back the approval and turn state before reporting a decision as saved.
- Keep aggregate observability derived from durable events and bounded by a
  caller-selected time window; never store unredacted tool arguments in metrics.
- Add migrations rather than editing already released migrations. Mutation tests should assert durable read-back and restart behavior.
- From `backend/`, run focused storage tests such as `go test ./internal/storage -run RunJournal`, then `go test ./...`.
