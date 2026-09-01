# ADR 0002: Plugin surfaces and agent visibility

Status: Proposed

## Context

Axiom milestone 1 proved one full-stack plugin path: generate a Go sidecar and
an iframe UI, build an immutable release, approve its permissions, activate it,
invoke a model-facing capability, hot-swap a new release, and roll back.

That vertical slice intentionally assumed every plugin had all three parts:

1. a backend sidecar;
2. a frontend entry;
3. at least one model-callable capability.

The assumption is not valid for the product-level plugin system. A theme,
renderer, settings panel, storage provider, telemetry sink, policy guard, model
adapter, background job, Skill provider, and Agent tool have different callers
and different visibility requirements. Installing a plugin is a Host concern;
showing information to a model is a separate, explicit export.

The existing implementation also injects every active capability summary into
the system prompt on every turn. That creates cost and attention pressure as
the installation grows, and incorrectly lets installation topology shape the
model context.

## Decision

Axiom will separate the plugin system into four layers:

1. **Package plane** — source, build, immutable releases, signatures, grants,
   dependencies, installation, updates, rollback, and audit.
2. **Runtime plane** — backend processes, UI assets, services, hooks, jobs, and
   lifecycle coordination.
3. **Agent capability plane** — only model-facing Tools, Skills, and conditional
   context providers.
4. **Presentation plane** — browser slots, iframe bridges, tool result views,
   and user interactions that do not implicitly become Agent tools.

The Host knows every installed plugin. The Agent harness knows only the
registrations that affect an Agent. The model sees only the model-facing subset
selected for the current Agent and current turn.

The governing rule is:

> Plugins belong to the Host. Surfaces belong to their callers. Only Agent
> surfaces may enter model context.

## Terminology

- **Plugin Project**: editable source controlled by Plugin Forge in its own Git
  repository.
- **Plugin Package**: one immutable, content-addressed build output.
- **Release**: package identity plus manifest, verification report, and digest.
- **Installation**: a user's desired and observed release for one plugin ID.
- **Surface**: a contribution mounted for a specific caller, such as UI,
  Service, Tool, Skill, Hook, or Job.
- **Export**: one named contract provided by a surface.
- **Registry**: the Host-owned index and dispatcher for one export family.
- **Agent View**: the Agent-scoped set of eligible Tools, Skills, and context
  providers after policy filtering.
- **Turn Scope**: ephemeral model-visible capabilities loaded for one turn.
- **Principal**: the caller identity: `host`, `user`, `ui`, `agent`, or `job`.

## Architectural boundaries

```text
                         Package plane
                +---------------------------+
                | Forge / Store / Grants    |
                | Install / Update / Rollback|
                +-------------+-------------+
                              |
                    Activation Coordinator
                              |
        +---------------------+---------------------+
        |                     |                     |
 Backend Supervisor      UI Slot Registry     Host Registries
 Service Registry        UI Bridge            Hooks / Jobs
        |                     |                     |
        +---------------------+---------------------+
                              |
                    Agent Surface Registry
                  Tools / Skills / Context
                              |
                    Capability Resolver
                 search -> load -> project
                              |
                          Agent Loop
                              |
                            Model
```

No registry may infer model visibility from the fact that a plugin is active.

## Manifest V2

Manifest V2 makes every component optional and makes every export explicit.
At least one component or export must exist, but a plugin does not need a
backend, frontend, or Agent capability.

```json
{
  "specVersion": "axiom.plugin/v2",
  "id": "workspace.inspector",
  "name": "Workspace Inspector",
  "version": "1.2.0",
  "description": "Inspects workspace quality signals.",

  "runtime": {
    "backend": {
      "artifact": "backend/plugin.exe",
      "protocol": "axiom.rpc/v2",
      "shutdownMillis": 10000
    }
  },

  "ui": {
    "entry": "frontend/index.html",
    "assets": "frontend/**",
    "slots": ["workspace.main", "plugins.preview"],
    "sandbox": "strict"
  },

  "exports": {
    "services": [
      {
        "id": "workspace.inspector.query",
        "contract": "axiom.service/v1"
      }
    ],
    "tools": [
      {
        "id": "workspace.inspector.scan",
        "summary": "Scan the workspace for quality markers.",
        "tags": ["workspace", "quality", "scan"],
        "visibility": "discoverable",
        "risk": "workspace-read",
        "inputSchema": { "type": "object" },
        "outputSchema": { "type": "object" }
      }
    ],
    "skills": [],
    "hooks": [],
    "jobs": []
  },

  "dependencies": {
    "plugins": [],
    "services": []
  },

  "permissions": {
    "filesystem": {
      "read": ["${workspace}"],
      "write": ["${pluginData}"]
    },
    "network": [],
    "secrets": [],
    "process": false,
    "background": false
  },

  "upgrade": {
    "strategy": "drain",
    "pinActiveCalls": true,
    "stateVersion": 1
  }
}
```

