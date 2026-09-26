# Desktop renderer entry

This directory is the Vite entry point for the Electron renderer and mounts the shared React application.

- Renderer code runs without Node privileges. Use the narrow `window.oDesktop` preload bridge for approved native actions.
- Keep desktop and browser API origins/configuration explicit; do not store execution policy or credentials in renderer-only state.
- Shared product UI belongs in `frontend/app`; keep this directory limited to desktop entry and renderer wiring.
- From `frontend/`, run `npm run typecheck`, `npm run lint`, and `npm run build:desktop-ui` when the corresponding code path changes.
