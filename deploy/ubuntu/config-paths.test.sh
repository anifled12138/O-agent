#!/usr/bin/env bash
set -Eeuo pipefail

source "$(dirname -- "$0")/config-paths.sh"
root=$(mktemp -d)
trap 'rm -rf -- "$root"' EXIT
mkdir -- "$root/real-directory"
: > "$root/regular-file"
ln -s -- "$root/regular-file" "$root/file-link"
ln -s -- "$root/real-directory" "$root/directory-link"
mkfifo -- "$root/fifo"

o_agent_require_real_directory "$root/real-directory"
o_agent_require_real_config_file "$root/regular-file"
for path in "$root/directory-link" "$root/regular-file" "$root/fifo"; do
  if o_agent_require_real_directory "$path" 2>/dev/null; then
    echo "unexpectedly accepted directory path $path" >&2
    exit 1
  fi
done
for path in "$root/file-link" "$root/directory-link" "$root/real-directory" "$root/fifo"; do
  if o_agent_require_real_config_file "$path" 2>/dev/null; then
    echo "unexpectedly accepted configuration path $path" >&2
    exit 1
  fi
done
echo 'configuration path guards: passed'
