#!/usr/bin/env bash
# Builds the shpool the box runs: upstream v$UPSTREAM_VERSION plus patches/*.patch, as a static
# x86_64 musl binary, in a pinned image with fixed paths so that every build is byte-identical and
# install.sh can pin it by sha256. Needs docker.
# Usage: build.sh <out-file>
set -euo pipefail
shopt -s inherit_errexit

UPSTREAM_VERSION=0.11.5
UPSTREAM_COMMIT=3c41df9a610428b6c1766d78d36d3fefd5685c3b
UPSTREAM_REPO=https://github.com/shell-pool/shpool.git

die() { echo "build.sh: $*" >&2; exit 1; }

[[ $# -eq 1 ]] || die "usage: build.sh <out-file>"
OUT="$1"
DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE="remote-agents-shpool-build:$(sha256sum "$DIR/Dockerfile" | cut -c1-12)"

docker build -q -t "$IMAGE" "$DIR" >/dev/null
out_dir="$(mktemp -d)"
trap 'rm -rf "$out_dir"' EXIT
docker run --rm --user "$(id -u):$(id -g)" --tmpfs /build:exec,mode=1777 \
  -e HOME=/build -e CARGO_HOME=/build/cargo -e RUSTUP_TOOLCHAIN=1.85.0 \
  -v "$DIR/patches:/patches:ro" -v "$out_dir:/out" -w /build "$IMAGE" bash -euo pipefail -c "
    git -c advice.detachedHead=false clone -q --branch v$UPSTREAM_VERSION --depth 1 $UPSTREAM_REPO src
    [[ \$(git -C src rev-parse HEAD) == $UPSTREAM_COMMIT ]] || { echo 'v$UPSTREAM_VERSION is not $UPSTREAM_COMMIT' >&2; exit 1; }
    git -C src apply /patches/*.patch
    cd src
    cargo build -q --release --locked --target x86_64-unknown-linux-musl -p shpool
    cp target/x86_64-unknown-linux-musl/release/shpool /out/shpool"
install -m 0755 "$out_dir/shpool" "$OUT"
