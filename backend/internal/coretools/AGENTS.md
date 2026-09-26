# Built-in Agent tools

This package implements the Host-provided workspace inspection and command tools exposed to Agent turns.

- Treat all model-supplied paths, regexes, and command arguments as untrusted; use the workspace resolver and explicit resource limits.
- Do not swallow filesystem, traversal, process, or output-persistence errors when returning a successful tool result.
- Keep tool schemas, descriptions, handlers, and actual authority consistent. Shell execution remains opt-in and bounded.
- From `backend/`, run `go test ./internal/coretools` and `go test ./...` for tool contract changes.
