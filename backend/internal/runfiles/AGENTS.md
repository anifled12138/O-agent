# Run file lifecycle rules

This package creates and cleans temporary workspaces scoped to an Agent turn.

- Keep generated scratch files outside the user's project workspace and bind
  every run directory to its owner and turn.
- Cleanup must be idempotent, report filesystem failures, and avoid deleting
  paths outside the managed root.
- Account for live child processes before removing a run directory; platform
  specific process checks belong in the `*_windows.go` and `*_other.go` files.
- Update lifecycle documentation and downstream tool descriptions when cleanup
  timing or persistence behavior changes.
