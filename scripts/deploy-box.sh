#!/usr/bin/env bash
# Builds the box binary and installs remote-agents on HOST (an ssh destination)
# under the install name NAME.
# Usage: scripts/deploy-box.sh <host> <name>
set -euo pipefail

[[ $# -eq 2 ]] || { echo "usage: $0 <host> <name>" >&2; exit 1; }
HOST="$1"
NAME="$2"
[[ "$NAME" =~ ^[a-z0-9][a-z0-9-]*$ ]] || { echo "$0: invalid install name $NAME" >&2; exit 1; }
STAGE_REMOTE=".cache/$NAME-stage"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STAGE_LOCAL="$(mktemp -d)"
trap 'rm -rf "$STAGE_LOCAL"' EXIT

cp -R "$ROOT/box/." "$STAGE_LOCAL/"
(cd "$ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$STAGE_LOCAL/remote-agents-box" ./cmd/remote-agents-box)

ssh "$HOST" bash -s -- "$STAGE_REMOTE" <<'REMOTE'
rm -rf "$1" && mkdir -p "$1"
REMOTE
scp -q -p -r "$STAGE_LOCAL/." "$HOST:$STAGE_REMOTE/"
ssh "$HOST" bash -s -- "$STAGE_REMOTE" "$NAME" <<'REMOTE'
bash "$1/install.sh" "$1" "$2" && rm -rf "$1"
REMOTE
