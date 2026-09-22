# MCP lifecycle rules

- Distinguish configured, enabled, running, and healthy MCP servers.
- Never report an MCP server as enabled/active solely because its config says
  so; a live client and successful handshake are required for active status.
- Persist configuration errors must be returned. Start/stop failures must roll
  configuration and clients back to the previous consistent state.
- Startup restoration failures must remain visible in catalog status and logs.

