#!/usr/bin/env bash
# Installs the box OS from the Hetzner Rescue System, wiping the drives installimage.conf names,
# writes the installed system's ED25519 host key to HOST_KEY_OUT, then reboots into it.
# Verify the rescue host key before running this; its session is how the new key is trusted.
# Usage: install-os.sh [-F ssh_config] <rescue ssh destination> <hostname> <host key out>
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
ssh_options=(-o BatchMode=yes)
while getopts F: opt; do
  case "$opt" in
    F) ssh_options+=(-F "$OPTARG") ;;
    *) exit 1 ;;
  esac
done
shift $((OPTIND - 1))
[[ $# -eq 3 ]] || { echo "usage: $0 [-F ssh_config] <rescue ssh destination> <hostname> <host key out>" >&2; exit 1; }
destination="$1"
hostname="$2"
host_key_out="$3"
[[ "$hostname" =~ ^[a-z0-9][a-z0-9-]*$ ]] || { echo "$0: invalid hostname $hostname" >&2; exit 1; }

scp "${ssh_options[@]}" "$here/installimage.conf" "$destination:/tmp/installimage.conf"
host_key="$(ssh "${ssh_options[@]}" "$destination" bash -s -- "$hostname" <<'REMOTE'
set -euo pipefail
hostname="$1"
installimage=/root/.oldroot/nfs/install/installimage
[[ -x "$installimage" ]] || { echo "not a Hetzner Rescue System: $installimage missing" >&2; exit 1; }
mapfile -t drives < <(awk '$1 ~ /^DRIVE[0-9]+$/ {print $2}' /tmp/installimage.conf)
[[ "${#drives[@]}" -eq 2 ]] || { echo "installimage.conf must name exactly two drives" >&2; exit 1; }
for drive in "${drives[@]}"; do
  [[ "$(lsblk -dno TYPE,RM "$drive" | tr -s ' ')" == "disk 0" ]] || { echo "$drive is not a fixed disk" >&2; exit 1; }
  if lsblk -no MOUNTPOINT "$drive" | grep -q .; then
    echo "$drive has mounted filesystems" >&2
    exit 1
  fi
done
others="$(lsblk -dnpo NAME,TYPE | awk -v drives="${drives[*]}" 'BEGIN {split(drives, d, " "); for (i in d) skip[d[i]] = 1} $2 == "disk" && !($1 in skip) {printf "%s ", $1}')"
echo "installing on ${drives[*]}; untouched: ${others:-none}" >&2
{
  printf 'HOSTNAME %s\n' "$hostname"
  cat /tmp/installimage.conf
} >/tmp/installimage.box.conf
TERM=xterm "$installimage" -a -c /tmp/installimage.box.conf </dev/null >&2

mnt="$(mktemp -d)"
keys=()
for md in /dev/md[0-9]*; do
  [[ "$(blkid -o value -s TYPE "$md")" == ext4 ]] || continue
  mount -o ro "$md" "$mnt"
  if [[ -f "$mnt/etc/ssh/ssh_host_ed25519_key.pub" ]]; then
    keys+=("$(cat "$mnt/etc/ssh/ssh_host_ed25519_key.pub")")
  fi
  umount "$mnt"
done
[[ "${#keys[@]}" -eq 1 ]] || { echo "expected one installed ED25519 host key, found ${#keys[@]}" >&2; exit 1; }
printf '%s\n' "${keys[0]}"
REMOTE
)"
printf '%s\n' "$host_key" >"$host_key_out"
ssh-keygen -lf "$host_key_out"
ssh "${ssh_options[@]}" "$destination" 'nohup sh -c "sleep 2; reboot" >/dev/null 2>&1 &'
echo "rebooting into the installed system; its host key is in $host_key_out"
