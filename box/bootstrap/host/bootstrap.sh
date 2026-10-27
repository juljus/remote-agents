#!/usr/bin/env bash
# Base setup of the box, run as root on it by provision.sh; idempotent. Installs packages, the
# pinned toolchain (tools.sh), Google Chrome, Tailscale and unattended security upgrades, creates
# the box user with zsh, passwordless sudo, the staged authorized_keys, lingering and the session
# workdir, makes SSH key-only for that user alone, and loads the public NIC firewall. SSH stays
# reachable on the public NIC only with the argument `open`. The values come from the staged
# box.env; the overlay's hooks in local/*.sh run last, in name order, with those values exported.
# Usage: bootstrap.sh open|closed
set -euo pipefail
shopt -s inherit_errexit

here="$(cd "$(dirname "$0")" && pwd)"

die() { echo "bootstrap.sh: $*" >&2; exit 1; }

[[ $# -eq 1 && "$1" =~ ^(open|closed)$ ]] || die "usage: bootstrap.sh open|closed"
public_ssh="$1"
[[ "$(id -u)" -eq 0 ]] || die "run as root"
set -a
# shellcheck source=/dev/null
source "$here/box.env"
set +a
: "${BOX_USER:?}" "${BOX_HOME:?}" "${BOX_WORKDIR:?}"

export DEBIAN_FRONTEND=noninteractive
install -d -m 0755 /usr/share/keyrings
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg \
  -o /usr/share/keyrings/tailscale-archive-keyring.gpg
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list \
  -o /etc/apt/sources.list.d/tailscale.list
# Google Chrome for the Playwright MCP, whose default browser is the chrome channel.
CHROME_KEY_FINGERPRINT=EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796
chrome_key="$(mktemp)"
curl -fsSL https://dl.google.com/linux/linux_signing_key.pub -o "$chrome_key"
[[ "$(gpg --show-keys --with-colons "$chrome_key" | awk -F: '$1 == "fpr" {print $10; exit}')" == "$CHROME_KEY_FINGERPRINT" ]] ||
  die "Google's apt signing key does not have fingerprint $CHROME_KEY_FINGERPRINT"
gpg --dearmor --yes -o /usr/share/keyrings/google-chrome.gpg "$chrome_key"
rm -f "$chrome_key"
# The package's own maintainer script rewrites this file in the same form on every upgrade.
install -m 0644 "$here/google-chrome.sources" /etc/apt/sources.list.d/google-chrome.sources
install -m 0644 "$here/52unattended-upgrades-chrome" /etc/apt/apt.conf.d/52unattended-upgrades-chrome
apt-get update
apt-get -y upgrade
apt-get install -y \
  build-essential git curl jq rsync unzip zip xz-utils htop tmux strace lsof ripgrep zsh \
  nftables unattended-upgrades tailscale google-chrome-stable
install -m 0644 "$here/20auto-upgrades" /etc/apt/apt.conf.d/20auto-upgrades
bash "$here/tools.sh"

install -d -m 0755 "$(dirname "$BOX_HOME")"
if ! id "$BOX_USER" >/dev/null 2>&1; then
  useradd --create-home --home-dir "$BOX_HOME" --shell /usr/bin/zsh --groups sudo "$BOX_USER"
fi
usermod --shell /usr/bin/zsh "$BOX_USER"
current_home="$(getent passwd "$BOX_USER" | cut -d: -f6)"
[[ "$current_home" == "$BOX_HOME" ]] || die "$BOX_USER's home is $current_home, expected $BOX_HOME"
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$BOX_USER" >"/etc/sudoers.d/90-$BOX_USER"
chmod 0440 "/etc/sudoers.d/90-$BOX_USER"
visudo -cf "/etc/sudoers.d/90-$BOX_USER"
install -d -m 0700 -o "$BOX_USER" -g "$BOX_USER" "$BOX_HOME/.ssh"
install -m 0600 -o "$BOX_USER" -g "$BOX_USER" "$here/authorized_keys" "$BOX_HOME/.ssh/authorized_keys"
# The workdir and every directory between it and the home belong to the box user.
dir="$BOX_HOME"
IFS=/ read -ra parts <<<"${BOX_WORKDIR#"$BOX_HOME"/}"
for part in "${parts[@]}"; do
  dir="$dir/$part"
  install -d -m 0755 -o "$BOX_USER" -g "$BOX_USER" "$dir"
done
loginctl enable-linger "$BOX_USER"

sed -e "s/@BOX_USER@/$BOX_USER/g" "$here/sshd.conf.in" >"$here/sshd.conf"
install -m 0644 "$here/sshd.conf" /etc/ssh/sshd_config.d/10-box.conf
# Socket-activated ssh.service creates /run/sshd only when it starts; sshd -t needs it.
install -d -m 0755 /run/sshd
sshd -t
systemctl try-reload-or-restart ssh.service
rm -f /root/.ssh/authorized_keys

mapfile -t public_nics < <({ ip -o -4 route show default; ip -o -6 route show default; } |
  awk '{for (i = 1; i < NF; i++) if ($i == "dev") print $(i + 1)}' | sort -u)
[[ "${#public_nics[@]}" -eq 1 ]] || die "expected one default-route NIC, found: ${public_nics[*]}"
ssh_rule=""
if [[ "$public_ssh" == open ]]; then
  ssh_rule="tcp dport 22 accept"
fi
install -d -m 0755 /etc/box-firewall
sed -e "s/@PUBLIC_NIC@/${public_nics[0]}/g" -e "s/@PUBLIC_SSH@/$ssh_rule/" "$here/firewall.nft.in" >"$here/firewall.nft"
nft -c -f "$here/firewall.nft"
install -m 0644 "$here/firewall.nft" /etc/box-firewall/firewall.nft
install -m 0644 "$here/box-firewall.service" /etc/systemd/system/box-firewall.service
systemctl daemon-reload
systemctl enable box-firewall.service
systemctl reload-or-restart box-firewall.service

systemctl enable --now tailscaled
tailscale set --hostname="$(hostname)" --accept-routes=true --ssh=false
if [[ "$(tailscale status --json | jq -r .BackendState)" == NeedsLogin ]]; then
  echo "Tailscale is logged out: run 'sudo tailscale login' on the box and open its URL"
fi

shopt -s nullglob
for hook in "$here"/local/*.sh; do
  echo "running hook local/$(basename "$hook")"
  bash "$hook"
done
echo "bootstrap OK: user $BOX_USER, public NIC ${public_nics[0]}, public ssh $public_ssh, tailscale $(tailscale version | head -1)"
