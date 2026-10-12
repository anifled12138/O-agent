# Desktop Client Implementation

## Outcome

O is packaged as a local Windows application rather than a browser bookmark.
The user starts `O.exe`; the Electron Main process owns the UI and a private Go
Host process for the entire application lifetime. A development Web UI remains
available, but it is no longer the delivery boundary.

The packaged renderer uses exactly `oapp://app`. The backend accepts that
origin only for the local execution role bound to loopback, without requiring
public HTTPS or an authentication bootstrap token. Public Web and cloud hosts
retain their HTTPS and authentication requirements; other custom origins are
rejected. The Windows package smoke uses this real renderer origin for API
requests and verifies remote polling and durable conversation read-back after
restart.

## Process topology

```text
O.exe (Electron Main)
├── sandboxed Chromium Renderer
│   └── packaged React bundle from oapp://app
└── o-host.exe (Go)
    ├── local API + SSE on a random 127.0.0.1 port
    ├── Agent runtime
    └── isolated plugin sidecars
```

Electron was selected for this milestone because the existing React client can
be reused without moving product logic into a second UI implementation. The Go
Host remains an independent executable and the authoritative product runtime;
Electron is a lifecycle and presentation boundary, not a replacement backend.

## Startup and shutdown

1. Main acquires a single-instance lock.
2. Main chooses an available loopback port.
3. In development it builds `o-host.exe` and starts a dedicated Vite renderer;
   in production it uses both artifacts from the application package.
4. Main passes the exact renderer origin, data directory, workspace root, and
   loopback address to the Host.
5. Main waits for `/api/v1/health` before creating the window.
6. Preload exposes the validated runtime origin to the Renderer.
7. On application exit Main closes the Host stdin channel. The Host converts
   EOF into context cancellation and runs its normal HTTP/plugin shutdown path.

An installed application stores durable product data under Electron's stable
per-user application data directory. Development mode and portable builds run
directly from this repository deliberately reuse `D:\agent-harness\data`, so
existing provider configuration and conversations remain available. Host and
renderer logs are kept under the application's `logs` directory.

## Renderer security

- Production UI is served from the privileged custom `oapp://app` scheme, not
  from `file://` or a remote website.
- Node integration is disabled; context isolation, Chromium sandboxing, and Web
  security are enabled explicitly.
- Preload exports immutable metadata only. It does not expose `ipcRenderer`,
  filesystem, shell, process execution, or arbitrary message channels.
- Browser permissions are denied. New windows and unexpected top-level
  navigation are denied.
- A restrictive CSP allows only packaged scripts/styles plus loopback API,
  image, and plugin iframe traffic.
- The Go Host accepts only the exact renderer origin selected by Main.
- Plugin iframe permissions and release-bound assets remain enforced by the
  existing Host plugin runtime.

These controls follow Electron's current recommendations for context isolation,
sandboxing, navigation restriction, and local packaged content.

## Build and distribution

`npm run build:desktop` and the full `npm run package` delivery target use the
same strict pipeline in `desktop/scripts/package.mjs`. UI-only, backend-only and
explicit fast/portable-only targets remain available for development.

The release pipeline builds the dedicated static renderer, the Go Host and both
Windows sandbox helpers, then packages Electron. On Windows both the Squirrel
installer and portable ZIP are required; failures propagate rather than being
reported as a successful release. Electron downloads retain upstream checksum
verification. Packaged executables are compared with their compiled inputs, the
ZIP is checked for corruption and required assets, and each distribution file
gets a SHA-256 checksum. `BUILD-INFO` records the source commit, application
version, platform and architecture in both the output directory and app runtime.

Every build uses a version-and-timestamp directory under `release/`, so earlier
packages are retained. The Windows package smoke test starts the bundled Go Host
with isolated data, verifies authenticated remote-node heartbeats and HTTP
polling against a local test control server, creates a conversation, restarts
the process, and reads that same conversation back. This does not constitute
acceptance against a deployed VPS or a real model provider.

Successful CI produces `o-agent-windows-desktop` and `o-agent-cloud-runtime`.
Version-tag releases test and build both platforms from the same commit and
publish the Windows installer/portable ZIP and Linux runtime together. Missing
packages, failed checksums or source mismatch block publication; release assets
are downloaded and checked again after publication.

The current Windows milestone is unsigned. Download from the project's own
release, verify the SHA-256 file, and quit O through its tray menu before
upgrading. Version 0.2.1 advances the installer version from previous 0.2.0
builds. The application data directory remains `%APPDATA%\O\data`; upgrading
program files must not replace that directory.

Local sign-in is optional. Open **账户与远程连接**, enter your self-hosted
server's HTTPS origin and a computer name, and sign in to that server in the
system browser. Confirm the displayed computer. The local client never needs
the account password: it exchanges a private device code for an encrypted,
revocable node credential and persists it before starting the remote worker.
The computer appears automatically in that account's device list. Only a
verified heartbeat and model/permission catalog read-back count as connected.
Restarting O restores the same device identity without adding another computer.

Remote Web can select that computer's actual model and permissions; selecting a
local computer does not require configuring a model on the cloud server. Cloud
conversation model references contain routing metadata, not API keys or a fake
cloud provider. A model removed locally is rejected before a new task is queued.
Cloud execution still requires a real model configured on the cloud server.

Logout stops remote work before reporting a disconnected state. If the server
is unreachable, credential revocation remains encrypted on disk and retries
after connectivity returns; the UI explicitly reports the pending revocation.
The server cannot be declared revoked until its acknowledgement is verified.
Local conversations and models remain usable without an account or a server.

Existing manual `O_NODE_CONTROL_URL` / node-credential startup configuration is
retained for legacy clients under advanced pairing. It is not needed for normal
account login and cannot run a second account worker alongside the legacy worker.
Both the local client and cloud backend must include this feature; updating
only the Windows program cannot add the cloud authorization endpoints.
