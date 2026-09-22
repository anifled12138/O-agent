# Plugin Forge rules

- Keep the lifecycle as small as the product requires. Build/test and install
  are real boundaries; do not retain ceremonial states that can be bypassed.
- Installation is the authorization boundary for this local single-user app.
  Its description must state that it grants the release's declared permissions
  and activates the release.
- Activation and persistence must remain atomic with compensation on failure.
- State-machine transitions, UI actions, agent tools, and audit events must tell
  the same lifecycle story.