### Component rules

- `runtime.backend` is optional. Its presence produces a managed sidecar.
- `ui` is optional. Its presence produces browser assets and slot registrations.
- `exports.services` requires a backend unless the service is declared as a
  Host-native provider.
- `exports.tools` requires an executor, which may be a sidecar service, a Host
  service, or a brokered remote adapter.
- `exports.skills` may be static package content and needs no backend.
- `exports.hooks` names only approved Host events; arbitrary event subscription
  is rejected.
- `exports.jobs` requires the background permission and a registered job
  handler.
- An empty `exports.tools` is valid and means the model cannot call this plugin.

### Model visibility

Agent exports declare one of:

- `none`: never exposed to a model; available only to trusted Host callers.
- `discoverable`: indexed by the capability resolver and loaded on demand.
- `always`: placed in every eligible Agent request. Reserved for a very small
  first-party core set.
- `creator-only`: visible only in an explicit Plugin Creator Agent preset.

Installation UI, service exports, hooks, and jobs have no model visibility
field because they are not model surfaces.

## Plugin shapes

The plugin type is inferred from its surfaces rather than declared as one
exclusive category.

| Shape | Components and exports | Model visibility |
| --- | --- | --- |
| Pure UI | `ui` | None |
| Pure backend service | backend + services | None |
| Policy/audit extension | hooks | None |
| Background worker | backend + jobs | None unless it also exports a Tool |
| Skill pack | skills | Summary is discoverable; body is lazy-loaded |
| Agent Tool | tool + executor | On demand or always |
| Full-stack feature | UI + backend services | None by default |
| Full-stack Agent feature | UI + backend + tools/skills | Only Agent exports |

## Registries

### PackageManager

Owns packages and installations. It has no model APIs.

```go
type PackageManager interface {
    Release(ctx context.Context, releaseID string) (Release, error)
    Installation(ctx context.Context, userID, pluginID string) (Installation, error)
    PlanActivation(ctx context.Context, userID, releaseID string) (ActivationPlan, error)
}
```

### BackendSupervisor

Starts, probes, drains, and stops sidecars. It does not register Agent tools.
One invocation pins one process and release until it completes.

```go
type BackendSupervisor interface {
    Prepare(ctx context.Context, release Release) (PreparedBackend, error)
    Commit(ctx context.Context, prepared PreparedBackend) (BackendLease, error)
    Drain(ctx context.Context, lease BackendLease) error
}
```

### ServiceRegistry

Registers Host-facing RPC contracts. UI, jobs, and Agent tool executors can all
consume services, but caller identity and grants are checked independently.

```go
type ServiceRegistry interface {
    Prepare(release Release, backend PreparedBackend) (PreparedServices, error)
    Resolve(principal Principal, serviceID string) (ServiceBinding, error)
}
```

### UISlotRegistry

Registers UI entries and slots. It never contributes prompt text or tool
schemas.

```go
type UISlotRegistry interface {
    Prepare(release Release) (PreparedUI, error)
    Slots(userID string) []UISlotBinding
}
```

### HookRegistry and JobRegistry

Hooks subscribe only to allowlisted typed events. Jobs run under a `job`
principal and explicit resource grants. Neither is model-visible unless a
separate Agent export controls it.

### AgentSurfaceRegistry

Holds Agent-facing cards and exact definitions. It is not a plugin list.

```go
type AgentSurfaceRegistry interface {
    Search(scope AgentScope, query string, limit int) []CapabilityCard
    Describe(scope AgentScope, capabilityID string) (CapabilityDescriptor, error)
    Execute(ctx context.Context, scope AgentScope, capabilityID string, input json.RawMessage) (json.RawMessage, error)
    Skill(scope AgentScope, name string) (SkillDefinition, error)
}
```

`CapabilityCard` contains only ID, summary, tags, risk, and export kind. It does
not expose source paths, package topology, sidecar details, or unrelated plugin
metadata.

## Activation Coordinator

Activation is a per-user, per-plugin transaction. Registries implement a
prepare/commit/abort contract so a partial release is never published.

