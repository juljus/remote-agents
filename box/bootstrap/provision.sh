#!/usr/bin/env bash
# Runs host/bootstrap.sh on the box as root, over SSH as root or as a passwordless sudoer;
# idempotent, so it also re-applies the configuration to a running box. Stages host/ with the
# bootstrap.env values, BOX_AUTHORIZED_KEYS and the overlay's local/host/ hooks.
# -p keeps SSH reachable on the public NIC, for the first run before Tailscale works.
# Usage: provision.sh [-F ssh_config] [-p] <ssh destination>
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
ssh_options=(-o BatchMode=yes)
public_ssh=closed
while getopts F:p opt; do
  case "$opt" in
    F) ssh_options+=(-F "$OPTARG") ;;
    p) public_ssh=open ;;
    *) exit 1 ;;
  esac
done
shift $((OPTIND - 1))
[[ $# -eq 1 ]] || { echo "usage: $0 [-F ssh_config] [-p] <ssh destination>" >&2; exit 1; }
destination="$1"
# shellcheck source=box/bootstrap/env.sh
source "$here/env.sh"
require_env BOX_AUTHORIZED_KEYS
[[ -s "$BOX_AUTHORIZED_KEYS" ]] || die "BOX_AUTHORIZED_KEYS $BOX_AUTHORIZED_KEYS is missing or empty"
# ssh-keygen -l also reads private keys, which must never leave the laptop.
! grep -q 'PRIVATE KEY' "$BOX_AUTHORIZED_KEYS" || die "BOX_AUTHORIZED_KEYS $BOX_AUTHORIZED_KEYS holds a private key"
ssh-keygen -l -f "$BOX_AUTHORIZED_KEYS" >/dev/null || die "BOX_AUTHORIZED_KEYS $BOX_AUTHORIZED_KEYS holds no public keys"

local_stage="$(mktemp -d)"
trap 'rm -rf "$local_stage"' EXIT
cp -R "$here/host/." "$local_stage/"
write_box_env "$local_stage/box.env"
cp "$BOX_AUTHORIZED_KEYS" "$local_stage/authorized_keys"
if [[ -d "$here/local/host" ]]; then
  cp -R "$here/local/host" "$local_stage/local"
fi

stage="$(ssh "${ssh_options[@]}" "$destination" mktemp -d)"
scp "${ssh_options[@]}" -q -r "$local_stage/." "$destination:$stage/"
ssh "${ssh_options[@]}" "$destination" bash -s -- "$stage" "$public_ssh" <<'REMOTE'
set -euo pipefail
trap 'rm -rf "$1"' EXIT
sudo -n bash "$1/bootstrap.sh" "$2"
REMOTE
