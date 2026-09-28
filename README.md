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
| `O_DATA_DIR` | Backend default: `../data`; desktop default: platform user data under `O/data` | SQLite database and encryption key directory |
| `O_AGENT_TEMP_DIR` | `%TEMP%/Axiom/agent-runs` (or the platform temp directory) | Private temporary scripts and artifacts scoped to Agent runs |
| `O_AGENT_MAX_MODEL_CALLS` | `300` | Configurable model-attempt budget per run, including provider retries; `0` disables this budget, with no configured upper bound |
| `O_FRONTEND_ORIGIN` | `http://127.0.0.1:3000` | Allowed browser origin |
| `O_WORKSPACE_ROOT` | `..` | Workspace exposed through approved Host brokers |

Legacy `AXIOM_*` variables remain accepted for compatibility. Agent run
directories are removed when each turn ends; startup and later run starts sweep
owned run directories older than 24 hours after a crash. Large `grep_search` results are
available only by their artifact ID during the current turn.

Closing the desktop window hides O to the system tray; the Host and active turns
continue until the tray's explicit **退出并停止任务** action is used. Provider
requests make at most three attempts total (up to two retries) for transient
transport, server, and rate-limit failures, within the per-run model attempt
budget. At most 32 messages may wait in one conversation and 256 across a
workspace; evaluation experiments also have a bounded persistent queue of 32.
The Host resumes queued evaluations from their committed trial records and
resumes regular Agent turns from encrypted checkpoints after restart when the
runtime binding is unchanged and every started tool has a committed result.
An unclosed tool operation, run-scoped artifact dependency, fragment operation,
or plugin routing change remains an explicit reconciliation state. Provider
responses that were received but not checkpointed may be requested again after
a crash. If the desktop Host exits unexpectedly, the
desktop process restarts it with an increasing delay while preserving the local
API address; explicit application exit still stops it.

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