```text
1. Lock installation key (userID, pluginID).
2. Load and validate the exact immutable release.
3. Verify dependency graph and permission grant hash.
4. Build an ActivationPlan from declared surfaces.
5. Prepare backend when present.
6. Health-check backend and exported service contracts.
7. Prepare UI, services, hooks, jobs, and Agent exports.
8. Commit all prepared registrations under one registry epoch.
9. Persist observed active release and epoch.
10. Stop routing new calls to the old epoch.
11. Drain old pinned calls and then stop old resources.
```

If steps 2–7 fail, every prepared resource is aborted and the old release stays
active. If persistence fails after registry commit, the coordinator restores
the previous registry epoch before returning failure. Startup reconciliation
compares persisted desired state with observed registry state and repairs it.

Frontend-only installation skips backend preparation. Backend-only installation
publishes no UI or Agent entries. Hybrid installation commits all declared
surfaces together.

## State model

The current single project state mixes authoring, approval, and runtime state.
V2 separates them.

### ProjectStage

```text
proposed -> generating -> generated -> building -> tested
                         \-> generation_failed
                                      \-> build_failed
```

### ReleaseStatus

```text
candidate -> verified -> awaiting_approval -> approved
                                      \-> rejected
```

### InstallationState

```text
inactive -> activating -> active -> draining -> inactive
                \-> failed         \-> failed
```

An installation stores both desired and observed release IDs. Building a new
project release never changes the current installation state.

### SurfaceInstanceState

Each mounted surface records `prepared`, `active`, `draining`, `failed`, and its
registry epoch. This is diagnostic state, not a second source of truth for the
installation.

## Agent capability resolution

The Agent service must depend on an `AgentCapabilityResolver`, not on Plugin
Forge or PackageManager.

### Always-visible core

The normal Agent starts with a bounded first-party set:

- `capability_search`
- `capability_load`
- `skill_search` or a compact Skill catalog
- `skill_load`
- essential collaboration controls such as user approval when enabled

Plugin authoring tools are not part of the normal set. They are supplied by a
`plugin-authoring` Skill or an explicit Creator preset.

### Lazy Tool flow

```text
User request
  -> model calls capability_search(intent)
  -> resolver returns at most five CapabilityCards
  -> model calls capability_load(id)
  -> policy checks Agent scope and permission preflight
  -> exact schema is added to TurnScope
  -> next model request contains only the loaded Tool schema
  -> normal Tool call executes through AgentSurfaceRegistry
  -> result and source metadata are appended to the run event log
```

The loaded Tool receives a stable model-facing name derived from the export ID.
The binding retains the real capability ID, release ID, permission hash, and
registry epoch outside the prompt. The model cannot select a different release
by editing arguments.

### TurnScope budgets

Initial defaults:

- maximum search results: 5;
- maximum loaded plugin Tools per turn: 4;
- maximum combined loaded Tool schema budget: 8,000 estimated tokens;
- maximum one Skill body before compaction: 64 KiB;
- search cards contain no examples or full JSON schemas;
- loaded definitions expire at turn completion unless the run policy pins them.

The resolver begins with deterministic lexical/tag search. A semantic index may
be added later without changing its contract. Search is over active eligible
Agent exports, never over all installed plugins.

### Context and trace requirements

Every model-visible injection is an append-only run event with a source:

- `capability/search-result`
- `capability/loaded`
- `skill/catalog`
- `skill/loaded`
- `tool/call`
- `tool/result`

The event stores release and registry epoch metadata outside model content.
Resume and replay reconstruct the same Agent View. Compaction may replace model
content with a summary but must retain source identity and evidence references.

## UI and Agent bridges

The existing iframe bridge conflates a UI action with an Agent capability call.
V2 defines separate protocols.

### UI bridge

```text
axiom.ui.call
```

- Caller principal is `ui`.
- Target must be a service/action explicitly allowed to that UI release.
- It does not create a model tool call or model-visible context.
- The Host binds the iframe's release and plugin identity; the iframe cannot
  choose another plugin identity in its message.

### Agent bridge

```text
axiom.agent.invoke
```

- Caller principal is `agent`.
- Target must be an active loaded Agent export.
- Input and output schemas are enforced.
- Permission guards, timeout, retry policy, and audit run before dispatch.

### User gestures

A user click may optionally submit a prompt or approve a request, but this must
be an explicit Host action. Rendering a UI plugin never injects context by
itself.

## Security and authority

Permission grants bind:

```text
user ID + release digest + canonical permission hash + exported surface set
```

Adding an Agent Tool, UI service access, Hook, Job, secret, filesystem scope, or
network target changes the grant hash and requires review.

Every call carries a Principal:

| Principal | Typical authority |
| --- | --- |
| `host` | Internal lifecycle and reconciliation only |
| `user` | Explicit approved UI/API action |
| `ui` | Release-bound service calls allowed by UI policy |
| `agent` | Loaded Agent exports within the current Agent scope |
| `job` | Declared background handler with job-specific grants |

