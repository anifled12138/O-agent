# Application UI rules

- Provider/model forms must round-trip every execution-affecting field through
  the Provider API. Prefer provider IDs over model-name keys because multiple
  providers may expose the same model.
- Plugin toggles must reload authoritative state and treat a mismatched read-back
  value as an error.
- Partial agent turns need a distinct visual state and message; never render
  them as completed merely because a summary message exists.
- Empty `catch` blocks are prohibited for user-triggered mutations.

