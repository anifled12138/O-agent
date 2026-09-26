# Agent generations and challenges

This package manages immutable Agent Definitions and Generations, challenge records, conversation pinning, and the user-gated promotion decision.

- Recompute and validate definition digests from canonical content; existing conversations stay pinned to their original generation.
- Candidate creation is not promotion. Promotion must verify completed evaluation evidence and update generation/challenge state consistently.
- Propagate linked storage failures; multi-record state changes belong in one transaction or need explicit compensation.
- From `backend/`, run `go test ./internal/evolution` and `go test ./...` for lifecycle changes.
