# Install and operations

The precise reference for installing, deploying, testing and running remote-agents: everything
the [README](../README.md) leaves out. [Accounts](accounts.md) and the
[security model](security.md) have their own pages.

## How it fits together

- **Laptop:** one binary, `remote-agents`, installed with `rclaude` and `rcodex` as symlinks to
  it; the name `r<agent>` picks `[agents.<agent>]` from the config. A launchd agent,
  `com.remote-agents.link`, runs `remote-agents link`: the link daemon that holds the SSH link
  and serves the box's clipboard, browser, login, file and cmux requests.
- **Box:** `remote-agents-box`, the session launcher and the busybox-style shims (`xclip`,
  `wl-paste`, `xdg-open`, `laptop-save`, and `cmux` for hooks), plus shpool. Sessions live in
  [shpool](https://github.com/shell-pool/shpool) on the box, so a closed lid, a network change or
  a dropped SSH link does not stop them: the laptop reconnects on its own. While the link is up,
  the link daemon gives box sessions the laptop's clipboard and browser, carries OAuth logins
  started on the box back to it, and forwards status events to cmux if you use it.

## Requirements

- Laptop: macOS (the link daemon runs under launchd), Go to build, `ssh` to the box without a
  password prompt (keys or an agent). cmux is optional; `make lint` needs staticcheck and
  shellcheck.
- Box: Linux x86_64 with systemd, a user with lingering enabled (`loginctl enable-linger <user>`)
  who can run docker, which `make deploy-box` uses to build shpool.
  `make deploy-box` installs shpool and Claude Code there. Codex comes with the bootstrap's pinned
  toolchain below; on another box, put `codex` on `/usr/local/bin` yourself.

`box/bootstrap/` builds such a box from scratch on a Hetzner dedicated server: one key-only SSH
user, Tailscale and a closed public NIC. See [its README](../box/bootstrap/README.md).

## Box install

From the laptop; idempotent, and it never restarts the shpool daemon:

    make deploy-box HOST=<ssh host>

`scripts/deploy-box.sh <host> <name>` does the work, with the install name `remote-agents`. It
builds `remote-agents-box` for linux/amd64, copies it and `box/` into the stage dir
`~/.cache/<name>-stage` on the box (deleting what was there first), runs `box/install.sh` from
it as the box user, and removes the stage dir on success.

It installs shpool, `remote-agents-box`, the clipboard, browser and hook shims, a
`systemctl --user` socket for shpool, and Claude Code if missing (from `claude.ai/install.sh`
into `~/.local/bin/claude`). It ends by asking the shpool daemon for its session list and prints
`remote-agents box install <name> OK` with the shpool and Claude versions.

### Install names

Every install on the box has a name (`[a-z0-9][a-z0-9-]*`) and its own files, shpool daemon and
sessions:

| What | Where on the box |
| --- | --- |
| Binaries: `bin/remote-agents-box`, `bin/shpool`, `shims/`, `hookbin/` | `~/.local/share/<name>/` |
| shpool socket, run records, activity, pasted copies (`session-pastes/`), saved files' records (`session-saves/`), `reaper.log` | `~/.local/state/<name>/` (mode 0700) |
| `shpool.toml`, `session.toml`, `reaper.toml`, `accounts.toml`, the `accounts/` vault | `~/.config/<name>/` |
| `<name>-shpool.socket`, `<name>-shpool.service` | `~/.config/systemd/user/` |
| Stage dir during a deploy | `~/.cache/<name>-stage` |
| Built shpool, shared by every install | `~/.cache/remote-agents/shpool-<sha256>` |

`remote-agents` is the production install. Only it gets `remote-agents-accounts-refresh.service`
and `.timer`; deploying any other name removes that timer if it has one. The laptop's
`box_install` config key picks the install it talks to; the e2e tests use `remote-agents-e2e`.

### shpool

The shpool is upstream's pinned release plus the fixes in `box/shpool/patches`, built on the box
with docker into a static binary that `box/install.sh` pins by sha256 (`SHPOOL_VERSION`,
`SHPOOL_SHA256`); after a change there, run `box/shpool/build.sh` on the box and put the sha256 it
gives into the pin. The install refuses a build whose sha256 differs from the pin.

A new shpool takes over from the next start of the shpool daemon, and restarting it ends every
session, so restart `remote-agents-shpool.service` on the box once no session runs. Until then
the old daemon keeps serving all sessions; the install prints a NOTE when that is the case. A
session the old daemon has wedged (every attach fails) cannot be recovered: stop its systemd
scope (`systemctl --user stop <name>-<session>-<instance>.scope`), then `rclaude --resume <id>`.

### Logging in on the box

Log in to Claude once on the box through a session (`rclaude`, then `/login`), and to Codex with
`rcodex login`, whose browser callback comes back through the link, or
`rcodex login --device-auth`. For more than one account, see [Accounts](accounts.md).

## Laptop install

    make install                                   # ~/.local/bin/remote-agents, rclaude, rcodex
    mkdir -p ~/.config/remote-agents
    cp config.example.toml ~/.config/remote-agents/config.toml   # then set host and workdir
    cp bin/com.remote-agents.link.plist ~/Library/LaunchAgents/
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.remote-agents.link.plist

`make install` builds both binaries, installs `remote-agents` with its `rclaude` and `rcodex`
links into `~/.local/bin` (`BINDIR`), and writes `bin/com.remote-agents.link.plist` for that
path. It loads nothing.

No shell setup is needed beyond having `~/.local/bin` (or the `BINDIR` you install to) on your
`PATH`. Stop the link daemon with
`launchctl bootout gui/$(id -u)/com.remote-agents.link`.

### Configuration

`~/.config/remote-agents/config.toml`, or the file `REMOTE_AGENTS_CONFIG` names. Unknown keys
are refused, and at least one `[agents.<name>]` table is required. See `config.example.toml`.

| Key | Meaning |
| --- | --- |
| `host` | ssh destination of the box; required |
| `workdir` | absolute path on the box every session runs in; required; a session fails to start while it does not exist |
| `box_install` | install name on the box; default `remote-agents` |
| `[link] opener` | absolute path of the laptop command that opens box URLs; default `/usr/bin/open` |
| `[link] revealer` | laptop command that shows a file `laptop-save` saved; default `["/usr/bin/open", "-R"]` |
| `[cmux] wrapper` | only with cmux: its `cmux-claude-wrapper` script, which decides which hooks to inject |
| `[agents.<name>]` | one table per `r<name>`: `command` (resolved on the box's session PATH), `kind` (`claude` or `codex`), `args` |

Arguments every run should get go in the agent's `args` in the config. Join option values with
`=` (`--model=opus`): Claude reads a separate value of some options as a list that would take
your prompt. The example config runs both agents without permission prompts or sandbox
(`--dangerously-skip-permissions`, `--yolo`): the box is the sandbox. Drop `args` to get the
prompts back.

### Logs

- `~/Library/Logs/remote-agents/link.log`: the link daemon.
- `~/Library/Logs/remote-agents/sessions.log`: what session wrappers cannot show over the agent's
  screen, such as hook relay problems.
- Box: `~/.local/state/<name>/reaper.log`, and `journalctl --user -u <name>-shpool.service`.

Laptop state lives in `~/.local/state/remote-agents/`: the wrappers' `control.sock` to the
daemon, and `pasted/`, the record of files pasted into each session.

## Upgrading

Deploy the box first, then the laptop: the box refuses a session spec with fields it does not
know, so a laptop newer than its box fails to start or reconnect sessions. A change to the link
protocol (`internal/linkproto`) needs both deploys.

After `make install`, restart the link daemon to load the new binary with `launchctl bootout`
followed by `launchctl bootstrap` (above). `launchctl kickstart -k` has left it "spawn scheduled"
with exit 78 (EX_CONFIG) and the link down for over a minute.

Running sessions keep their wrapper's old spec; a box-side setting belongs on the box (as in
`session.toml`), never in the spec, or live wrappers fail to reconnect.

## Sessions on the box

### Memory limits

Each session on the box runs in a systemd scope of its own, capped at 64G of memory and no swap:
the OOM killer then ends only the process that overran it. Set the cap in
`~/.config/remote-agents/session.toml` on the box, as `memory_max = "32G"` (K, M, G or T); it
applies to sessions started after the change. The shpool service itself keeps running when a
session is OOM-killed.

### Idle detached sessions

A session nobody is attached to is ended once it has been idle for 5 minutes, or 36 hours after
it was detached even while working. A turn waiting on its user counts as idle; an open turn, a
running tool, a busy process a tool left behind, a background task or a scheduled task counts as
working. Ending it also ends the processes its tools left behind, but not daemons that detached
themselves. When its wrapper reconnects, or on `rclaude --resume <id>` or
`remote-agents attach <session>`, the agent session resumes with
`resumed; the session had been ended for inactivity` (or `after a long time detached`, for the
36-hour cap). `remote-agents ls` also lists the sessions reaped in the last 7 days, with the
command that resumes each. A session whose agent session id is not known yet is never reaped.

Override the policy in `~/.config/<name>/reaper.toml` with Go durations: `idle_after`,
`working_cap`, `poll` (default 15s, at most `idle_after`). Each reap is logged to
`~/.local/state/<name>/reaper.log`.

### Lost sessions

A session ends when its agent exits. If the box loses it instead, say the shpool daemon stops,
the wrapper reconnects and resumes the same agent session; an agent that takes over 10 seconds
to exit on that stop is killed, which ends the session for an attached wrapper. A Codex lost
before its first turn has no session yet and starts again.

## Using it

- `rclaude [args]` starts a session in the configured workdir; `rclaude --resume <id>` resumes
  one, reattaching if it is still running. After Claude exits, `rclaude` prints how to resume the
  session on the box (Claude's own `claude --resume` line would look for it locally). With
  `--continue` or the resume picker the id comes from the SessionStart hook cmux injects; outside
  cmux only `rclaude --resume <id>` gets the line, since a new session may never have started.
- `rcodex [args]` starts Codex the same way, and `rcodex resume <id>` resumes a session. Codex
  picks a new session's id at its first turn, so `rcodex` prints the resume line only when the
  laptop knows the id: when it resumed one, or inside cmux, which relays the hooks reporting it.
  The box gives Codex its hooks with `-c`, trusted so Codex runs them without asking, and leaves
  `~/.codex/config.toml` alone; arguments that would replace or disable those hooks are refused.
  Each session runs `codex --no-daemon`, so it never shares Codex's background server.
- `remote-agents run <agent> [--] [args]` is the long form of `r<agent>`.
- A cloud in the terminal's bottom-right corner marks a remote session while it is shown. It is
  drawn only over blank cells, never over the agent's text, and goes when the session ends or
  detaches. A resize moves the drawn cloud wherever the terminal reflows it, beyond the
  wrapper's reach: Claude Code and Codex clear and redraw their screen on a resize, which erases
  it, and the cloud then returns to the new corner. Output that is not redrawn keeps the moved
  copy until it is overwritten.
- `remote-agents ls` lists sessions on the box; `remote-agents attach <session>` reattaches.
- While the link is down, Ctrl+C detaches; the session keeps running.

### Clipboard and browser

Inside a session, `xclip -selection clipboard` reads (`-o`) and sets the laptop clipboard, and
`wl-paste` reads it, images included (by MIME type, `-t`); other X selections are refused.
`xdg-open <url>` opens the URL in the laptop browser through the configured opener. A login whose
URL calls back to `localhost` gets its callback forwarded to the box; the [security
model](security.md#logins) has the rules.

### Pasted files and write-back

- Pasting or dragging laptop files into a session uploads each one the box lacks and pastes the
  path of the session's own copy instead,
  `~/.local/state/remote-agents/session-pastes/<session>/<key>/<name>`, so Claude can read it and
  attaches images. A path that exists on the box stays as typed: the box's own file wins.
  Directories and files over 100 MB are not uploaded; a notice on the bottom row says so.
- When the agent saves its copy of a pasted file, the box sends it back over the laptop file
  within about a second, and the laptop file's previous version goes to the Trash. If the laptop
  file changed since the paste (or since the last write-back), it stays as it is: the agent's
  version goes beside it as `name (box).ext`, with a notice on the bottom row. Paste the file
  again to write back over it once more. Plain files in your home are written back on their own;
  a hidden one, one under `~/Library`, outside your home, executable or reached through a symlink
  first asks for your Touch ID (or password), once per file and session. Copies changed while the
  link is down go when it is back. A copy goes 7 days after its last paste or change, once the
  laptop has it.

### laptop-save

`laptop-save <file> [<laptop path>]` on the box saves a file to the laptop once you approve it
with Touch ID (or your password), which it asks for each new place, `~/Downloads` included, once
per session. It goes into `~/Downloads` under its name, or a free `name (1).ext`, and shows it
in Finder (`[link] revealer`). A laptop path may name any place on the laptop (`~/` or
absolute; a relative one is in `~/Downloads`); a file it names is replaced, and the prompt says
so, its old version going to the Trash.

After a save from a session, the laptop file follows the box file for as long as a pasted copy
would: each later change goes over it within about a second, without asking again, under the
same conflict rule as a write-back (`name (box).ext` beside a file you changed since). Saving the
same box file again is one more such change, to the same laptop file, in a dir too. Another box
file saved to that laptop file is a save of its own, which asks unless that place's save was
approved already, and takes its place. Once the box file has been gone for about 30 seconds, the
laptop file stops following it and stays as it is. Each laptop file follows one box file per
session, which the laptop records: its pasted copy, or the box file saved to it, whichever of
the paste and the save came last; a file pasted into the session stays pasted when saved over.
The box keeps a record of each, and drops a saved one once the laptop refuses it as superseded.
After the link daemon restarts, the next change asks once more, naming the box file.

Agents learn all this from a note the box adds to their system prompt (Claude) or developer
instructions (Codex, after those of its `config.toml`). It says first that they run on the box,
whose paths mirror the laptop's on a separate disk, and that a file reaches the laptop only
through a write-back or `laptop-save`.

A declined prompt pauses every prompt for 2 minutes; see the [security model](security.md).

## Tests

    make test                      # unit tests, with the race detector
    make lint                      # go vet, staticcheck, shellcheck
    make e2e HOST=<ssh host>       # deploys to the box under a separate install name and
                                   # drives the laptop binaries against it in ~/remote-agents-test

`make e2e` deploys the box side as the install `remote-agents-e2e`, so it can run while the
production link daemon and sessions are up. It builds the laptop binary with the `e2e` tag, which
asks a fake approver (`RA_E2E_APPROVER`) instead of Touch ID, runs its own link daemon from a temp
dir under `/tmp`, drives fake and real agents through a pseudo-terminal, and sets the e2e
install's `memory_max` to 2G. The run's timeout is 280 s. The clipboard test replaces the real
laptop clipboard briefly, so it runs only with `RA_E2E_CLIPBOARD=1`.

## Pitfalls

- Two deploys of one install name at once share its stage dir: the second deletes it under the
  first. Two e2e runs against one box therefore break each other; run them one at a time.
- Scope process lookups in tests and scripts to your own processes (`pgrep -P`, `pgrep -x`).
  `pgrep -f <pattern>` also matches the shell running it: a `pgrep -f` wait loop has matched
  itself for two days, and a bare `pgrep -f link-hold` has killed a live link.
- Codex's own sandbox (bubblewrap) can fail on Ubuntu 24.04, whose AppArmor restricts
  unprivileged user namespaces; with `--yolo` the box is the sandbox instead.
