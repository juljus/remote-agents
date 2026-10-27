#!/usr/bin/env bash
# Copies Claude Code session transcripts (<id>.jsonl and their <id>/ subagent dirs) from one
# host's project dir to another's, relayed through this machine, so `claude --resume <id>` finds
# them under the destination's project key. Copies only: nothing is deleted at the source, and
# transcripts already at the destination are kept.
# Usage: copy-transcripts.sh <source ssh destination> <source project dir> <target ssh destination> <target project dir>
set -euo pipefail
[[ $# -eq 4 ]] || { echo "usage: $0 <source host> <source project dir> <target host> <target project dir>" >&2; exit 1; }

ssh "$3" mkdir -p "$4"
ssh "$1" bash -s -- "$2" <<'REMOTE' | ssh "$3" tar -x --skip-old-files -v -C "$4"
set -euo pipefail
cd "$1"
shopt -s nullglob
entries=()
for transcript in *.jsonl; do
  entries+=("$transcript")
  if [[ -d "${transcript%.jsonl}" ]]; then
    entries+=("${transcript%.jsonl}")
  fi
done
tar -c "${entries[@]}"
REMOTE
