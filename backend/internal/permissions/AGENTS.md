# Permission policy rules

This package evaluates session permission profiles against host-owned effect
classifications before Agent tools execute.

- Keep tool/effect classification in the host; plugin and MCP metadata cannot
  grant trust or widen a session profile.
- Unknown effects fail closed by asking or denying according to policy; never
  silently treat an unclassified mutation as read-only.
- Keep decisions deterministic and side-effect free. Approval persistence and
  turn suspension belong to the Agent and storage layers.
- Update the policy matrix and API/UI descriptions whenever profile behavior
  changes. Review shell isolation separately from user approval.
