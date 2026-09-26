# Plugin manifest contracts

This package decodes, normalizes, validates, and digests plugin manifests and their declared surfaces, capabilities, and permissions.

- Manifests are untrusted input. Reject unknown/invalid authority declarations and unsafe paths before build or activation.
- Canonicalization and permission digests are security contracts; changing them requires compatibility review and tests.
- Keep schema validation consistent with runtime enforcement and user-visible grant descriptions.
- From `backend/`, run `go test ./internal/pluginmanifest` and `go test ./...` for contract changes.
