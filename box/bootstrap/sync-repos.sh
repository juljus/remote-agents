#!/usr/bin/env bash
# Mirrors laptop git repos onto the box at the same paths relative to the home, with no GitHub
# access needed. SYNC_REPOS in bootstrap.env lists home-relative globs; every git repo they match is
# synced, a repo before the repos nested in it. A repo missing on the box is seeded from the laptop
# clone: origin's refs and tags, every local branch that origin also has as a local branch with its
# upstream, origin set to the laptop's URL and the laptop's checkout (branch or detached commit)
# checked out; a later `git fetch` brings it up to origin. Every local branch also goes to
# refs/remotes/laptop/<branch> on each run, never touching the box's own branches and never GitHub.
# Counts, per repo, the branches with commits that no origin ref (as last fetched on the laptop)
# contains; on the box, `git branch -r --list 'laptop/*'` names them all.
# The overlay's local/sync-repos.sh, if present, runs last on the laptop with the ssh destination.
# Usage: sync-repos.sh <ssh destination>
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
[[ $# -eq 1 ]] || { echo "usage: $0 <ssh destination>" >&2; exit 1; }
destination="$1"
# shellcheck source=box/bootstrap/env.sh
source "$here/env.sh"
require_env SYNC_REPOS

repos=()
for pattern in "${SYNC_REPOS[@]}"; do
  [[ "$pattern" != /* ]] || die "SYNC_REPOS pattern $pattern must be relative to \$HOME"
  matched=0
  while read -r dir; do
    dir="${dir%/}"
    if [[ -d "$dir/.git" ]]; then
      repos+=("${dir#"$HOME"/}")
      matched=1
    fi
  done < <(compgen -G "$HOME/$pattern")
  [[ "$matched" -eq 1 ]] || die "SYNC_REPOS pattern $pattern matches no git repo under $HOME"
done
# A repo sorts before the repos nested in it.
sorted=()
while read -r repo; do
  sorted+=("$repo")
done < <(printf '%s\n' "${repos[@]}" | LC_ALL=C sort -u)

# seed_start repo: creates an empty repo at <repo>.seeding on the box, bare while it
# receives, and pushes origin's refs, tags and the local branches origin has. A leftover .seeding
# dir is this script's own interrupted seed and starts over.
seed_start() {
  local repo="$1" branch
  local heads=()
  while read -r branch; do
    if git -C "$HOME/$repo" show-ref --verify --quiet "refs/remotes/origin/$branch"; then
      heads+=("refs/heads/$branch:refs/heads/$branch")
    fi
  done < <(git -C "$HOME/$repo" for-each-ref --format='%(refname:lstrip=2)' refs/heads)
  ssh "$destination" bash -s -- "$repo.seeding" "$(git -C "$HOME/$repo" rev-parse --is-shallow-repository)" <<'REMOTE'
set -euo pipefail
rm -rf "$1"
git init --quiet "$1"
git -C "$1" config core.bare true
# A shallow laptop clone can only seed a shallow box clone; git fetch --unshallow deepens it.
if [[ "$2" == true ]]; then
  git -C "$1" config receive.shallowUpdate true
fi
REMOTE
  # Separate pushes: the box's receive-pack fails a shallow push that updates a branch and its
  # origin ref together ("remote failed to report status").
  git -C "$HOME/$repo" push --quiet "$destination:$repo.seeding" \
    'refs/remotes/origin/*:refs/remotes/origin/*' '^refs/remotes/origin/HEAD'
  git -C "$HOME/$repo" push --quiet "$destination:$repo.seeding" 'refs/tags/*:refs/tags/*'
  if [[ "${#heads[@]}" -gt 0 ]]; then
    git -C "$HOME/$repo" push --quiet "$destination:$repo.seeding" "${heads[@]}"
  fi
}

# seed_finish repo: makes the seeded repo a working clone like the laptop's and moves it into
# place; an empty directory there, such as the session workdir, gives way to it.
seed_finish() {
  local repo="$1" kind=branch at
  # ssh joins arguments into one command line, so an empty one would vanish: always send a kind.
  if ! at="$(git -C "$HOME/$repo" symbolic-ref --quiet --short HEAD)"; then
    kind=detached
    at="$(git -C "$HOME/$repo" rev-parse HEAD)"
  fi
  ssh "$destination" bash -s -- "$repo" "$(git -C "$HOME/$repo" remote get-url origin)" "$kind" "$at" <<'REMOTE'
set -euo pipefail
target="$1"
cd "$target.seeding"
git config core.bare false
git remote add origin "$2"
while read -r branch; do
  if git show-ref --verify --quiet "refs/remotes/origin/$branch"; then
    git config "branch.$branch.remote" origin
    git config "branch.$branch.merge" "refs/heads/$branch"
  fi
done < <(git for-each-ref --format='%(refname:lstrip=2)' refs/heads)
case "$3" in
  branch)
    if ! git show-ref --verify --quiet "refs/heads/$4"; then
      git branch --quiet "$4" "refs/remotes/laptop/$4"
    fi
    git symbolic-ref HEAD "refs/heads/$4"
    ;;
  detached)
    git cat-file -e "$4^{commit}"
    git update-ref --no-deref HEAD "$4"
    ;;
esac
git reset --quiet --hard
cd - >/dev/null
if [[ -d "$target" ]]; then
  rmdir "$target"
fi
mv "$target.seeding" "$target"
echo "seeded $target at $3 $4"
REMOTE
}

# sync_repo repo: seeds the repo on the box if missing, mirrors the laptop branches into it and
# counts the branches not on origin.
sync_repo() {
  local repo="$1" seeded="" target total unpushed branch
  target="$repo"
  if ! ssh "$destination" test -e "$target/.git"; then
    seed_start "$repo"
    seeded=1
    target="$target.seeding"
  fi
  git -C "$HOME/$repo" push --quiet --force --prune "$destination:$target" 'refs/heads/*:refs/remotes/laptop/*'
  if [[ -n "$seeded" ]]; then
    seed_finish "$repo"
  fi
  total=0
  unpushed=0
  while read -r branch; do
    total=$((total + 1))
    if [[ "$(git -C "$HOME/$repo" rev-list --count "refs/heads/$branch" --not --remotes=origin)" -gt 0 ]]; then
      unpushed=$((unpushed + 1))
    fi
  done < <(git -C "$HOME/$repo" for-each-ref --format='%(refname:lstrip=2)' refs/heads)
  if [[ "$unpushed" -gt 0 ]]; then
    echo "$repo: $unpushed of $total branches not on origin"
  fi
}

for repo in "${sorted[@]}"; do
  sync_repo "$repo"
done
if [[ -f "$here/local/sync-repos.sh" ]]; then
  bash "$here/local/sync-repos.sh" "$destination"
fi
