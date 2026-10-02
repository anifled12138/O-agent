#!/usr/bin/env bash
set -Eeuo pipefail

data_dir=${O_BACKUP_DATA_DIR:-/var/lib/o-agent/data}
key_file=${O_BACKUP_KEY_FILE:-/etc/o-agent/backup.key}
remote=${O_BACKUP_REMOTE:-}
axiom=${O_BACKUP_AXIOM:-/opt/o-agent/current/backend/axiom}
export_dir=${O_BACKUP_EXPORT_DIR:-/var/lib/o-agent/exports}
retention_days=${O_BACKUP_RETENTION_DAYS:-90}

[[ -x "$axiom" ]] || { echo "O backup binary is not executable: $axiom" >&2; exit 1; }
[[ -d "$data_dir" ]] || { echo "O data directory does not exist: $data_dir" >&2; exit 1; }
[[ -f "$key_file" && -r "$key_file" ]] || { echo 'O backup credential is missing or unreadable.' >&2; exit 1; }
[[ "$remote" =~ ^[A-Za-z0-9_-]+:.+ ]] || { echo 'O_BACKUP_REMOTE must name a configured rclone remote and destination path.' >&2; exit 1; }
command -v rclone >/dev/null || { echo 'rclone is required for offsite backup.' >&2; exit 1; }
[[ "$retention_days" =~ ^(0|[1-9][0-9]{0,3})$ ]] || { echo 'O_BACKUP_RETENTION_DAYS must be 0 (disabled) or between 1 and 9999.' >&2; exit 1; }

install -d -m 0700 "$export_dir"

upload_archive() {
  local archive=$1
  local remote_archive="${remote%/}/$(basename -- "$archive")"

  # Verify any preserved local archive again. If a prior run uploaded it but
  # failed during read-back, recognize the identical remote object and finish
  # cleanup without creating duplicate copies or overwriting a conflict.
  "$axiom" backup verify-encrypted "$archive" "$key_file"
  if rclone check --download "$archive" "$remote_archive" >/dev/null 2>&1; then
    rm -- "$archive"
    printf 'verified offsite control backup: %s\n' "$remote_archive"
    return
  fi

  rclone copyto --immutable "$archive" "$remote_archive"
  rclone check --download "$archive" "$remote_archive"
  rm -- "$archive"
  printf 'verified offsite control backup: %s\n' "$remote_archive"
}

prune_expired_archives() {
  # Retention applies only to our own immutable archive names in the configured
  # backup directory. Pending local archives are uploaded and read back first.
  if [[ "$retention_days" == 0 ]]; then
    printf 'offsite backup retention disabled under %s\n' "${remote%/}/"
    return 0
  fi
  local cutoff name archive_time listing
  local archive_pattern='^control-([0-9]{8}T[0-9]{6}Z)(-[[:alnum:]]{8})?\.oabk$'
  cutoff=$(date -u -d "$retention_days days ago" +%Y%m%dT%H%M%SZ)
  listing=$(rclone lsf --files-only "${remote%/}/") || return
  if [[ -n "$listing" ]]; then
    while IFS= read -r name; do
      [[ "$name" =~ $archive_pattern ]] || continue
      archive_time=${BASH_REMATCH[1]}
      if [[ "$archive_time" < "$cutoff" ]]; then
        rclone deletefile "${remote%/}/$name"
      fi
    done <<< "$listing"
  fi
  printf 'applied offsite backup retention: %s days under %s\n' "$retention_days" "${remote%/}/"
}

# Retry retained archives before making another snapshot. A failed retry stops
# this run and preserves the only local copy for the next attempt/operator.
shopt -s nullglob
for archive in "$export_dir"/control-*.oabk; do
  if [[ ! -f "$archive" || -L "$archive" ]]; then
    echo "refusing non-regular pending backup archive: $archive" >&2
    exit 1
  fi
  upload_archive "$archive"
done

stamp=$(date -u +%Y%m%dT%H%M%SZ)
archive=$(mktemp "$export_dir/control-$stamp-XXXXXXXX.oabk")
if ! "$axiom" backup create-encrypted "$archive" "$key_file" "$data_dir"; then
  if [[ ! -s "$archive" ]]; then
    rm -- "$archive"
  fi
  exit 1
fi
upload_archive "$archive"
prune_expired_archives
