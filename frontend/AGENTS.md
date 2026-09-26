# Frontend rules

This tree contains the browser UI and Vite renderer entry used by the Electron desktop shell.

- Develop from `frontend/`; run `npm run typecheck` and `npm run lint` for focused UI changes, and `npm run build` or `npm run build:desktop-ui` for the affected delivery target.
- Shared product screens live in `app/`; desktop renderer bootstrapping and preload contracts live in `desktop/` and the Electron shell under the repository's `desktop/` directory.
- Do not edit generated `dist` or `dist-desktop` output by hand.

- Do not display success after a caught mutation error. Surface the error and
  keep the editor open.
- Local optimistic updates must be rolled back or replaced by backend read-back.
- localStorage may hold presentation preferences, drafts, and caches only. Any
  value that affects backend execution must be sent to and returned by the API.
- Labels such as enabled, active, completed, installed, and connected must map
  to explicit backend states rather than inferred UI state.
