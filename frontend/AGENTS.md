# Frontend rules

- Do not display success after a caught mutation error. Surface the error and
  keep the editor open.
- Local optimistic updates must be rolled back or replaced by backend read-back.
- localStorage may hold presentation preferences, drafts, and caches only. Any
  value that affects backend execution must be sent to and returned by the API.
- Labels such as enabled, active, completed, installed, and connected must map
  to explicit backend states rather than inferred UI state.

