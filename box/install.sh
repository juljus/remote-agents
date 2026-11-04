#!/usr/bin/env bash
# Installs remote-agents on the box for the current user under an install
# name (boxlayout.DefaultName in production). Idempotent; never restarts the
# shpool daemon, because that would kill running sessions.
# Usage: install.sh <stage-dir> <name>   (stage-dir holds this directory's files plus remote-agents-box)
set -euo pipefail
shopt -s inherit_errexit

# shpool/build.sh builds this byte for byte; update the pin whenever box/shpool changes.
SHPOOL_VERSION=0.11.5+ra.1
SHPOOL_SHA256=338c342dd09af10693592eb2989e294c59254de23d7150aa2d377586832b2367

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
REFRESH="$NAME-accounts-refresh"

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

# pinned_shpool file: whether file is the shpool build SHPOOL_SHA256 pins.
pinned_shpool() { echo "$SHPOOL_SHA256  $1" | sha256sum -c --quiet - >/dev/null 2>&1; }

# Every install on the box shares one build of the pinned shpool.
SHPOOL_BUILD="$HOME/.cache/remote-agents/shpool-$SHPOOL_SHA256"
current_shpool=""
if [[ -x "$SHARE/bin/shpool" ]]; then
  current_shpool="$("$SHARE/bin/shpool" version)"
fi
if ! pinned_shpool "$SHARE/bin/shpool"; then
  if ! pinned_shpool "$SHPOOL_BUILD"; then
    command -v docker >/dev/null || die "building shpool $SHPOOL_VERSION needs docker"
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    "$STAGE/shpool/build.sh" "$tmp/shpool"
    pinned_shpool "$tmp/shpool" \
      || die "shpool build has sha256 $(sha256sum "$tmp/shpool" | cut -d' ' -f1), not SHPOOL_SHA256; update the pin"
    mkdir -p "$(dirname "$SHPOOL_BUILD")"
    install -m 0755 "$tmp/shpool" "$SHPOOL_BUILD.tmp.$$"
    mv -f "$SHPOOL_BUILD.tmp.$$" "$SHPOOL_BUILD"
  fi
  install -m 0755 "$SHPOOL_BUILD" "$SHARE/bin/shpool.tmp.$$"
  mv -f "$SHARE/bin/shpool.tmp.$$" "$SHARE/bin/shpool"
  echo "installed shpool $SHPOOL_VERSION (was: ${current_shpool:-none})"
  if [[ -n "$current_shpool" ]] && systemctl --user is-active --quiet "$SERVICE"; then
    echo "NOTE: the running shpool daemon still uses the old binary, and restarting it ends"
    echo "      every session; restart $SERVICE once none is running."
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

# Only the production install (boxlayout.DefaultName) keeps saved Claude and
# Codex logins fresh: a test install's account commands never touch the real
# logins, and it runs no timer.
units=(shpool.socket shpool.service)
if [[ "$NAME" == remote-agents ]]; then
  units+=(accounts-refresh.service accounts-refresh.timer)
elif [[ -e "$UNITS/$REFRESH.timer" ]]; then
  systemctl --user disable --now "$REFRESH.timer" >/dev/null
  rm -f "$UNITS/$REFRESH.timer" "$UNITS/$REFRESH.service"
  systemctl --user daemon-reload
  echo "removed $REFRESH.timer: only the production install runs it"
fi
units_changed=""
for unit in "${units[@]}"; do
  sed "s/@NAME@/$NAME/g" "$STAGE/systemd/$unit.in" > "$STAGE/systemd/$NAME-$unit"
  units_changed+="$(install_file "$STAGE/systemd/$NAME-$unit" "$UNITS/$NAME-$unit" 0644)"
done
if [[ -n "$units_changed" ]]; then
  systemctl --user daemon-reload
  echo "installed systemd units"
fi
systemctl --user enable --now "$SOCKET" >/dev/null
if [[ "$NAME" == remote-agents ]]; then
  systemctl --user enable --now "$REFRESH.timer" >/dev/null
fi

if [[ ! -x "$HOME/.local/bin/claude" ]]; then
  curl -fsSL https://claude.ai/install.sh | bash
fi

"$SHARE/bin/shpool" --socket "$STATE/shpool.sock" --config-file "$CONFIG/shpool.toml" --no-daemonize list >/dev/null \
  || die "shpool daemon does not answer on $STATE/shpool.sock"
echo "remote-agents box install $NAME OK: $("$SHARE/bin/shpool" version), claude $("$HOME/.local/bin/claude" --version)"
