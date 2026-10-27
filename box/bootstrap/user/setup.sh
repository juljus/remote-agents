#!/usr/bin/env bash
# Shell and git setup of the box user, run as that user through run-user.sh; idempotent. Installs
# the zsh config (with the overlay's zshrc.local as ~/.zshrc.local) and pre-commit, sets the git
# identity from bootstrap.env and, once gh is logged in, makes git reach GitHub over HTTPS through
# gh, ssh-style remotes included.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"

install -m 0644 "$here/zshrc" "$HOME/.zshrc"
if [[ -f "$here/zshrc.local" ]]; then
  install -m 0644 "$here/zshrc.local" "$HOME/.zshrc.local"
fi
uv tool install --quiet pre-commit==4.6.0

git config --global user.name "${GIT_USER_NAME:?}"
git config --global user.email "${GIT_USER_EMAIL:?}"
git config --global url."https://github.com/".insteadOf "git@github.com:"
gh auth status --hostname github.com >/dev/null || { echo "setup.sh: gh is not logged in: run 'gh auth login' on the box" >&2; exit 1; }
gh auth setup-git --hostname github.com
echo "user setup OK"
