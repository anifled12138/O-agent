# Ubuntu cloud runtime

This directory contains a staged deployment for the cloud-control-plane role. The Web application is a same-origin bridge: Caddy routes `/api/*` to the Go control plane and all other paths to the Node standalone server. The backend and read-only Web process use separate Linux accounts. The Web/PWA is only a control surface; tasks run in the Go control plane's cloud worker, paired local nodes, or a sandboxed Chromium worker.

## Release layout

CI publishes a single `o-agent-cloud-runtime` artifact for each successful run. Version-tag releases run the same checks against the exact tagged commit and attach `o-agent-cloud-runtime.tar.gz` and its `.sha256` file to GitHub Releases; automatic source archives are not installable runtime packages. A failed test or provenance check prevents runtime publication. Verify the `.sha256` file, then extract the package; it has this exact layout:

```bash
sha256sum -c o-agent-cloud-runtime.tar.gz.sha256
mkdir o-agent-release && tar -xzf o-agent-cloud-runtime.tar.gz -C o-agent-release
cd o-agent-release
```

```text
release/
  backend/axiom
  backend/axiom-sandbox-check
  backend/axiom-quota-helper
  frontend/standalone/server.js
  frontend/standalone/dist/
  frontend/standalone/public/
  deploy/ubuntu/
```

The frontend standalone build supplies `standalone/server.js`, its `dist/` and `public/` assets, and the minimal runtime dependencies under `standalone/`. Keep the exact backend and frontend outputs from the same commit. The installer does not fetch code, build on the 4 GiB VPS, overwrite an existing release, or start services.

## Stage a release

### Verify the VPS SSH identity before the first login

Obtain the VPS SSH host-key fingerprint from the provider console or another
independent trusted channel before sending credentials. On the VPS console,
read the Ed25519 host-key fingerprint with:

```bash
sudo ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256
```

From the operator machine, compare it with the fingerprint returned by:

```bash
ssh-keyscan -t ed25519 <vps-hostname-or-ip> 2>/dev/null | ssh-keygen -lf - -E sha256
```

Only add the key to `known_hosts` after the independent fingerprints match. Do
not disable host-key checking or trust a first-seen key solely because the
server is reachable. Prefer a dedicated SSH key for administration; rotate any
bootstrap password after key access is verified and disable password login
only after confirming the key-based session works.

On a dedicated Ubuntu 26.04 VPS, install Bubblewrap, Caddy, Node.js 22.13 or newer, Go-built release artifacts, and the standard systemd/logind tools using your organization's trusted package sources. To expose the Agent's optional `browser_session` tool, also install a trusted Chrome/Chromium build whose resolved executable is a regular executable under `/usr`; snap-backed browsers and binaries resolving under `/opt` are currently not visible to the Bubblewrap runtime. Browser sessions start only when that executable, Bubblewrap, `systemd-run`, and cgroup v2 are all available. Set `O_BROWSER_EXECUTABLE=/usr/lib/.../chromium` in `/etc/o-agent/cloud.env` only if the binary is not discoverable on `PATH`; do not set it to a wrapper script or symlink. Without a supported browser binary the rest of O continues to work and the browser tool stays unavailable.

The browser uses a throw-away profile under the sandbox's private `/tmp`, an exact public-host allow-list and cgroup IP filtering; it does not persist login cookies. One browser session runs per O host at a time. O reserves a 1.5 GiB memory envelope while it runs, so new cloud/local tasks are admitted against the reduced capacity and remain queued when resources are tight. Verify a real page open, screenshot, allowed navigation and blocked unlisted host on the target Ubuntu host before enabling browser-dependent workflows.

Review and run:

```bash
sudo bash deploy/ubuntu/install-runtime.sh . 20261002.1
```

Before creating accounts or staging a release, the installer requires `/etc/o-agent` to be a real directory and its `cloud.env`/`quota-helper.env` entries to be regular files; symlinks and other file types fail closed. The script creates dedicated `oagent` (backend/worker) and `oagent-web` (read-only Web) system accounts if needed, installs a versioned release under `/opt/o-agent/releases/`, prepares service units and user-manager cgroup delegation, enables backend user lingering, and updates the `current` symlink only after the release is copied. It deliberately leaves services stopped. It stages a separate root-owned quota-helper service; the helper exposes only three project-quota actions over `/run/o-agent/quota.sock`, authenticates the `oagent` UID with `SO_PEERCRED`, and restricts paths to generated task workspaces under `/var/lib/o-agent/workspaces`.

