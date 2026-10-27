#!/usr/bin/env bash
# Copies the laptop's Claude Code configuration to the box: instructions (the top-level *.md),
# settings, statusline, keybindings, and those of skills, agents, team templates and installed
# plugins the laptop has. Runtime state (history, transcripts, sessions, caches, telemetry,
# credentials, sync state) stays behind. A project's memory copies only while the box has none for
# it, so memories box sessions wrote are never overwritten.
# Usage: sync-claude.sh <ssh destination>
set -euo pipefail
shopt -s nullglob
[[ $# -eq 1 ]] || { echo "usage: $0 <ssh destination>" >&2; exit 1; }
destination="$1"
src="$HOME/.claude"

files=()
for file in "$src"/*.md "$src"/settings.json "$src"/statusline-command.sh "$src"/keybindings.json; do
  if [[ -f "$file" ]]; then
    files+=("$file")
  fi
done

ssh "$destination" 'mkdir -p ~/.claude/projects'
if [[ "${#files[@]}" -gt 0 ]]; then
  rsync -a "${files[@]}" "$destination:.claude/"
fi
for dir in agents team-templates; do
  if [[ -d "$src/$dir" ]]; then
    rsync -a --delete "$src/$dir/" "$destination:.claude/$dir/"
  fi
done
# skills/synced and plugins/synced are claude.ai sync state; the box's Claude syncs its own.
if [[ -d "$src/skills" ]]; then
  rsync -a --delete --exclude /synced "$src/skills/" "$destination:.claude/skills/"
fi
if [[ -d "$src/plugins" ]]; then
  rsync -a --delete --exclude /synced --exclude /data --exclude /store --exclude '/*cache*.json' \
    "$src/plugins/" "$destination:.claude/plugins/"
fi

present="$(ssh "$destination" bash -s <<'REMOTE'
shopt -s nullglob
for memory in ~/.claude/projects/*/memory; do
  basename "$(dirname "$memory")"
done
REMOTE
)"
for memory in "$src"/projects/*/memory; do
  project="$(basename "$(dirname "$memory")")"
  if grep -qxF -- "$project" <<<"$present"; then
    echo "kept the box's memory of $project"
    continue
  fi
  ssh "$destination" mkdir -p ".claude/projects/$project"
  rsync -a "$memory" "$destination:.claude/projects/$project/"
  echo "copied memory of $project"
done
