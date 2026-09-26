# Model provider gateway

This package stores provider configuration and credentials, probes endpoints, adapts provider protocols, and normalizes model completions and tool calls.

- Keep provider IDs as runtime identity; model names alone are not unique. Persist every execution-affecting configuration field in the backend.
- Never expose API keys or raw secret-bearing provider payloads in errors, traces, or logs. Classify errors while retaining safe diagnostics.
- Update adapter capability declarations, compatibility warnings, request construction, and tests together; do not claim unsupported streaming or tool behavior.
- From `backend/`, run `go test ./internal/provider` and `go test ./...` for adapter or contract changes.
