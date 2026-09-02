# Axiom Agent

Axiom is a local-first agent product with a Go backend and a web frontend. The
runtime follows one rule: every capability is mounted as a plugin, including
storage, authentication, model providers, the agent loop, and transports.

## Product capabilities

- Local user registration and cookie-based login
- Encrypted model-provider credentials
- OpenAI-compatible provider configuration and connectivity test
- Persistent conversations and a working model turn
- Plugin lifecycle and runtime inspection
- Responsive product UI
- V2 full-stack, UI-only, Service, Agent Tool, and lazy Skill plugins
- Immutable releases, explicit grants, hot swap, rollback, and crash recovery
- Lazy capability search/load with per-turn release pinning and durable traces
- Separate sandboxed UI bridge and Agent authority
- Dependency-checked UI-to-Service bridge with an explicit caller principal
- Host resource brokers and Windows Job Object containment

The current plugin architecture is documented in
[`docs/plugin-runtime-implementation.md`](docs/plugin-runtime-implementation.md)
and designed in [`docs/adr/0002-plugin-surfaces-and-agent-visibility.md`](docs/adr/0002-plugin-surfaces-and-agent-visibility.md).

## Run locally

Requirements: Go 1.27+ and Node.js 22+.

```powershell
# terminal 1
cd D:\agent-harness\backend
$env:Path = 'D:\DevTools\go\bin;' + $env:Path
go run ./cmd/axiom

# terminal 2
cd D:\agent-harness\frontend
npm run dev
```

Open `http://127.0.0.1:3000`. Runtime data is written to
`D:\agent-harness\data` by default and is intentionally ignored by Git.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `AXIOM_ADDR` | `127.0.0.1:8080` | Backend listen address |
| `AXIOM_DATA_DIR` | `../data` | SQLite database and encryption key directory |
| `AXIOM_FRONTEND_ORIGIN` | `http://127.0.0.1:3000` | Allowed browser origin |
| `AXIOM_WORKSPACE_ROOT` | `..` | Workspace exposed through approved Host brokers |

## Verify

```powershell
cd D:\agent-harness\backend
& 'D:\DevTools\go\bin\go.exe' test ./...
& 'D:\DevTools\go\bin\go.exe' vet ./...

cd D:\agent-harness\frontend
npm run lint
npm run build
```
