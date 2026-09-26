# Desktop packaging scripts

This directory contains the Electron packaging pipeline and icon/build helpers.

- Treat package output, downloaded Electron artifacts, and runtime binaries as generated files; do not hand-edit them.
- Preserve platform selection, checksums, required runtime assets, and explicit failure reporting in packaging steps.
- Follow the root `scripts/AGENTS.md` decision tree; routine source edits do not authorize a full release package.
- Validate script syntax with the relevant Node/Python command. Run a package build only when it is required by the requested delivery.
