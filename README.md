# Axiom Agent

Axiom is a local-first agent product with a Go backend and a web frontend. The
runtime follows one rule: every capability is mounted as a plugin, including
storage, authentication, model providers, the agent loop, and transports.

## First milestone

- Local user registration and cookie-based login
- Encrypted model-provider credentials
- OpenAI-compatible provider configuration and connectivity test
- Persistent conversations and a working model turn
- Plugin lifecycle and runtime inspection
- Responsive product UI

## Run locally

Requirements: Go 1.27+ and Node.js 22+.

```powershell
# terminal 1
cd D:\agent-harness\backend
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

