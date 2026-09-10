# ADR 0004: Atomic Full-Stack Plugin Hot Swap

Status: Proposed  
Date: 2026-09-10

## Context

An Axiom plugin release may contribute any combination of:

- an out-of-process backend;
- Host services consumed by another plugin;
- Agent tools and lazy Skills;
- browser UI mounted into one or more Host slots;
- event hooks and background jobs.

Hot plug cannot mean replacing code inside the Go process. Go plugins cannot be
reliably unloaded and are not the Axiom extension boundary. Axiom hot swap is a
transactional switch between two immutable plugin releases. The old release
remains available while work already bound to it drains.

The current V2 runtime already starts a candidate sidecar before publishing it,
switches registries under one lock and registry epoch, pins Agent turn calls,
and drains the previous process. The remaining full-stack gap is release skew:
an iframe from release A can currently reach a backend selected only by plugin
ID after release B becomes active. The Plugin Center also combines the latest
project manifest with the active installation release when rendering a preview.

## Decision

### 1. Release switching is the only hot-swap primitive

Every built artifact is immutable and addressed by release ID plus digest.
Activation never edits an active bundle. An update creates release B alongside
active release A and asks the Activation Coordinator to switch the installation.

```text
release A: active -------------------------- draining ---- retired
release B:          preparing ---- ready --- active ---------------->
                                             ^
                                      one commit point
```

`retired` means unavailable for new routing. The bundle remains retained while
an installation, active lease, durable job, rollback pointer, or audit record
references it.

### 2. One release owns all of its surfaces

UI, backend, Tool, Skill, Service, Hook, and Job definitions are published as
one `MountedRelease`. The runtime must never construct a public view by mixing
surfaces from different releases of the same plugin.

```go
type MountedRelease struct {
    UserID      string
    PluginID    string
    ReleaseID   string
    Digest      string
    Epoch       uint64
    Permissions PermissionGrantID
    Surfaces    SurfaceSet
    Backend     *ProcessLease
}
```

The public routing table is an immutable snapshot. Readers acquire a lease to
one snapshot without holding the activation lock. A commit replaces the current
snapshot pointer once; it does not update registries one at a time in a way that
readers can observe.

### 3. Prepare every required surface before commit

Activation is scoped to `(userID, pluginID)` and serialized by a keyed lock.
Preparation performs no public registration.

1. Load and verify the immutable release and digest.
2. Resolve dependency contracts against one candidate registry snapshot.
3. Compare the release permission hash with the current grant.
4. Start the candidate backend in a new contained process when present.
5. Complete protocol handshake and `plugin.health` readiness checks.
6. Discover and validate exported Service and Tool contracts.
7. Validate Skill resources and static UI asset graph.
8. Preload each UI entry in a hidden sandboxed iframe and wait for a bounded
   release-bound readiness handshake.
9. Register Hooks and Jobs in a private prepared set without admitting events.
10. Produce an immutable `PreparedMount` or abort every prepared resource.

An update that expands permissions stops before step 4 and waits for user
approval while the previous release remains active. Equal or reduced authority
may continue according to user policy.

### 4. Use a durable activation journal

Memory and SQLite cannot be committed atomically. The coordinator therefore
records a recoverable state machine:

```text
requested -> preparing -> ready -> committing -> active -> draining -> settled
                    \-> failed        \-> recovery_required
```

Before publishing release B, SQLite records the desired release, previous
release, proposed epoch, grant, and transaction ID as `committing`. The Host
then swaps the in-memory routing snapshot and marks the transaction `active`.

Recovery rules are deterministic:

- `preparing` or `ready` after restart: discard private resources and retry;
- `committing`: rebuild B, verify it, publish the recorded epoch, then mark
  active; if B cannot be rebuilt, restore A and record failure;
- `active` with missing runtime resources: reconstruct the exact release;
- `draining`: make the release unavailable to new calls and finish bounded
  cleanup.

No database row is treated as proof that a process or iframe is alive. Persisted
state is desired state; observed surface health is reconciled separately.

### 5. Pin every call to release and epoch

All callers acquire a `SurfaceLease` before dispatch:

```go
type SurfaceLease struct {
    PluginID  string
    ReleaseID string
    Epoch     uint64
    SurfaceID string
    Principal Principal
}
```

- Agent Tools are pinned for the current Agent turn.
- Host Service calls are pinned for the duration of the call.
- UI instances are pinned for the lifetime of that iframe instance.
- Hook delivery is pinned when the event is admitted.
- Durable Job records persist release ID and contract version.

New leases resolve to B after commit. Existing A leases remain callable while A
drains. A call may never silently fall forward from A to B just because both
releases have the same plugin ID.

### 6. Bind frontend RPC to an exact UI instance

The Host creates a `UIInstance` for one slot and one release:

```text
UIInstanceID + userID + pluginID + releaseID + epoch + slotID + nonce
```

The iframe does not choose these values. The Host transfers a `MessagePort`
after the iframe sends the expected nonce in its readiness handshake. All later
RPC uses that port; wildcard `postMessage` is not the authority boundary.

Each request carries only an operation ID and input. The Host derives the caller
principal, release, epoch, allowed Service dependencies, and response channel
from the `UIInstance`. Requests from a disposed or draining instance fail with
`UI_INSTANCE_RETIRED`; they are never rerouted to the new backend.

### 7. Swap UI with double buffering

For a UI-bearing update:

