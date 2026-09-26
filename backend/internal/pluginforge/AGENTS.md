# Plugin Forge rules

This package owns plugin projects, source revisions, builds/tests, immutable releases, grants, installations, and activation orchestration.

- Develop from `backend/`; use `go test ./internal/pluginforge` for lifecycle changes and `go test ./...` for runtime/repository integration.
- Build and install are real security boundaries. Preserve revision checks, immutable release digests, permission grants, and activation compensation.
- Never report installation or activation until the durable installation and mounted runtime have been read back.

- Keep the lifecycle as small as the product requires. Build/test and install
  are real boundaries; do not retain ceremonial states that can be bypassed.
- Installation is the authorization boundary for this local single-user app.
  Its description must state that it grants the release's declared permissions
  and activates the release.
- Activation and persistence must remain atomic with compensation on failure.
- State-machine transitions, UI actions, agent tools, and audit events must tell
  the same lifecycle story.
- Installation and permission approval must use an exact immutable release ID;
  legacy latest-release helpers must not silently select or grant a release.
- Storage usage reports cover verified content-addressed release bundles only.
  Valid release history is kept without an automatic count limit. Only a user
  confirmed unusable release may have its bundle deleted on the next startup;
  preserve release metadata and audit records, and keep cleanup idempotent.
