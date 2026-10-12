# Conversation UI and execution routing

The shared web and desktop renderer keeps execution destinations at the top of
the sidebar. Mobile composition uses four controls: attachments/plugins,
permissions, model, and send/stop. Model selection uses the selected computer's
reported catalog for local tasks. Assistant text has 20px side gutters, 16px
body text, and a 1.75 line height on phones.

## Destination and synchronization

- Choosing a destination prepares the next submission. It does not start a task.
  The destination and computer/model IDs are sent through the existing task API;
  execution configuration is never represented by a localStorage preference.
- Each selection and submission reads durable task and device state. An offline,
  revoked, or full selected computer never falls back to another online computer.
- The backend local-task API persists a conversation snapshot, the project
  workspace delta and input artifact manifests before the task becomes claimable.
  The worker verifies and prepares those inputs before executing.
- Running tasks, unknown states and reconciliation states prevent new dispatch.
  An archived local transcript also prevents continuing its old source
  conversation. These guards reconstruct from the task API after page reload.
- The sidebar opens the relevant task at the top of the task center. Local tasks
  use the existing safe-pause, artifact verification and continuation endpoints.
  External effects still require the existing review. Failure keeps the original
  destination and draft and leaves the review available.
- Continuation reads back both the new task and the imported conversation,
  verifies their relationship, opens the continuation branch, and observes the
  cloud task. The backend preserves parent lineage and imports checkpoints where
  available. Switching to another computer requires this cloud continuation to
  finish before submitting the next local task.
- A current server turn must finish before selecting another destination. Live
  processes, credentials and login state are not transported by this UI.

## Verification

From frontend/, run npm run typecheck, npm run lint, npm run test:web,
npm run build and npm run build:desktop-ui.

The standalone browser regression is tests/ui-execution.browser.mjs. Start a
local renderer preview, set O_UI_TEST_URL if it is not http://127.0.0.1:5187,
and run node tests/ui-execution.browser.mjs. It needs Playwright (or an absolute
O_PLAYWRIGHT_MODULE path) and optionally O_CHROME_PATH. It intercepts all API
requests and executes no real tasks. O_UI_SCREENSHOTS controls screenshot output.

It covers phone widths, landscape, enlarged text, menus, device models, offline
dispatch protection, restart reconstruction, failed-switch rollback, failed
handoff retry and observation of the imported conversation/continuation task.
Real multi-device execution and device authentication need an integration run
against connected nodes.