Review `/etc/o-agent/cloud.env`; set a random `O_AUTH_BOOTSTRAP_TOKEN` of at least 32 characters without putting it in shell history. Restrict it to root and the `oagent` group (`root:oagent`, mode `0640`). Replace the example `O_FRONTEND_ORIGIN` with the public HTTPS origin and use that hostname in Caddy's `Caddyfile.example`. The systemd unit reads the origin from this file so the configured domain is the value the backend actually consumes. Cloud role also needs `O_QUOTA_HELPER_SOCKET=/run/o-agent/quota.sock` and a positive `O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES` value; the installer writes these defaults only when creating a new `cloud.env`, so add them when upgrading an existing installation. `O_CLOUD_TASK_WORKSPACE_RETENTION` defaults to `720h` (30 days) for verified completed scratch workspaces; set it to `0` to stop starting new automatic cleanup. Any release already in progress will still resume after restart. Cleanup preserves durable artifacts and never removes Git worktrees. Cloud startup refuses to serve as a worker without authentication, HTTPS origin configuration, a reachable quota helper and supported project-quota mount. Restrict inbound firewall access to SSH and HTTPS. Provider keys are added after authenticated first-time setup and are stored in the encrypted provider vault.

Reserve Linux project IDs `0x80000000` through `0xffffffff` for O task workspaces; the installer records this requirement in the root-owned quota-helper environment file. Do not assign that range to other project-quota consumers on the same filesystem. Existing O workspace IDs are persisted and remain stable across retries and upgrades.

The service uses private, durable paths under `/var/lib/o-agent`; do not place credentials, SQLite state, or workspace data in the release symlink. The installer creates `/etc/o-agent/quota-helper.env` as `root:root` mode `0600`; review the allowed UID/GID and `O_QUOTA_MAX_BYTES` there. Cloud `cloud.env` defaults the per-task limit to 8 GiB and preserves 4 GiB of shared filesystem headroom through `O_CLOUD_TASK_DISK_RESERVE_BYTES`. Worker admission checks live available memory, CPU, and workspace filesystem space; insufficient disk space leaves queued work waiting and never rejects a task. The reserve is configurable (including zero); set it based on the 40 GiB system disk's operating-system, database, artifact, and log needs. The service-level quota ceiling defaults to 24 GiB; a per-task limit must not exceed it. Changes to the task quota require a helper/backend restart. Configure disk monitoring and backups before activating cloud task execution.

Artifacts use local storage unless `/etc/o-agent/cloud.env` sets `O_ARTIFACT_STORE_BACKEND=s3`. For an S3-compatible provider, configure `O_ARTIFACT_S3_ENDPOINT`, `O_ARTIFACT_S3_REGION`, `O_ARTIFACT_S3_BUCKET`, `O_ARTIFACT_S3_ACCESS_KEY_ID`, and `O_ARTIFACT_S3_SECRET_ACCESS_KEY`; optionally set `O_ARTIFACT_S3_SESSION_TOKEN`, `O_ARTIFACT_S3_PREFIX` (defaults to `o-agent/artifacts`), and `O_ARTIFACT_S3_PATH_STYLE` (defaults to `true`). Keep credentials in this root-owned, `oagent`-readable mode-0640 env file and restart the backend after changing them. Use HTTPS for non-loopback endpoints and a private bucket with narrowly scoped read/write/list multipart permissions. The default transfer path relays retryable chunks through the VPS and uses local temporary staging space. After verifying the provider's presigned PUT checksum and conditional CopyObject behavior in a private test bucket, an operator may set `O_ARTIFACT_S3_DIRECT_UPLOAD=true` to let execution nodes directly upload single batches up to 500,000,000 bytes; the control service still reads back and verifies the complete object before publishing artifact metadata. The setting is rejected unless the S3 backend is enabled and otherwise defaults off. It controls new uploads; already persisted direct-upload attempts remain resumable if the setting is later disabled. Larger artifacts remain on resumable 8 MiB chunks; 500 MB is not a task or artifact size limit. Existing local artifacts migrate to the configured bucket when first read, with their original local objects retained. Configure private access, bucket versioning/deletion protection, staging-prefix lifecycle rules, and recovery retention before production use; these settings are separate from the encrypted SQLite backup timer.

## Personal account registration and recovery

Local standalone mode stays usable without an account. Cloud mode requires an authenticated browser session. This deployment has one primary account managing the cloud Linux Agent and its paired local computers.

For email registration and password recovery, add the following settings to the existing root-owned, mode-0640 `/etc/o-agent/cloud.env`, using the actual values from your providers:

- `O_AUTH_OWNER_EMAIL`: the only mailbox eligible to initialize this server's primary account.
- `O_RESEND_API_KEY` and `O_AUTH_MAIL_FROM`: configure together. Verify the sender domain with Resend and grant the key permission to both send emails and retrieve their persisted records.
- `O_TURNSTILE_SITE_KEY` and `O_TURNSTILE_SECRET_KEY`: configure together. Register the public frontend hostname with Turnstile; the backend checks that hostname and the form action.

