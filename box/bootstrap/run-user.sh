#!/usr/bin/env bash
# Runs one of the user/ or overlay local/user/ scripts on the box as the box user over SSH, staged
# beside the other files of both directories, with the bootstrap.env values exported.
# Usage: run-user.sh <ssh destination> <script in user/ or local/user/>
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
[[ $# -eq 2 ]] || { echo "usage: $0 <ssh destination> <script in user/ or local/user/>" >&2; exit 1; }
destination="$1"
script="$2"
# shellcheck source=box/bootstrap/env.sh
source "$here/env.sh"

local_stage="$(mktemp -d)"
trap 'rm -rf "$local_stage"' EXIT
cp -R "$here/user/." "$local_stage/"
if [[ -d "$here/local/user" ]]; then
  for file in "$here"/local/user/*; do
    [[ ! -e "$local_stage/$(basename "$file")" ]] || die "local/user/$(basename "$file") would replace user/$(basename "$file")"
  done
  cp -R "$here/local/user/." "$local_stage/"
fi
[[ -f "$local_stage/$script" ]] || die "no $script in user/ or local/user/"
write_box_env "$local_stage/box.env"

stage="$(ssh "$destination" mktemp -d)"
scp -q -r "$local_stage/." "$destination:$stage/"
ssh "$destination" bash -s -- "$stage" "$script" <<'REMOTE'
set -euo pipefail
trap 'rm -rf "$1"' EXIT
set -a
source "$1/box.env"
set +a
bash "$1/$2"
REMOTE
