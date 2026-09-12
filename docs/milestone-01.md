# Milestone 01: Product spine

## Goal

Prove that O is a real local product rather than a framework diagram. A
user must be able to create a local identity, connect an API model, create a
mission, complete a model turn, and inspect the mounted runtime.

## Included

- dependency-aware plugin kernel and service registry;
- SQLite state and schema bootstrap;
- Argon2id password hashing and opaque server-side sessions;
- AES-256-GCM encryption for provider credentials;
- OpenAI-compatible model adapter and health check;
- persistent mission/message loop;
- product login, workspace, runtime inspector, and provider settings UI;
- API, unit, build, lint, audit, and browser acceptance checks.

## Deferred by design

- multi-step planning and verifier policies;
- tool execution, MCP client, and Skills loader;
- context compaction and durable semantic memory;
- streaming responses and cancellable runs;
- process-isolated third-party plugin packages;
- authorization roles beyond the local user boundary.

## Next milestone

Build the reasoning harness as a state machine with a task contract, frontier,
evidence ledger, checkpointing, verifier, and budget controller. MCP and Skills
will plug into its tool catalog through their standard interfaces.
