#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: sudo install-runtime.sh <release-directory> <release-id>

The release directory must contain:
  backend/axiom
  backend/axiom-sandbox-check
  backend/axiom-quota-helper
  frontend/standalone/server.js
  frontend/standalone/dist/
  frontend/standalone/public/
  deploy/ubuntu/backup-and-upload.sh
  deploy/ubuntu/o-agent-backup.service
  deploy/ubuntu/o-agent-backup.timer
  deploy/ubuntu/o-agent-quota-helper.service
  deploy/ubuntu/config-paths.sh

This stages a release and systemd units. It does not start services or configure
DNS, TLS, credentials, or firewall rules. Review README.md before activation.
EOF
  exit 2
}

[[ $# -eq 2 ]] || usage
[[ ${EUID} -eq 0 ]] || { echo 'Run as root (sudo).' >&2; exit 1; }
source_dir=$(realpath -e -- "$1")
release_id=$2
[[ "$release_id" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || { echo 'Invalid release id.' >&2; exit 1; }
release_dir="/opt/o-agent/releases/$release_id"
[[ ! -e "$release_dir" ]] || { echo "Release already exists: $release_dir" >&2; exit 1; }
for required in backend/axiom backend/axiom-sandbox-check backend/axiom-quota-helper frontend/standalone/server.js frontend/standalone/dist frontend/standalone/public deploy/ubuntu/backup-and-upload.sh deploy/ubuntu/o-agent-backup.service deploy/ubuntu/o-agent-backup.timer deploy/ubuntu/o-agent-quota-helper.service deploy/ubuntu/config-paths.sh; do
  [[ -e "$source_dir/$required" ]] || { echo "Release is missing $required" >&2; exit 1; }
done
[[ -x "$source_dir/backend/axiom" && -x "$source_dir/backend/axiom-sandbox-check" && -x "$source_dir/backend/axiom-quota-helper" ]] || { echo 'Backend binaries must be executable.' >&2; exit 1; }
command -v node >/dev/null || { echo 'Node.js >=22.13 is required.' >&2; exit 1; }
node -e 'const [major,minor]=process.versions.node.split(".").map(Number); process.exit(major > 22 || (major === 22 && minor >= 13) ? 0 : 1)' || { echo "Node.js >=22.13 is required; found $(node --version)." >&2; exit 1; }
command -v systemctl >/dev/null || { echo 'systemd is required.' >&2; exit 1; }
command -v loginctl >/dev/null || { echo 'systemd-logind is required.' >&2; exit 1; }
command -v bwrap >/dev/null || { echo 'Bubblewrap is required for Linux task isolation.' >&2; exit 1; }
command -v systemd-run >/dev/null || echo 'Network-enabled sandbox commands will be unavailable without systemd-run; offline Bubblewrap remains available.' >&2

source "$(dirname -- "$0")/config-paths.sh"
o_agent_require_real_directory /etc/o-agent
install -d -o root -g root -m 0755 /etc/o-agent
o_agent_require_real_config_file /etc/o-agent/cloud.env
o_agent_require_real_config_file /etc/o-agent/quota-helper.env

install -d -o root -g root -m 0755 /opt/o-agent/releases /etc/systemd/system/user@.service.d
if ! id oagent >/dev/null 2>&1; then
  useradd --system --create-home --home-dir /var/lib/o-agent --shell /usr/sbin/nologin oagent
fi
if ! id oagent-web >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin oagent-web
fi
install -d -o oagent -g oagent -m 0700 /var/lib/o-agent /var/lib/o-agent/data /var/lib/o-agent/workspaces /var/lib/o-agent/tmp/agent-runs
install -d -o root -g oagent -m 0750 /etc/o-agent

stage_dir=$(mktemp -d "/opt/o-agent/releases/.stage-${release_id}.XXXXXX")
cleanup() { [[ -z "${stage_dir:-}" ]] || rm -rf -- "$stage_dir"; }
trap cleanup EXIT
cp -a -- "$source_dir/backend" "$stage_dir/backend"
cp -a -- "$source_dir/frontend" "$stage_dir/frontend"
chown -R root:root "$stage_dir"
chmod 0755 "$stage_dir/backend/axiom" "$stage_dir/backend/axiom-sandbox-check" "$stage_dir/backend/axiom-quota-helper"
mv -- "$stage_dir" "$release_dir"
stage_dir=

if [[ ! -e /etc/o-agent/cloud.env ]]; then
  install -o root -g oagent -m 0640 /dev/null /etc/o-agent/cloud.env
  cat > /etc/o-agent/cloud.env <<'EOF'
# Add O_AUTH_BOOTSTRAP_TOKEN (at least 32 random characters) before starting.
# Optional personal-account email registration/recovery, configure real values:
# O_AUTH_OWNER_EMAIL=owner@example.com
# O_RESEND_API_KEY=<server-side-key-with-send-and-read-permission>
# O_AUTH_MAIL_FROM=auth@example.com
# Optional Turnstile (site and secret keys must be configured together):
# O_TURNSTILE_SITE_KEY=<public-site-key>
# O_TURNSTILE_SECRET_KEY=<server-side-secret>
# Replace the example origin with the public HTTPS origin used in Caddy.
# Add provider credentials through the authenticated O settings UI after first login.
O_FRONTEND_ORIGIN=https://o.example.com
O_CLOUD_WORKER_CONCURRENCY=64
O_QUOTA_HELPER_SOCKET=/run/o-agent/quota.sock
O_CLOUD_TASK_WORKSPACE_QUOTA_ENABLED=false
O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES=0
O_CLOUD_TASK_DISK_RESERVE_BYTES=4294967296
O_CLOUD_TASK_WORKSPACE_RETENTION=720h
EOF
fi
chown root:oagent /etc/o-agent/cloud.env
chmod 0640 /etc/o-agent/cloud.env

install -D -o root -g root -m 0644 "$(dirname -- "$0")/o-agent.service" /etc/systemd/user/o-agent.service
install -D -o root -g root -m 0644 "$(dirname -- "$0")/o-agent-quota-helper.service" /etc/systemd/system/o-agent-quota-helper.service
install -D -o root -g root -m 0644 "$(dirname -- "$0")/o-web.service" /etc/systemd/system/o-web.service
install -D -o root -g root -m 0644 "$(dirname -- "$0")/o-agent-backup.service" /etc/systemd/system/o-agent-backup.service
install -D -o root -g root -m 0644 "$(dirname -- "$0")/o-agent-backup.timer" /etc/systemd/system/o-agent-backup.timer
install -d -o root -g root -m 0755 /usr/local/lib/o-agent
install -o root -g root -m 0755 "$(dirname -- "$0")/backup-and-upload.sh" /usr/local/lib/o-agent/backup-and-upload.sh
install -D -o root -g root -m 0644 "$(dirname -- "$0")/user-manager-delegation.conf" /etc/systemd/system/user@.service.d/50-o-agent-delegation.conf
if [[ ! -e /etc/o-agent/quota-helper.env ]]; then
  install -o root -g root -m 0600 /dev/null /etc/o-agent/quota-helper.env
  cat > /etc/o-agent/quota-helper.env <<EOF
O_AGENT_UID=$(id -u oagent)
O_AGENT_GID=$(id -g oagent)
O_QUOTA_MAX_BYTES=25769803776
# Reserve Linux project IDs 0x80000000-0xffffffff for O task workspaces.
EOF
fi
if ! grep -Fq '# Reserve Linux project IDs 0x80000000-0xffffffff for O task workspaces.' /etc/o-agent/quota-helper.env; then
  printf '\n# Reserve Linux project IDs 0x80000000-0xffffffff for O task workspaces.\n' >> /etc/o-agent/quota-helper.env
fi
grep -Fq '# Reserve Linux project IDs 0x80000000-0xffffffff for O task workspaces.' /etc/o-agent/quota-helper.env
chown root:root /etc/o-agent/quota-helper.env
chmod 0600 /etc/o-agent/quota-helper.env
systemctl daemon-reload
loginctl enable-linger oagent

ln -sfn -- "$release_dir" /opt/o-agent/current.new
mv -Tf -- /opt/o-agent/current.new /opt/o-agent/current
printf 'Staged release %s at %s. Services remain stopped until configuration and host checks pass.\n' "$release_id" "$release_dir"