Restart the backend to consume these settings. Never put the secret keys in frontend environment variables, Git, or chat. Do not overwrite an existing env file. Public first-time registration only appears when an owner mailbox and mail service are configured and the primary account has no password yet. Without email configuration, the existing administrator bootstrap setup remains available; password-only login remains available for initialized accounts.

Verification codes expire after ten minutes and cannot be replayed. Provider acceptance is read back but does not claim inbox delivery. A failed or uncertain mail request does not create an account or change a password. Password recovery atomically changes credentials and revokes prior browser sessions. New passwords must not be empty or entirely whitespace. There is no password-specific minimum or maximum length, truncation, or character-composition requirement; the API retains its general request-body size protection. Existing credentials remain compatible. When configured, Turnstile protects registration/recovery requests and appears after repeated login failures. Server-side limits remain active without Turnstile.

See [the account design and API behavior](../../docs/remote-chat-and-auth.md). Before public use, verify real inbox receipt, registration, logout, recovery, old-session revocation, and Turnstile success/failure on the configured HTTPS hostname. Automated tests use local provider substitutes and do not constitute live email or VPS acceptance.

## Encrypted control-data backup

`axiom backup create-encrypted` exports a verified SQLite snapshot, its vault key, and finalized artifact objects as a chunk-authenticated AES-256-GCM archive. Generate the independent raw 32-byte backup key on a separate operator device, and keep a recoverable copy outside the VPS and away from the archive:

```bash
axiom backup keygen ./o-agent-backup.key
```

For a backup run, make the key readable to `oagent` only for the duration of the command (for example, stage it in `/run` from an approved secret channel), then create and verify a new, uniquely named archive:

```bash
sudo install -o oagent -g oagent -m 0600 /secure-transfer/o-agent-backup.key /run/o-agent-backup.key
sudo -u oagent sh -eu -c '
  trap "rm -f /run/o-agent-backup.key" EXIT
  /opt/o-agent/current/backend/axiom backup create-encrypted \
    /var/lib/o-agent/exports/control-20261002T120000Z.oabk \
    /run/o-agent-backup.key /var/lib/o-agent/data
  /opt/o-agent/current/backend/axiom backup verify-encrypted \
    /var/lib/o-agent/exports/control-20261002T120000Z.oabk /run/o-agent-backup.key
'
```

The release also installs a daily systemd backup job, but leaves its timer disabled until you configure an offsite remote and key. The uploader creates and locally verifies an encrypted archive, uploads it to a unique remote name, downloads and byte-checks the remote copy, and removes the local archive only after that check passes. Failed runs preserve the archive locally; the next run verifies and retries all retained archives before creating another snapshot. After a new snapshot is verified, it prunes matching `control-*.oabk` objects older than `O_BACKUP_RETENTION_DAYS` (default `90`) under the configured backup directory. Set a value from `0` to `9999` days in `/etc/o-agent/backup.env`; `0` disables pruning, and invalid values are rejected before backup work. The rclone remote needs permission to list this directory and delete only the matching backup objects; if pruning fails, the backup is already verified remotely but systemd reports the maintenance error. Use provider versioning or independent lifecycle protection if you need recovery from operator deletion. Since each control archive is a full snapshot, estimate monthly egress as `verified archive size × backup count × 2` (upload plus full remote read-back); keep the VPS's 350 GB monthly transfer budget in view.

Configure rclone with a dedicated remote, preferably a `crypt` wrapper over a supported object store, and place the config at `/etc/o-agent/rclone.conf` as `root:oagent` mode `0640`. Keep the independent backup key at `/etc/o-agent/backup.key` as `root:root` mode `0600`; also retain a recoverable copy away from the VPS. Configure `/etc/o-agent/backup.env` as `root:oagent` mode `0640`:

```ini
O_BACKUP_REMOTE=offsite-crypt:o-agent/control
O_BACKUP_RETENTION_DAYS=90
```

The remote must be a configured rclone remote plus a destination path. Install rclone from its trusted upstream/package source. Then test one backup and verify that the object exists and decrypts from a different machine before enabling the timer:

```bash
sudo systemctl start o-agent-backup.service
sudo systemctl status o-agent-backup.service --no-pager
sudo journalctl -u o-agent-backup.service -n 100 --no-pager
sudo systemctl enable --now o-agent-backup.timer
systemctl list-timers o-agent-backup.timer
```

The service exposes its key and rclone config to the process through systemd credentials, not environment variables. `restore-encrypted` restores into a new data directory and refuses to overwrite an existing one. Backups cover the control DB, vault key, and finalized artifact objects, not project worktrees, TLS files, service configuration, or operating-system state. Complete and record a restore drill before relying on a recovery objective.

To recover a named remote archive, download it to a new local filename, authenticate the archive, and restore into a new data directory. Keep the original offsite object untouched until the restored database passes its read-back check:

