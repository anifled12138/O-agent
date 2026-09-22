# Agent runtime rules

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
- Remove empty lifecycle hooks and placeholder branches; they create the false
  impression that a subsystem is wired into the loop.