Direct network, process creation, unrestricted filesystem access, and arbitrary
secret reads remain unavailable to third-party sidecars. They must use Host
brokers. Static import policy is defense in depth, not the final security
boundary. Windows production hardening will add Job Objects and AppContainer or
an equivalent restricted token boundary.

## Build and package planning

Build no longer assumes Go plus one HTML file. A `BuildPlanner` derives steps
from the manifest.

| Surface | Required verification |
| --- | --- |
| Backend | language tests, build, banned API policy, protocol probe |
| UI | asset graph, entry existence, CSP compatibility, bridge contract |
| Tool | schemas, executor binding, risk and visibility metadata |
| Skill | frontmatter, invocation policy, body/resource bounds |
| Hook | allowlisted event and handler contract |
| Job | handler contract, background permission, cancellation behavior |

The release digest covers the canonical manifest and every declared artifact
in deterministic path order. It must not hash only the executable and entry
HTML. Source files outside declared package artifacts do not enter the release.

## Storage model

Migrations are additive during the V1 compatibility period.

### Existing tables retained

- `plugin_projects`
- `plugin_releases`
- `plugin_installations`
- `plugin_grants`
- `plugin_audit_events`

### New tables

- `plugin_release_surfaces`
  - release ID, surface kind, export ID, descriptor JSON, descriptor digest
- `plugin_surface_instances`
  - user ID, plugin ID, release ID, surface kind, export ID, state, epoch, error
- `plugin_dependencies`
  - release ID, dependency kind, target, version constraint
- `plugin_activation_transactions`
  - transaction ID, desired release, previous release, phase, epoch, timestamps
- `agent_capability_index`
  - active export card fields and search revision; rebuildable projection
- `agent_run_events`
  - append-only model/tool/context events and source metadata

`agent_capability_index` is a projection and may be rebuilt from active surface
instances. It is never installation authority.

## API boundaries

Product APIs are separated by caller rather than exposing one generic plugin
namespace.

```text
/api/v2/plugins/*                 package and installation management
/api/v2/ui/slots                  active UI contributions
/api/v2/ui/call                   authenticated UI bridge
/api/v2/agent/capabilities/search Agent resolver transport if needed
/api/v2/agent/runs/*              run and trace state
/api/v2/plugin-assets/*           release-bound static assets
```

The Agent normally calls resolver services in-process. Agent HTTP routes exist
for diagnostics and remote harness clients, not as the internal source of truth.

## V1 compatibility

V1 bundles remain immutable and are adapted at load time:

```text
V1 backend       -> V2 runtime.backend
V1 frontend      -> V2 ui
V1 capabilities  -> V2 exports.tools with visibility=discoverable
V1 permissions   -> V2 canonical permission document
```

The adapter never rewrites a V1 release or changes its digest. Existing grants
remain valid for that exact V1 digest. A newly built package uses V2 and a new
grant hash.

Existing `/api/v1/plugin-runtime/capabilities` and iframe calls continue through
compatibility adapters until the frontend and reference plugin have migrated.
V1 creation is disabled after V2 Forge generation is stable; V1 activation is
removed only after no active V1 installations remain.

## Failure handling

- A backend crash marks its backend and dependent exports failed without
  pretending the installation is healthy.
- A UI asset failure does not silently expose a half-installed release.
- A Tool schema conflict rejects preparation before commit.
- A Hook or Job that fails registration aborts activation.
- A failed update leaves the old release and registry epoch active.
- A failed drain is logged and force-stopped after its bounded deadline; it does
  not roll back a successful new commit.
- Resolver search failure leaves always-visible core tools usable.
- Loading a capability that disappeared returns a structured stale-card error
  and asks the resolver to search again.

## Testing requirements

### Manifest matrix

- pure UI;
- pure backend service;
- Skill-only;
- Tool-only with Host executor;
- backend Tool;
- full-stack without Agent exports;
- full-stack with Agent exports;
- invalid empty package;
- invalid cross-surface executor reference.

### Activation matrix

- prepare failure at every surface;
- persistence failure after registry commit;
- concurrent activate/update/deactivate on one plugin;
- old-call pinning during update;
- process crash and startup reconciliation;
- frontend-only activation with no sidecar;
- rollback across different surface sets;
- permission expansion requiring a new grant.

### Agent visibility matrix

