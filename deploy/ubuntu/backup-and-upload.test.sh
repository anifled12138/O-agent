#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/data" "$tmp/remote/control" "$tmp/exports"
touch "$tmp/key"
cat > "$tmp/bin/axiom" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$1 $2" == 'backup create-encrypted' || "$1 $2" == 'backup verify-encrypted' ]]
if [[ "$2" == create-encrypted ]]; then printf 'test encrypted archive\n' > "$3"; else [[ -s "$3" ]]; fi
EOF
cat > "$tmp/bin/rclone" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
case "$1" in
  copyto)
    [[ "$2" == --immutable ]]
    source=$3
    target=${4#test:}
    mkdir -p "$(dirname -- "$REMOTE_ROOT$target")"
    if [[ -e "$REMOTE_ROOT$target" ]]; then
      cmp -- "$source" "$REMOTE_ROOT$target"
    else
      cp -- "$source" "$REMOTE_ROOT$target"
    fi
    ;;
  check)
    [[ "$2" == --download ]]
    [[ ${FAIL_CHECK:-0} != 1 ]]
    source=$3
    target=${4#test:}
    cmp -- "$source" "$REMOTE_ROOT$target"
    ;;
  delete)
    target=${2#test:}
    [[ "$target" == /control/control-*.oabk ]]
    rm -- "$REMOTE_ROOT$target"
    ;;
  lsf)
    [[ "$2" == --files-only && "$3" == test:/control/ ]]
    [[ ${FAIL_LIST:-0} != 1 ]]
    shopt -s nullglob
    for archive in "$REMOTE_ROOT/control"/control-*.oabk; do
      basename -- "$archive"
    done
    ;;
  deletefile)
    target=${2#test:}
    [[ "$target" == /control/control-*.oabk ]]
    rm -- "$REMOTE_ROOT$target"
    ;;
  *) exit 64 ;;
esac
EOF
cat > "$tmp/bin/date" <<'EOF'
#!/usr/bin/env bash
if [[ "$2" == -d ]]; then printf '20251003T000000Z\n'; else printf '20260101T000000Z\n'; fi
EOF
cat > "$tmp/bin/install" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$#" == 4 && "$1 $2 $3" == '-d -m 0700' ]]
mkdir -p -- "$4"
EOF
chmod +x "$tmp/bin/axiom" "$tmp/bin/rclone" "$tmp/bin/date" "$tmp/bin/install"
export PATH="$tmp/bin:$PATH"
export O_BACKUP_AXIOM="$tmp/bin/axiom" O_BACKUP_DATA_DIR="$tmp/data" O_BACKUP_KEY_FILE="$tmp/key"
export O_BACKUP_REMOTE='test:/control' O_BACKUP_EXPORT_DIR="$tmp/exports" REMOTE_ROOT="$tmp/remote"
printf 'old backup\n' > "$tmp/remote/control/control-20250101T000000Z-old00001.oabk"
printf 'recent backup\n' > "$tmp/remote/control/control-20251201T000000Z-new00001.oabk"

bash "$script_dir/backup-and-upload.sh"
[[ -z "$(find "$tmp/exports" -type f -name '*.oabk' -print -quit)" ]]
[[ -n "$(find "$tmp/remote/control" -type f -name '*.oabk' -print -quit)" ]]
[[ ! -e "$tmp/remote/control/control-20250101T000000Z-old00001.oabk" ]]
[[ -e "$tmp/remote/control/control-20251201T000000Z-new00001.oabk" ]]

export FAIL_CHECK=1
if bash "$script_dir/backup-and-upload.sh"; then
  echo 'upload check failure unexpectedly returned success' >&2
  exit 1
fi
[[ -n "$(find "$tmp/exports" -type f -name '*.oabk' -print -quit)" ]]

unset FAIL_CHECK
bash "$script_dir/backup-and-upload.sh"
[[ -z "$(find "$tmp/exports" -type f -name '*.oabk' -print -quit)" ]]
[[ "$(find "$tmp/remote/control" -type f -name '*.oabk' | wc -l)" -eq 4 ]]
export O_BACKUP_RETENTION_DAYS=10000
if bash "$script_dir/backup-and-upload.sh"; then
  echo 'invalid retention configuration unexpectedly returned success' >&2
  exit 1
fi
[[ -z "$(find "$tmp/exports" -type f -name '*.oabk' -print -quit)" ]]
[[ "$(find "$tmp/remote/control" -type f -name '*.oabk' | wc -l)" -eq 4 ]]
export FAIL_LIST=1
export O_BACKUP_RETENTION_DAYS=0
bash "$script_dir/backup-and-upload.sh"
[[ "$(find "$tmp/remote/control" -type f -name '*.oabk' | wc -l)" -eq 5 ]]
unset O_BACKUP_RETENTION_DAYS
if bash "$script_dir/backup-and-upload.sh"; then
  echo 'remote retention listing failure unexpectedly returned success' >&2
  exit 1
fi
[[ -z "$(find "$tmp/exports" -type f -name '*.oabk' -print -quit)" ]]
[[ "$(find "$tmp/remote/control" -type f -name '*.oabk' | wc -l)" -eq 6 ]]
echo 'backup upload, retention, and failure-preservation checks passed'
