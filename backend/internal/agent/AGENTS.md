# Agent runtime rules

This package owns conversation turns, context construction, capability scopes, the model/tool loop, cancellation, and durable run traces.

- Develop from `backend/`; use `go test ./internal/agent` for focused changes and `go test ./...` for cross-package changes.
- A completed turn requires a normal final answer. Limits, cancellation, and runtime failures need distinct terminal states and stop reasons.

## Observability

- Durable turn events are the per-run forensic record; operational logs and aggregate metrics serve different purposes and must not replace that record.
- Correlate runtime diagnostics with turn, step, tool-call, provider, and generation IDs. Record durations and safe error classes.
- Do not copy prompts, credentials, or tool payloads into operational logs. Detailed trace payload capture is controlled by Run Inspector, currently enabled by default; preserve redaction and size bounds, and treat changes to its default/retention as a privacy decision.
- A failed tool response must remain visible as a failed tool outcome even when the Agent continues and returns a final answer.

- A turn is `completed` only when the agent produced a normal final answer.
  Step, token, time, approval, or context limits must use a distinct terminal
  status and stop reason.
- Every model invocation counts toward metrics, including fallback summaries and
  failed calls when usage is available.
- Context compaction is lossy unless a provider guarantees otherwise. Preserve
  the current goal and recent complete tool-call/result pairs, emit before/after
  metrics, and never claim 100% retention.
- Agent tools must describe their real authority. Do not advertise a user-only
  approval boundary if the tool can grant or bypass it.
- Every model tool call must pass through the host-owned authorization classifier
  before `turnScope.execute`; unknown tools default to ask or deny. When adding a
  tool, classify its effect and resource before exposing its schema.
- Approval requests bind the exact arguments and immutable plugin release shown
  to the user. Do not resolve an approval by looking up a moving `latest` target.
- Treat tool metrics as evidence for investigation and proposals. They must not
  mutate tool code, grants, or stable plugin releases automatically.
- Remove empty lifecycle hooks and placeholder branches; they create the false
  impression that a subsystem is wired into the loop.