- pure UI and backend services never appear in search or prompt assembly;
- discoverable cards do not inject full schemas;
- `capability_load` adds only the selected exact schema;
- Agent scope restrictions remove search and execution authority;
- unloaded capability invocation is denied;
- loaded bindings pin release and registry epoch;
- Skill summary and Skill body have separate loading lifecycles;
- context compaction preserves event source and evidence identity.

### End-to-end acceptance

1. Install and render a pure frontend plugin; assert no sidecar and no Agent
   visibility.
2. Install a pure backend service used by that UI; assert no Agent visibility.
3. Install a Skill pack; assert summary-only discovery and lazy body load.
4. Install an Agent Tool; search, load, invoke, update during an active call,
   and verify release pinning.
5. Install a hybrid plugin; verify UI calls use `ui` authority and Agent calls
   use `agent` authority.
6. Restart the Host and reconstruct all active surfaces and Agent index.

## Implementation sequence

Each phase is independently testable and receives its own Git commit.

### Phase 1 — V2 domain and compatibility

- Add V2 manifest types and strict validation.
- Add V1-to-V2 in-memory adapter.
- Add canonical surface and permission digests.
- Add manifest matrix tests.
- Keep all current runtime behavior through the adapter.

### Phase 2 — Build planner and package store

- Replace unconditional Go/HTML build logic with surface-derived BuildPlan.
- Hash all declared artifacts deterministically.
- Generate V2 reference packages for pure UI, pure backend, Skill, Tool, and
  hybrid shapes.
- Keep V1 package reading intact.

### Phase 3 — Registries and activation transaction

- Extract BackendSupervisor from capability registration.
- Add Service, UI, Hook, Job, and AgentSurface registries.
- Add per-plugin Activation Coordinator and registry epochs.
- Split project, release, installation, and surface states.
- Migrate hot-swap, restore, deactivate, and rollback tests.

### Phase 4 — UI bridge and Plugin Center

- Add `/api/v2/ui/slots`, release-bound assets, and `axiom.ui.call`.
- Render frontend-only and hybrid plugins without assuming Agent tools.
- Show surfaces, principals, permission changes, desired/observed release, and
  per-surface health in Plugin Center.
- Retain V1 iframe compatibility.

### Phase 5 — Agent resolver and lazy visibility

- Replace the Agent's Plugin Forge dependency with AgentCapabilityResolver.
- Remove full active capability catalog injection.
- Add search/load and TurnScope budgets.
- Project only selected exact schemas into provider requests.
- Move Plugin Forge actions behind a creator-only Skill/preset.
- Persist model-visible capability and Skill events.

### Phase 6 — hardening and V1 retirement

- Add brokered filesystem, network, secret, and process APIs.
- Add Windows process containment.
- Add reconciliation, crash, concurrency, and adversarial tests.
- Stop generating V1 packages.
- Remove V1 routes only after migration telemetry reports zero active V1
  installations.

## Engineering constraints

- No plugin installation may patch Axiom Host source.
- Core upgrades and plugin packages remain separate product operations.
- No destructive migration; schema changes are additive until verified.
- Existing user data and immutable packages are preserved.
- Every phase must pass Go tests, vet, frontend lint/build, compatibility tests,
  and a clean Git diff check before commit.
- New packages depend on interfaces owned by their layer, not concrete providers.
- Model-visible content must be traceable to a durable source event.

## Consequences

### Positive

- Pure frontend and pure backend plugins become first-class.
- Plugin count no longer directly determines model context size.
- UI and Agent permissions are independent and auditable.
- Agent reasoning depends on capabilities rather than installation topology.
- Updates can change surface composition atomically.
- Skills, MCP tools, Host services, and native tools can share a resolver without
  sharing an implementation.

### Costs

- Activation becomes a multi-registry transaction.
- More explicit types and persisted state are required.
- Lazy loading adds at least one resolver step before first use.
- V1 and V2 must coexist during migration.
- Full security requires OS containment plus brokered resources, not only
  manifest checks.

## Non-goals

- Inventing replacements for MCP or the Skill file format.
- Allowing third-party packages to modify Host source code.
- Treating every Host service as a model tool.
- Solving semantic search quality in the first resolver implementation.
- Loading arbitrary native code into the Go process.
- Removing user approval from permission-expanding updates.

## Acceptance criteria for this ADR

This design is considered implemented when:

1. a pure UI plugin and pure backend plugin can install independently;
2. neither appears in model context or capability search without an Agent
   export;
3. a discoverable Tool is absent from the initial request and appears only
   after load;
4. UI and Agent calls use different principals and grants;
5. a hybrid release activates all surfaces atomically and rolls back as one
   release;
6. V1 installations continue to run during migration;
7. run replay can identify every capability/context injection by source,
   release, and registry epoch.
