# shellcheck shell=bash
# Pinned-download helpers, sourced by tools.sh and the overlay's host hooks, run as root. Each
# archive is checked against its sha256; a tool reinstalls only when its pin changes.

STAMPS=/var/lib/box-tools
install -d -m 0755 "$STAMPS" /usr/local/lib

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# fetch name url sha256: downloads url into a fresh $work/name directory and verifies it; prints the
# archive path, or nothing when the stamp says this sha256 is already installed.
fetch() {
  local name="$1" url="$2" sha256="$3"
  if [[ -f "$STAMPS/$name" && "$(cat "$STAMPS/$name")" == "$sha256" ]]; then
    return 0
  fi
  rm -rf "${work:?}/$name"
  mkdir -p "$work/$name"
  local archive
  archive="$work/$name/$(basename "$url")"
  curl -fsSL -o "$archive" "$url"
  echo "$sha256  $archive" | sha256sum -c --quiet -
  printf '%s\n' "$archive"
}

stamp() { printf '%s\n' "$2" >"$STAMPS/$1"; echo "installed $1"; }

# install_tree name dir: replaces /usr/local/lib/name with dir.
install_tree() {
  rm -rf "/usr/local/lib/$1.new"
  mv "$2" "/usr/local/lib/$1.new"
  rm -rf "/usr/local/lib/$1"
  mv "/usr/local/lib/$1.new" "/usr/local/lib/$1"
}
