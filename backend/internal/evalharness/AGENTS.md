# Agent evaluation harness

This package runs paired Baseline/Candidate trials, persists trial evidence, computes deterministic reports, and gates promotion recommendations.

- A trial that errored, was cancelled, or reached a step/time limit is not a successful complete result, even if its summary matches an evaluator.
- Require complete, correctly paired evidence before issuing a promotion recommendation. Record infrastructure failures separately from task failures.
- Experiments are currently process-owned asynchronous work; do not claim restart-resume until startup reconciliation is implemented.
- Keep evaluation capabilities read-only and align API evaluator limits with runtime behavior.
- From `backend/`, run `go test ./internal/evalharness` and `go test ./...` for changes.
