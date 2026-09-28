# Permission policy rules

This package applies the selected session profile uniformly before Agent tools
execute. Whether a tool is built in or provided by an enabled extension does
not change the profile rules.

- `read_only` permits reads and turn-local operations, and rejects mutations.
- `workspace_autonomous` permits ordinary operations without per-call prompts;
  ask only for host-classified destructive actions or executing/installing
  plugin code. Unknown extension calls are available once the extension is
  enabled.
- `fully_autonomous` skips per-call permission filtering, including for
  unknown effects. The OS sandbox and tool availability remain separate
  execution boundaries.
- `ask_on_sensitive` is a legacy persisted value and follows
  `workspace_autonomous` behavior.
- Keep decisions deterministic and side-effect free. Approval persistence and
  turn suspension belong to the Agent and storage layers.
- Update API/UI descriptions whenever profile behavior changes. Review shell
  isolation separately from user approval.
