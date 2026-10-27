# shellcheck shell=bash
# Sourced by the laptop-side bootstrap scripts: loads the personal values from bootstrap.env beside
# this file (start from bootstrap.env.example) and checks them, naming whatever is missing or wrong.

die() { echo "$0: $*" >&2; exit 1; }

bootstrap_env="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/bootstrap.env"
[[ -f "$bootstrap_env" ]] || die "$bootstrap_env is missing: copy bootstrap.env.example and fill it in"
# shellcheck source=/dev/null
source "$bootstrap_env"

# The values the scripts running on the box get, through write_box_env.
BOX_ENV_VARS=(BOX_USER BOX_HOME BOX_WORKDIR GIT_USER_NAME GIT_USER_EMAIL)

# require_env name...: exits naming every variable that is unset or empty.
require_env() {
  local name
  local missing=()
  for name in "$@"; do
    if [[ -z "${!name:-}" ]]; then
      missing+=("$name")
    fi
  done
  [[ "${#missing[@]}" -eq 0 ]] || die "set ${missing[*]} in $bootstrap_env"
}

# write_box_env file: writes BOX_ENV_VARS as shell assignments for the box side to source.
write_box_env() {
  local name
  for name in "${BOX_ENV_VARS[@]}"; do
    printf '%s=%q\n' "$name" "${!name}"
  done >"$1"
}

require_env "${BOX_ENV_VARS[@]}"
[[ "$BOX_USER" =~ ^[a-z_][a-z0-9_-]*$ ]] || die "BOX_USER $BOX_USER is not a valid user name"
# plain_path path: true for an absolute path without a trailing slash, "." or ".." components.
plain_path() { [[ "$1" =~ ^(/[^/]+)+$ && "$1/" != */./* && "$1/" != */../* ]]; }
plain_path "$BOX_HOME" || die "BOX_HOME $BOX_HOME must be an absolute path without a trailing slash, . or .."
plain_path "$BOX_WORKDIR" && [[ "$BOX_WORKDIR" == "$BOX_HOME"/* ]] ||
  die "BOX_WORKDIR $BOX_WORKDIR must be a path inside BOX_HOME without a trailing slash, . or .."
