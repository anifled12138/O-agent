# Local secret vault

This package encrypts and decrypts provider credentials for local persistence.

- Keep keys and plaintext secrets out of logs, API responses, traces, and error messages.
- Fail closed on missing, invalid, or unreadable key material; never silently replace a key and make stored credentials unreadable.
- Review file permissions, atomic writes, and migration compatibility when changing the vault format.
- From `backend/`, run `go test ./...` for vault changes; add focused secure-package tests when present.
