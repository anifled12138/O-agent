# O Agent

O is a local-first agent product with a Go backend and an Electron desktop
client. The same React interface remains available as a development Web UI. The
runtime follows one rule: every capability is mounted as a plugin, including
storage, model providers, the agent loop, and transports.

## Product capabilities

- Single-user local workspace with no login screen or session cookies
- Encrypted model-provider credentials
- OpenAI Responses, Anthropic Messages, DeepSeek Chat, and generic OpenAI-compatible providers
- Persistent conversations and a working model turn
- Durable asynchronous turns with cancellation, restart classification, and resumable SSE events
- Plugin lifecycle and runtime inspection
- Responsive product UI
- Windows desktop process with an embedded UI bundle and managed Go Host
- V2 full-stack, UI-only, Service, Agent Tool, and lazy Skill plugins
- Immutable releases, explicit grants, hot swap, rollback, and crash recovery
- Lazy capability search/load with per-turn release pinning and durable traces
- Agent-driven inspect/patch/build plugin authoring with per-change Git commits
- Separate sandboxed UI bridge and Agent authority
- Dependency-checked UI-to-Service bridge with an explicit caller principal
- Host resource brokers and Windows Job Object containment
- Immutable Agent Definitions and generation-pinned conversations
- Frontier Challenges with model-assisted bounded candidate generation
- Paired Baseline/Candidate A/B evaluation and user-gated promotion
- Evolution Lab for lineage, evidence, metrics, and promotion control

The current plugin architecture is documented in
[`docs/plugin-runtime-implementation.md`](docs/plugin-runtime-implementation.md)
and designed in [`docs/adr/0002-plugin-surfaces-and-agent-visibility.md`](docs/adr/0002-plugin-surfaces-and-agent-visibility.md).
The target desktop product and Agent runtime architecture is drafted in
[`docs/system-design-v0.1.md`](docs/system-design-v0.1.md). The next product
direction—recursive Agent bootstrapping with generation-level A/B evaluation—is
defined in
[`docs/prd-recursive-bootstrap-agent-v0.1.md`](docs/prd-recursive-bootstrap-agent-v0.1.md).
The implemented M0/M1 vertical slice is documented in
[`docs/evolution-runtime-implementation.md`](docs/evolution-runtime-implementation.md).
The executable architecture and implementation contracts are indexed in
[`docs/design/README.md`](docs/design/README.md).

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

To run the real desktop client in development, use one command. It builds and
owns the Go Host automatically:

```powershell
cd D:\agent-harness
npm run dev:desktop
```

To produce both a portable Windows application folder and an installer:

```powershell
cd D:\agent-harness
npm run build:desktop
```

Versioned outputs are written under `D:\agent-harness\release`. The desktop
runtime implementation is documented in
[`docs/desktop-client-implementation.md`](docs/desktop-client-implementation.md).

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `O_ADDR` | `127.0.0.1:9171` | Backend listen address |
| `O_DATA_DIR` | `../data` | SQLite database and encryption key directory |
| `O_FRONTEND_ORIGIN` | `http://127.0.0.1:3000` | Allowed browser origin |
| `O_WORKSPACE_ROOT` | `..` | Workspace exposed through approved Host brokers |

Legacy `AXIOM_*` variables remain accepted for compatibility.

## Verify

```powershell
cd D:\agent-harness\backend
& 'D:\DevTools\go\bin\go.exe' test ./...
& 'D:\DevTools\go\bin\go.exe' vet ./...

cd D:\agent-harness\frontend
npm run lint
npm run build
npm run build:desktop-ui

cd D:\agent-harness\desktop
npm audit --audit-level=high
```
