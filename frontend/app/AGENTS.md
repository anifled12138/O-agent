# Application UI rules

This directory contains the shared React screens, API client, conversation UI, settings, plugin center, and Evolution Lab.

- Develop from `frontend/`; run `npm run typecheck` and `npm run lint`. Use `npm run build` for browser delivery or `npm run build:desktop-ui` for the Electron renderer.
- API types and UI forms must match backend request/response contracts. For mutations, keep editors open on failure and reload/verify the authoritative state.
- Keep drafts and presentation preferences local; backend execution policy and configuration belong to backend APIs.

- Provider/model forms must round-trip every execution-affecting field through
  the Provider API. Prefer provider IDs over model-name keys because multiple
  providers may expose the same model.
- Plugin toggles must reload authoritative state and treat a mismatched read-back
  value as an error.
- Partial agent turns need a distinct visual state and message; never render
  them as completed merely because a summary message exists.
- Empty `catch` blocks are prohibited for user-triggered mutations.
- Plugin installation and rollback forms must submit the selected immutable
  release ID and explicit permission consent; never authorize a moving `latest`
  release in the server action.
- The observability panel shows bounded aggregates only. Do not expose raw tool
  arguments, credentials, or unredacted provider payloads there.
