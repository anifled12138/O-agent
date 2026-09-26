# Electron desktop shell

This directory contains the Electron main process and preload bridge that launch the bundled UI and managed Go Host.

- Keep `contextIsolation` and renderer sandboxing enabled; expose only narrow, validated IPC methods through preload.
- Bind the Host to loopback, own its process lifetime, and preserve useful startup/exit diagnostics without logging secrets.
- Packaged runtime assets are generated. Make source changes in `main.cjs`, `preload.cjs`, or build scripts rather than generated runtime output.
- Use `npm --prefix desktop run dev` for development. Use root packaging commands only for the requested UI/backend/package target; see `scripts/AGENTS.md` for release rules.
