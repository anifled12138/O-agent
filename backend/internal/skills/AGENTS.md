# Workspace skills

This package discovers Markdown skills under the workspace, persists enablement, and compiles enabled skill prompts for Agent turns.

- Skill text is untrusted prompt content, not executable authority. Keep its declared role and runtime effect clear.
- Persist toggle changes before returning success; restore the previous in-memory state when persistence fails.
- Treat a failed reload or unreadable state file as an observable error instead of silently claiming a successful reload.
- From `backend/`, run `go test ./internal/skills` and `go test ./...` for discovery or persistence changes.
