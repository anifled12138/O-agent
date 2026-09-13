# Desktop Client Implementation

## Outcome

O is packaged as a local Windows application rather than a browser bookmark.
The user starts `O.exe`; the Electron Main process owns the UI and a private Go
Host process for the entire application lifetime. A development Web UI remains
available, but it is no longer the delivery boundary.

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

`npm run build:desktop` performs a reproducible pipeline:

1. build the dedicated static desktop renderer;
2. build the Go Host with `CGO_ENABLED=0`, `-trimpath`, and stripped symbols;
3. package the current platform with Electron Packager;
4. copy only the renderer artifacts and Host executable into the runtime area;
5. on Windows, create a Squirrel installer.

Every build uses a version-and-timestamp output directory under `release/`, so
an earlier package is not overwritten. The current milestone is unsigned; code
signing and trusted update metadata are required before public distribution.
