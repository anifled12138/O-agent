#!/usr/bin/env bash

o_agent_require_real_directory() {
  local path=$1
  if [[ -L "$path" || ( -e "$path" && ! -d "$path" ) ]]; then
    echo "Refusing a symlink or non-directory path: $path" >&2
    return 1
  fi
}

o_agent_require_real_config_file() {
  local path=$1
  if [[ -L "$path" || ( -e "$path" && ! -f "$path" ) ]]; then
    echo "Refusing to modify a symlink or non-regular configuration path: $path" >&2
    return 1
  fi
}
