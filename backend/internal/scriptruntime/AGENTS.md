# Isolated JavaScript execution

This package executes capability fragments in managed JavaScript workers and owns worker supervision and OS-specific containment.

- All programs and inputs are untrusted. Enforce timeout, memory, output, cancellation, and process cleanup limits on every platform.
- Return worker startup, execution, termination, and containment failures; do not treat a returned object as proof the requested work completed.
- Keep Windows Job Object behavior and non-Windows fallback behavior explicit and covered separately.
- From `backend/`, run `go test ./internal/scriptruntime` and `go test ./...` for changes.