```bash
sudo install -o oagent -g oagent -m 0600 /secure-transfer/o-agent-backup.key /run/o-agent-backup.key
sudo install -d -o oagent -g oagent -m 0700 /var/lib/o-agent/exports
sudo -u oagent sh -eu -c '
  trap "rm -f /run/o-agent-backup.key" EXIT
  rclone --config /etc/o-agent/rclone.conf copyto \
    offsite-crypt:o-agent/control/control-20261002T032000Z.oabk \
    /var/lib/o-agent/exports/recovery-control.oabk
  /opt/o-agent/current/backend/axiom backup verify-encrypted \
    /var/lib/o-agent/exports/recovery-control.oabk /run/o-agent-backup.key
  /opt/o-agent/current/backend/axiom backup restore-encrypted \
    /var/lib/o-agent/exports/recovery-control.oabk /run/o-agent-backup.key \
    /var/lib/o-agent/recovery-data
'
```

The destination directory must not already exist. A successful command verifies SQLite integrity and opens the restored store before reporting success. Keep the downloaded archive and offsite source until the restored service has been tested; this command does not restore worktrees, TLS, service units, or host configuration.

## Preflight and activation

Run the backend as the same `oagent` user that will run cloud tasks. Enable cgroup v2 and ensure `cpu`, `memory`, and `pids` are delegated to that user's systemd manager. The installer reloads systemd before enabling linger so the first user manager starts with delegation. If an existing host needs a new delegation setting, drain its tasks and schedule a maintenance restart; never terminate a user manager that may own running tasks just to apply a deployment change.

```bash
sudo systemctl daemon-reload
sudo systemctl start user@$(id -u oagent).service
sudo -u oagent env XDG_RUNTIME_DIR=/run/user/$(id -u oagent) \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u oagent)/bus \
  systemctl --user daemon-reload
sudo -u oagent env XDG_RUNTIME_DIR=/run/user/$(id -u oagent) \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u oagent)/bus \
  /opt/o-agent/current/backend/axiom-sandbox-check
```

The check must exit zero and return `healthy`, `networkEgressFiltering: true`, and `writeVerified: true`. It also reports `workspaceQuotaMount` for the configured workspace filesystem. `mountSupported` and `quotaOptionFound` only confirm ext4/XFS mount prerequisites; `hardLimitVerified` must remain false until the privileged task quota backend applies and reads back a per-workspace limit. Do not present the mount preflight as task disk isolation. A successful binary build alone is not host validation. If the workspace filesystem is unsupported or lacks active `prjquota/pquota`, stop here and prepare the filesystem using the VPS provider's supported maintenance procedure; the installer does not repartition, reformat, or remount it.

After the mount preflight, start the privileged helper and read its systemd state back before starting the cloud backend:

```bash
sudo systemctl enable --now o-agent-quota-helper.service
sudo systemctl is-active o-agent-quota-helper.service
sudo journalctl -u o-agent-quota-helper.service -n 50 --no-pager
```

The backend checks helper reachability and mount prerequisites during cloud startup. It refuses cloud-role startup if the helper or quota mount is unavailable. This proves only that the helper is reachable and the mount advertises project-quota support; per-task `hardLimitVerified` remains false until a task workspace is allocated and the kernel read-back succeeds. On a 40 GiB disk, monitor actual headroom: quotas bound individual task workspaces, not the shared Git object store, artifacts, database, staging files, or logs.

Then, enable the persistent backend user service and the separate Web system service, and read both states back:

```bash
sudo -u oagent XDG_RUNTIME_DIR=/run/user/$(id -u oagent) \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u oagent)/bus \
  systemctl --user daemon-reload
sudo -u oagent XDG_RUNTIME_DIR=/run/user/$(id -u oagent) \
  DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u oagent)/bus \
  systemctl --user enable --now o-agent.service
sudo systemctl enable --now o-web.service
```

Verify `systemctl --user is-active o-agent.service`, `sudo systemctl is-active o-web.service`, `curl -fsS http://127.0.0.1:9171/api/v1/health`, and the public HTTPS login page. Verify a queued cloud task consumes CPU/memory scope and its durable artifact can be read back after service restart before declaring deployment complete.

## Rollback

Stop `o-agent.service` in the `oagent` user manager and the `o-web.service` system service, repoint `/opt/o-agent/current` to the previous immutable release, then start and read back both services and the health endpoint. Preserve `/var/lib/o-agent` and take a verified backup before a schema-changing downgrade. Do not roll back the database or delete a release if its task/checkpoint/artifact references are still needed.

The deployment supports one cloud runtime plus N independently paired local nodes. Node-local task state is not shared between local computers; the cloud control plane owns durable task history and confirmed artifacts. A local node needs outbound HTTPS to the cloud URL; it does not need an inbound public port.
