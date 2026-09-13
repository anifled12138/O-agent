# Plugin Runtime Implementation

This document describes the implemented `axiom.plugin/v2` architecture. The
normative design and trade-offs remain in ADR 0002.

## Package and build plane

Plugin Forge generates one of five shapes: hybrid, Agent Tool, UI-only,
backend Service, or lazy Skill. Each project is a separate Git repository.
The build planner validates only declared surfaces, produces a deterministic
artifact digest, and writes an immutable content-addressed bundle. V1 bundles
remain readable through an in-memory adapter; Forge no longer generates V1.

Build validation includes strict manifest decoding, Go import and OS API
policy, path and symlink containment, UI asset graph checks, strict sandbox
compatibility, Skill entry validation, tests, and output packaging.

## Runtime and activation plane

Runtime, Service, UI, Tool, Skill, Hook, and Job surfaces have separate
registries. Activation prepares a candidate sidecar, validates every export,
then swaps all registries under one lock and advances a registry epoch. The
old sidecar drains pinned calls before stopping. Installation and per-surface
state are persisted transactionally; persistence failure restores the prior
mount.

UI-only and Skill-only plugins mount without a process. Backend crashes revoke
dependent Tool, Service, Hook, and Job registrations, retain independent UI
and Skill surfaces, persist failed health, and emit an audit event. Active
surface state is reconstructed after Host restart.

## Presentation plane

Sandboxed plugin UI uses `axiom.ui.call`. It resolves the UI principal and its
mounted backend directly; it never traverses the Agent Tool registry. Assets
are release-bound, path-contained, CSP-protected, and served only for the
user's active installation. Plugin Center displays desired release, observed
surfaces, principal authority, registry epoch, health, permission hash, and
release history.

UI-only plugins can call a backend Service through the dedicated
`/api/v2/ui/plugins/{plugin}/services/{service}/call` bridge. The runtime
requires the calling release to declare the exact service ID and contract,
pins the provider process for the call, and passes an explicit `ui` principal.
Services remain absent from Agent discovery and cannot be reached by merely
knowing their IDs.

## Agent capability plane

No plugin catalog is injected into the system prompt. Each Agent turn receives
a registry lease that pins the exact release processes and Skill bundles. The
provider initially sees only:

1. `axiom_capability_search`, returning compact cards without schemas;
2. `axiom_capability_load`, loading one exact Tool schema or Skill body.

Loaded tools receive stable per-release function names and exact input schemas.
Unloaded invocation is denied. Plugin Forge self-bootstrapping actions use the
same resolver with `creator-only` visibility and cannot approve grants. A
bounded context builder retains recent persisted messages, and durable trace
events record model and tool boundaries without storing raw tool arguments.

## Agent authoring loop

Generated plugin source is not injected into normal Agent context. Creator
actions expose a lazy authoring protocol instead:

1. inspect a compact source tree containing paths, sizes, and SHA-256 hashes;
2. read only the files needed for the current change;
3. apply a Git unified diff against an explicit expected revision;
4. build and test the immutable candidate;
5. use structured build failure text to repeat the inspect/patch/build loop;
6. request user approval, then install only after the user grants it.

Each accepted file write or patch is a Git commit in the plugin's own
repository. Patch validation rejects deletion, rename, binary data, file-mode
changes, symlinks, paths outside the declared source contract, stale revisions,
files over 512 KiB, and patches over 1 MiB. The latest bounded commit diff can
be inspected without loading the entire project. The Host repository is never
an authoring target, and an active release remains mounted while a new revision
is developed. Per-project source operations are serialized, and build or edit
refuses uncommitted external source changes so every packaged byte remains
traceable to a commit; ephemeral `build/` output is excluded.

## Resource and process hardening

New V2 sidecars use `axiom.rpc/v2` and request filesystem, network, secret, or
trusted process operations from the Host over the same framed RPC channel.
The resource broker enforces the release grant, scope-contained and
symlink-resolved paths, payload limits, HTTPS host allowlists, DNS private-IP
rejection, exact secret names, and a trusted Host command registry. Generated
reference plugins scan workspaces only through `host.fs.list` and
`host.fs.read`; direct filesystem traversal is rejected by the V2 source
policy.

On Windows, each sidecar is assigned to a Job Object with kill-on-close, one
active process, and a 512 MiB process memory limit. RPC input, frame, stderr,
directory, file, and HTTP response sizes are bounded. Static source policy,
brokers, and Job Objects are defense in depth; a future production installer
may additionally launch third-party binaries under AppContainer or a dedicated
restricted identity.

## Compatibility and retirement

`GET /api/v1/plugin-runtime/migration` reports active V1 installations. V1
package compatibility remains available for existing immutable bundles while
the telemetry separately reports when legacy routes are safe to retire. Forge
cannot create new V1 releases. No immutable V1 bundle is rewritten or removed,
and no destructive database migration is used.

## Verification

Automated matrices cover manifest shapes, visibility, context compaction,
registry collision, concurrent UI activation/deactivation, resource traversal,
network address policy, secret/process grants, and strict UI content. E2E runs
cover all five build/install shapes, restart reconstruction, separated UI RPC,
lazy search/load/invoke traces, Windows containment, backend crash
reconciliation, and brokered V2 workspace scanning.
