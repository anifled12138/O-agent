# Permission policy rules

This package applies the selected session profile uniformly before Agent tools
execute. Whether a tool is built in or provided by an enabled extension does
not change the profile rules.

- `read_only` permits reads and turn-local operations, and rejects mutations.
- `workspace_autonomous` permits ordinary workspace writes, process commands,
  and network access without per-call prompts; ask for host-classified
  destructive actions and executing/installing plugin code.
- `request_approval` asks before workspace writes and external access, in
  addition to the destructive and sensitive actions above.
- `fully_autonomous` skips per-call permission filtering, including for
  unknown effects. The OS sandbox and tool availability remain separate
  execution boundaries.
- The one-time startup migration moves existing `workspace_autonomous`
  conversations to `request_approval` and maps legacy `ask_on_sensitive`
  conversations to `workspace_autonomous`; new conversations default to
  `workspace_autonomous`.
- Keep decisions deterministic and side-effect free. Approval persistence and
  turn suspension belong to the Agent and storage layers.
- Update API/UI descriptions whenever profile behavior changes. Review shell
  isolation separately from user approval; command write/network flags must be
  enforced by the OS sandbox regardless of whether approval was required.
