#!/usr/bin/env bash
# Installs the pinned developer toolchain into /usr/local, run as root by bootstrap.sh; idempotent.
# Agents get /usr/local/bin on PATH without any shell setup, which the MCP servers Claude Code
# starts rely on.
set -euo pipefail
shopt -s inherit_errexit
# shellcheck source=box/bootstrap/host/tools-lib.sh
source "$(dirname "$0")/tools-lib.sh"

GO_SHA256=ccfe7ae822d2c2f41f4fe7c956199967a4dac22d0ee0d2b2bfdb440aab3b20c5
archive="$(fetch go https://go.dev/dl/go1.26.10.linux-amd64.tar.gz "$GO_SHA256")"
if [[ -n "$archive" ]]; then
  tar -xzf "$archive" -C "$work/go"
  install_tree go "$work/go/go"
  ln -sfn ../lib/go/bin/go /usr/local/bin/go
  ln -sfn ../lib/go/bin/gofmt /usr/local/bin/gofmt
  stamp go "$GO_SHA256"
fi

NODE_SHA256=fd8e59d5a511510f6a298afb548f18c7d2b1be404d8b4a27d94fbe49f56cb2d6
archive="$(fetch node https://nodejs.org/dist/v24.21.0/node-v24.21.0-linux-x64.tar.xz "$NODE_SHA256")"
if [[ -n "$archive" ]]; then
  tar -xJf "$archive" -C "$work/node"
  install_tree node "$work/node/node-v24.21.0-linux-x64"
  for bin in node npm npx corepack; do
    ln -sfn "../lib/node/bin/$bin" "/usr/local/bin/$bin"
  done
  stamp node "$NODE_SHA256"
fi

BUN_SHA256=8611ba935af886f05a6f38740a15160326c15e5d5d07adef966130b4493607ed
archive="$(fetch bun https://github.com/oven-sh/bun/releases/download/bun-v1.3.11/bun-linux-x64.zip "$BUN_SHA256")"
if [[ -n "$archive" ]]; then
  unzip -q "$archive" -d "$work/bun"
  install -m 0755 "$work/bun/bun-linux-x64/bun" /usr/local/bin/bun
  ln -sfn bun /usr/local/bin/bunx
  stamp bun "$BUN_SHA256"
fi

UV_SHA256=c0f3236f146e55472663cfbcc9be3042a9f1092275bbe3fe2a56a6cbfd3da5ce
archive="$(fetch uv https://github.com/astral-sh/uv/releases/download/0.11.3/uv-x86_64-unknown-linux-gnu.tar.gz "$UV_SHA256")"
if [[ -n "$archive" ]]; then
  tar -xzf "$archive" -C "$work/uv"
  install -m 0755 "$work/uv/uv-x86_64-unknown-linux-gnu/uv" "$work/uv/uv-x86_64-unknown-linux-gnu/uvx" /usr/local/bin/
  stamp uv "$UV_SHA256"
fi

GH_SHA256=d0422caade520530e76c1c558da47daebaa8e1203d6b7ff10ad7d6faba3490d8
archive="$(fetch gh https://github.com/cli/cli/releases/download/v2.89.0/gh_2.89.0_linux_amd64.tar.gz "$GH_SHA256")"
if [[ -n "$archive" ]]; then
  tar -xzf "$archive" -C "$work/gh"
  install -m 0755 "$work/gh/gh_2.89.0_linux_amd64/bin/gh" /usr/local/bin/gh
  stamp gh "$GH_SHA256"
fi