1. Keep iframe A visible and functional.
2. Load iframe B hidden from release-bound asset URLs.
3. Complete B's readiness and optional state-import handshake.
4. Commit the Host routing snapshot.
5. Atomically replace the slot's visible instance with B.
6. Mark A draining, settle its pending RPC, and dispose it after the deadline.

UI state is split into two classes:

- durable domain state belongs to Host-brokered plugin storage and is not owned
  by an iframe;
- ephemeral view state may be transferred through optional versioned
  `ui.exportState` / `ui.importState` contracts with a strict size limit.

State transfer failure does not block activation unless the manifest declares
it required. A plugin must remain correct when its ephemeral UI state is lost.

The Host application shell is not a hot-swappable plugin target. UI plugins use
declared slots and isolated documents. A release that patches Host React source
is a core application update, not a plugin update.

### 8. Keep frontend and backend compatibility explicit

A full-stack release normally ships its matching UI and backend together. UI
RPC declares versioned contracts, for example:

```json
{
  "service": "inventory.query",
  "contract": "inventory.query/v2"
}
```

Preparation rejects a release when its UI dependencies cannot be satisfied by
the candidate surface set. Cross-plugin Service dependencies declare an exact
contract or compatible range. Breaking a contract requires a coordinated
activation plan; the runtime does not guess compatibility from matching method
names.

### 9. Separate code switch from data migration

Plugin code is immutable; plugin data is Host-owned and release-independent.
Every release declares a data schema version and optional migration steps.

Hot-safe migrations follow expand/migrate/contract:

1. `expand`: add data understood by both A and B;
2. start B and migrate/copy data idempotently;
3. switch traffic to B;
4. retain A-compatible data through the rollback window;
5. `contract`: remove old representation only in a later explicit maintenance
   operation after rollback is no longer required.

An irreversible migration cannot be advertised as hot-swappable. It requires a
maintenance transition, backup/snapshot, and explicit user approval. Runtime
rollback changes code routing; it does not pretend to reverse external effects
or destructive third-party API calls.

### 10. Define surface-specific drain semantics

| Surface | Stop admitting | Drain behavior |
| --- | --- | --- |
| Backend Tool/Service | routing snapshot commit | finish pinned calls, then `plugin.shutdown` |
| UI | slot switch | finish pending MessagePort RPC, then dispose iframe |
| Skill | new turn snapshot | existing model context keeps its pinned release provenance |
| Hook | event admission | admitted handler finishes under old lease |
| Job | scheduler claim | claimed job keeps recorded release; unclaimed jobs use migration policy |
| MCP connection | tool registry commit | finish calls, close transport after deadline |

Drain deadlines are bounded. Timeout causes containment termination and an audit
event. Force termination does not convert an unknown external side effect into
a failed/no-op result.

### 11. Rollback is another activation

Rollback does not mutate B into A. It prepares retained release A and commits a
new epoch that routes new work to A. B then drains normally.

Automatic rollback is allowed only when:

- B fails readiness immediately after commit or crashes within a configured
  stabilization window;
- the permission grant for A remains valid;
- no incompatible data migration crossed its rollback barrier;
- A's required dependencies remain available.

Otherwise the installation enters `degraded` and asks for an explicit recovery
decision while independent surfaces may remain available.

### 12. Deactivation is recoverable hot unplug

Deactivation commits a routing snapshot with the plugin's public surfaces
removed, then drains all existing leases. It records the installation as
inactive but retains immutable releases and plugin data. Physical deletion is a
separate lifecycle operation and is not required for hot unplug.

## Failure behavior

- Candidate failure before commit leaves A untouched.
- UI readiness failure aborts B even when its backend is healthy if UI is a
  required surface of the release.
- Backend crash before commit aborts B.
- Backend crash after commit revokes only dependent B surfaces, records observed
  health, and evaluates the rollback barrier.
- Host crash at any activation state is resolved from the activation journal.
- Dependency disappearance prevents new leases and moves dependent surfaces to
  degraded; it never silently binds to an incompatible provider.

## Required implementation changes

1. Resolve Plugin Center preview metadata from `installation.activeReleaseId`,
   not `project.latestRelease`.
2. Replace plugin-ID-only UI RPC with release-bound `UIInstance` sessions.
3. Add hidden iframe readiness and `MessageChannel` transport.
4. Replace mutable per-registry publication with one immutable routing snapshot
   pointer and per-surface leases.
5. Add the durable activation transaction journal and keyed coordinator lock.
6. Persist release IDs for Hook and Job admission.
7. Add plugin data schema and migration contracts.
8. Add stabilization-window rollback and rollback-barrier reporting.

## Verification matrix

- backend B fails before ready: A receives all new calls;
- UI B fails handshake: A remains visible and uses backend A;
- update during Agent Tool call: old call completes on A, next turn uses B;
- update during UI RPC: old iframe finishes against A, new iframe calls B;
- rollback after update: UI, backend, Tool, Skill, Hook, and Job surfaces all
  resolve to A under one new epoch;
- Host crash in every activation journal state converges to A or B without a
  mixed surface set;
- incompatible UI/backend contract blocks before commit;
- permission expansion keeps A active while approval is pending;
- required migration failure leaves A active;
- drain timeout kills only the retired contained process;
- pure UI, pure backend, Skill-only, and Service-only releases follow the same
  transaction while skipping absent preparation steps.

## Consequences

Full-stack plugins can update without stopping the Host and without exposing a
mixed frontend/backend version. The cost is explicit release leases, a durable
coordinator, double-buffered UI, and strict data migration rules. This cost is
necessary: hot swap without version pinning is only fast replacement and cannot
provide consistent behavior under concurrent calls.

