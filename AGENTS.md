# Axiom engineering rules

These instructions apply to the whole repository. More specific `AGENTS.md`
files may add directory-local rules.

## Semantic completion

- Never equate an HTTP 2xx, a non-nil object, or an in-memory assignment with
  successful completion. Success means the user-visible effect is durable and
  the runtime that consumes it has observed the new state.
- Do not return `ok`, `completed`, `enabled`, `installed`, `saved`, or similar
  labels until the final authoritative state has been read back or otherwise
  verified.
- If work stops because of a budget or recoverable boundary, report an explicit
  non-complete terminal state. A summary of partial work is not completion.
- Never swallow an error on a state-changing path. Roll back, return the error,
  or record an explicit degraded/error state.

## One source of truth

- Configuration shown in the UI must be persisted by the backend and consumed
  by the execution path. Browser-only state is allowed only for presentation.
- Avoid parallel booleans for configured, enabled, running, and healthy. When
  they are genuinely different, expose all relevant states and name them
  precisely.
- Defaults, validation limits, runtime limits, UI copy, and tests must agree.

## Verification

- Tests for mutations must assert the downstream effect, not just the response.
- Add a restart/persistence test for durable settings and a failure/rollback
  test for multi-step state changes.
- Tool and API descriptions are contracts. Update them in the same change as
  behavior, especially around lossiness, permissions, limits, and completion.

