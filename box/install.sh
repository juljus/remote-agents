#!/usr/bin/env bash
# Installs remote-agents on the box for the current user under an install
# name (boxlayout.DefaultName in production). Idempotent; never restarts the
# shpool daemon, because that would kill running sessions.
# Usage: install.sh <stage-dir> <name>   (stage-dir holds this directory's files plus remote-agents-box)
set -euo pipefail
shopt -s inherit_errexit

SHPOOL_VERSION=0.11.5
SHPOOL_SHA256=132f786bd275749a46224c17a071474a850f0be1fbb752b1c96d910778543c9b
SHPOOL_URL="https://github.com/shell-pool/shpool/releases/download/v${SHPOOL_VERSION}/shpool-x86_64-unknown-linux-musl.tar.gz"

die() { echo "install.sh: $*" >&2; exit 1; }

[[ $# -eq 2 ]] || die "usage: install.sh <stage-dir> <name>"
STAGE="$1"
NAME="$2"
[[ "$NAME" =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "invalid install name $NAME"
[[ -x "$STAGE/remote-agents-box" ]] || die "$STAGE/remote-agents-box missing"
[[ "$(uname -s)/$(uname -m)" == "Linux/x86_64" ]] || die "unsupported platform $(uname -s)/$(uname -m)"
[[ "$(id -u)" -ne 0 ]] || die "run as the box user, not root"

# systemctl --user needs the user manager's runtime dir, which non-interactive ssh does not set.
uid="$(id -u)"
export XDG_RUNTIME_DIR="/run/user/$uid"
[[ -d "$XDG_RUNTIME_DIR" ]] || die "$XDG_RUNTIME_DIR missing; enable lingering for $(id -un)"

SHARE="$HOME/.local/share/$NAME"
STATE="$HOME/.local/state/$NAME"
CONFIG="$HOME/.config/$NAME"
UNITS="$HOME/.config/systemd/user"
SERVICE="$NAME-shpool.service"
SOCKET="$NAME-shpool.socket"

mkdir -p "$SHARE/bin" "$SHARE/shims" "$SHARE/hookbin" "$CONFIG" "$UNITS"
mkdir -p "$STATE"
chmod 0700 "$STATE"

# install_file src dst mode: replaces dst atomically and prints "changed" when its content differed.
install_file() {
  local src="$1" dst="$2" mode="$3"
  if [[ -f "$dst" ]] && cmp -s "$src" "$dst"; then
    return 0
  fi
  install -m "$mode" "$src" "$dst.tmp.$$"
  mv -f "$dst.tmp.$$" "$dst"
  echo changed
}

current_shpool=""
if [[ -x "$SHARE/bin/shpool" ]]; then
  current_shpool="$("$SHARE/bin/shpool" version)"
fi
if [[ "$current_shpool" != "shpool $SHPOOL_VERSION" ]]; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL -o "$tmp/shpool.tgz" "$SHPOOL_URL"
  echo "$SHPOOL_SHA256  $tmp/shpool.tgz" | sha256sum -c --quiet -
  tar -xzf "$tmp/shpool.tgz" -C "$tmp" shpool
  install -m 0755 "$tmp/shpool" "$SHARE/bin/shpool.tmp.$$"
  mv -f "$SHARE/bin/shpool.tmp.$$" "$SHARE/bin/shpool"
  echo "installed shpool $SHPOOL_VERSION (was: ${current_shpool:-none})"
  if [[ -n "$current_shpool" ]] && systemctl --user is-active --quiet "$SERVICE"; then
    echo "NOTE: the running shpool daemon still uses the old binary;"
    echo "      restart $SERVICE once no sessions are running."
  fi
fi

changed="$(install_file "$STAGE/remote-agents-box" "$SHARE/bin/remote-agents-box" 0755)"
[[ -z "$changed" ]] || echo "installed remote-agents-box"
for shim in xclip wl-paste xdg-open; do
  ln -sfn ../bin/remote-agents-box "$SHARE/shims/$shim"
done
ln -sfn ../bin/remote-agents-box "$SHARE/hookbin/cmux"

changed="$(install_file "$STAGE/shpool.toml" "$CONFIG/shpool.toml" 0644)"
[[ -z "$changed" ]] || echo "installed shpool.toml (the daemon reloads it)"

units_changed=""
for kind in socket service; do
  sed "s/@NAME@/$NAME/g" "$STAGE/systemd/shpool.$kind.in" > "$STAGE/systemd/$NAME-shpool.$kind"
  units_changed+="$(install_file "$STAGE/systemd/$NAME-shpool.$kind" "$UNITS/$NAME-shpool.$kind" 0644)"
done
if [[ -n "$units_changed" ]]; then
  systemctl --user daemon-reload
  echo "installed systemd units"
fi
systemctl --user enable --now "$SOCKET" >/dev/null

if [[ ! -x "$HOME/.local/bin/claude" ]]; then
  curl -fsSL https://claude.ai/install.sh | bash
fi

"$SHARE/bin/shpool" --socket "$STATE/shpool.sock" --config-file "$CONFIG/shpool.toml" --no-daemonize list >/dev/null \
  || die "shpool daemon does not answer on $STATE/shpool.sock"
echo "remote-agents box install $NAME OK: $("$SHARE/bin/shpool" version), claude $("$HOME/.local/bin/claude" --version)"
