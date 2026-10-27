# Box bootstrap

Builds a remote-agents box from a Hetzner dedicated server in the Rescue System: Ubuntu 24.04 on
NVMe RAID1, one key-only SSH user, Tailscale, and a firewall that leaves nothing on the public NIC
but WireGuard and ICMP once Tailscale works. Hetzner Rescue is the recovery path. On another
provider, skip step 1 and start from a fresh Ubuntu 24.04.

## Personal values

Copy `bootstrap.env.example` to `bootstrap.env` (gitignored) and fill in every value: the box user,
its home and workdir, the public keys allowed in, the git identity and the repos `sync-repos.sh`
mirrors. The scripts refuse to run while a value is missing or malformed.

## Steps

1. Verify the rescue host key against Hetzner's delivery mail, then
   `install-os.sh root@<ip> <hostname> <host key out>`. It wipes the two NVMe drives in
   `installimage.conf` (RAID1), leaves other drives alone and writes the installed system's host
   key, read through the rescue session, to `<host key out>`. Pin that key.
2. `provision.sh -p root@<ip>`: packages, the pinned toolchain, unattended security upgrades, the
   box user with passwordless sudo, key-only SSH without root, the firewall with public SSH still
   open, Tailscale prefs. Root login ends during this run; repeat runs go as `<user>@<host>`.
3. On the box, `sudo tailscale login`, and open its URL as the box's owner.
4. Add the laptop's `Host` entry for the tailnet name and pin the host key for it, then
   `provision.sh <tailnet host>` without `-p` closes public SSH.
   Check from the laptop that `ssh <user>@<public ip>` now fails.
5. `make deploy-box HOST=<tailnet host>` installs remote-agents and Claude Code.

The user's setup, from the laptop, each step rerunnable:

6. `sync-claude.sh <host>`: Claude Code instructions, settings, skills, agents, plugins and
   project memories (only those the box lacks).
7. On the box, `gh auth login --web --scopes repo,read:org,workflow`; then
   `run-user.sh <host> setup.sh` installs the zsh config, pre-commit, the git identity and git
   over gh.
8. `sync-repos.sh <host>`: every repo `SYNC_REPOS` matches, at the same path under the box home,
   and the laptop's branches as `refs/remotes/laptop/*`.
9. `copy-transcripts.sh <old host> <old project dir> <host> <project dir>` moves sessions over for
   `claude --resume`.

`provision.sh` is idempotent; rerun it after changing anything in `host/` or `bootstrap.env`.

## Local overlay

Setup that only concerns you goes in `local/` (gitignored), which the scripts pick up when present:

- `local/host/*.sh` run as root at the end of `host/bootstrap.sh`, in name order, with the
  `bootstrap.env` values exported. `../tools-lib.sh` beside them has the pinned-download helpers.
- `local/user/` is staged beside `user/` by `run-user.sh`: its scripts run like `user/` ones, and
  `setup.sh` installs its `zshrc.local` as `~/.zshrc.local`. A name `user/` already has is refused.
- `local/sync-repos.sh` runs on the laptop after `sync-repos.sh` mirrors the repos, with the ssh
  destination as its argument, e.g. to copy untracked files such as a `.env`.
