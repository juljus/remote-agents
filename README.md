# remote-agents

Run Claude Code or Codex on a remote Linux box while it feels local on your Mac.

`rclaude` in a laptop terminal starts Claude Code on the box, `rcodex` starts Codex; the laptop
only displays them. `claude` and `codex` stay your local ones. Sessions live in
[shpool](https://github.com/shell-pool/shpool) on the box, so a closed lid, a network change or a
dropped SSH link does not stop them: the laptop reconnects on its own. While the link is up, a
launchd daemon on the laptop gives box sessions the laptop's clipboard and browser, carries OAuth
logins started on the box back to it, and forwards status events to cmux if you use it.

Why: long agent runs want a machine that stays on, has cores and disk to spare, and is not your
laptop; you still want to work with them from your own terminal, clipboard and browser.

## Requirements

- Laptop: macOS (the link daemon runs under launchd), Go to build, `ssh` to the box without a
  password prompt (keys or an agent). cmux is optional; `make lint` needs staticcheck and
  shellcheck.
- Box: Linux x86_64 with systemd, a user with lingering enabled (`loginctl enable-linger <user>`)
  who can run docker, which `make deploy-box` uses to build shpool.
  `make deploy-box` installs shpool and Claude Code there. Codex comes with the bootstrap's pinned
  toolchain below; on another box, put `codex` on `/usr/local/bin` yourself.

`box/bootstrap/` builds such a box from scratch on a Hetzner dedicated server: one key-only SSH
user, Tailscale and a closed public NIC. See [its README](box/bootstrap/README.md).

## Install

Box, from the laptop; idempotent, and it never restarts the shpool daemon:

    make deploy-box HOST=<ssh host>

It installs shpool, `remote-agents-box`, the clipboard, browser and hook shims, a
`systemctl --user` socket for shpool, and Claude Code if missing. The shpool is upstream's pinned
release plus the fixes in `box/shpool/patches`, built on the box with docker into a static binary
that `box/install.sh` pins by sha256; after a change there, run `box/shpool/build.sh` on the box and
put the sha256 it gives into the pin. A new shpool takes over from the next start of the shpool
daemon, and restarting it ends every session, so restart `remote-agents-shpool.service` on the box
once no session runs. Until then the old daemon keeps serving all sessions. Log in to Claude once on the box
through a session (`rclaude`, then `/login`), and to Codex with `rcodex login`, whose browser
callback comes back through the link, or `rcodex login --device-auth`.

Laptop:

    make install                                   # ~/.local/bin/remote-agents and rclaude
    mkdir -p ~/.config/remote-agents
    cp config.example.toml ~/.config/remote-agents/config.toml   # then set host and workdir
    cp bin/com.remote-agents.link.plist ~/Library/LaunchAgents/
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.remote-agents.link.plist

Each session on the box runs in a systemd scope of its own, capped at 64G of memory and no swap:
the OOM killer then ends only the process that overran it. Set the cap in
`~/.config/remote-agents/session.toml` on the box, as `memory_max = "32G"` (K, M, G or T); it
applies to sessions started after the change.

No shell setup is needed. Arguments every run should get go in the agent's `args` in the config.
The example config runs both agents without permission prompts or sandbox
(`--dangerously-skip-permissions`, `--yolo`): the box is the sandbox. Drop `args` to get the prompts back.
Stop the link daemon with `launchctl bootout gui/$(id -u)/com.remote-agents.link`.
Logs: `~/Library/Logs/remote-agents/link.log` (daemon), `sessions.log` (hook relay problems).

## Use

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
- A cloud in the terminal's bottom-right corner marks a remote session while it is shown. It is
  drawn only over blank cells, never over the agent's text, and goes when the session ends or
  detaches. A resize moves the drawn cloud wherever the terminal reflows it, beyond the
  wrapper's reach: Claude Code and Codex clear and redraw their screen on a resize, which erases
  it, and the cloud then returns to the new corner. Output that is not redrawn keeps the moved
  copy until it is overwritten.
- `remote-agents ls` lists sessions on the box; `remote-agents attach <session>` reattaches.
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
- `laptop-save <file> [<laptop path>]` on the box saves a file to the laptop once you approve it
  with Touch ID (or your password), which it asks for each new place, `~/Downloads` included, once
  per session. It goes into `~/Downloads` under its name, or a free `name (1).ext`, and shows it
  in Finder (`[link] revealer`). A laptop path may name any place on the laptop (`~/` or
  absolute; a relative one is in `~/Downloads`); a file it names is replaced, and the prompt says
  so, its old version going to the Trash. Agents learn both from one line the box adds to their
  system prompt (Claude) or developer instructions (Codex, after those of its `config.toml`).
- While the link is down, Ctrl+C detaches; the session keeps running.
- A session ends when its agent exits. If the box loses it instead, say the shpool daemon stops,
  the wrapper reconnects and resumes the same agent session; an agent that takes over 10 seconds
  to exit on that stop is killed, which ends the session for an attached wrapper. A Codex lost
  before its first turn has no session yet and starts again.

## Accounts

The box can hold several Claude logins and switch between them; a switch applies to every
session at once, running ones included, from their next request to Claude.

- `rclaude account` opens a picker: each account with what is left of its weekly window and
  when it resets (`▰▰▱▱▱ 49% left · resets Thu 18:00`), the cursor on the active one. ↑/↓ or
  j/k move, 1-9 jump to a row, Enter switches, Esc, q or Ctrl+C leave. a adds an account, e
  renames the highlighted one, d removes it after a y/N. A login the box has that no account
  names shows as `unsaved`; e names it. A weekly window that has not started shows as
  `not started`, and s starts it: Claude Code sends that account one `hi` to the cheapest
  model, from an empty dir with none of the box's MCP servers, settings or hooks, and the
  picker reads the usage again to say whether the window started. Outside a terminal it prints
  the table instead.
- `rclaude account list` prints the table; `list --json` is the form for scripts.
- `rclaude account use <name>` switches without asking.
- `rclaude account save <name>` names the login the box has now.
- `rclaude account add <name>` logs in to another account: Claude's `auth login` runs on the
  box and opens its page in the laptop browser. Run it again for an account that was signed out.
- `rclaude account rename <name> <new name>` renames an account.
- `rclaude account remove <name>` forgets an account other than the active one.
- `rclaude account start <name>` sends the account its one request, as s does.

Claude Code keeps the box's login in `~/.claude/.credentials.json` (`claudeAiOauth`) and
`~/.claude.json` (`oauthAccount`). A switch replaces only those two keys, under Claude Code's own
locks, and only if no session refreshed or replaced the login meanwhile; running sessions notice
the changed file. The other accounts wait in `~/.config/remote-agents/accounts/claude/<name>.json`
(mode 0600, directory 0700) on the box.

Nothing here refreshes the live login: Claude Code does. Its refresh tokens change with every
refresh, so each account command asks Anthropic whom the live login signs in as and copies it
back to that account. When it can't (the access token expired while no session ran, or was
refused), the login is unverified: its account is the one whose saved copy holds the same
token, or the one Claude Code's record and the last switch agree on; a switch then keeps it
there, unless that copy is newer. When nothing says whose it is, no account switches or refreshes
until a session has refreshed the login. A daily timer (`remote-agents-accounts-refresh.timer`)
refreshes the inactive logins a week before they would expire, and each day those whose expiry is
unknown; it fails, refreshing none, while nothing says whose the box's login is. Usage comes from
`api.anthropic.com/api/oauth/usage`, the endpoint Claude Code's `/usage` reads, and the live
login's account from `/api/oauth/profile`; both are undocumented, so a change in their shape
shows as an error. A weekly window with no reset time is taken as not started. To start an
inactive account's window, Claude Code runs with a copy of its login in a config dir of its own,
under the vault's lock; the copy is refreshed first so that Claude Code need not rotate it, and
any tokens it rotates there all the same go back to the account.

`rcodex account` does the same for Codex's ChatGPT logins, with one difference: a running Codex
keeps the login it started with, so a switch reaches only the Codex sessions started after it.
It names the running ones; restart each with `rcodex resume <id>` to move it over. A Codex
refreshes its login only while `auth.json` still holds that account, so a session left on the old
account never spends its saved copy; once that session's access token expires, it stops until it
is restarted. Codex keeps
its login in `~/.codex/auth.json`, which a switch replaces whole under Codex's `auth.lock`; the
saved ones wait in `~/.config/remote-agents/accounts/codex/`. The login names its own account,
so the vault always knows whose the live one is. A running Codex refreshes its login outside its
lock and keeps the new tokens only if `auth.json` still holds that login, so a switch that lands
in between costs that account its login. A switch is therefore refused while the live access
token is expired or within 15 minutes of expiry, when a Codex refreshes it; a Codex that
refreshes because a request was refused can still race a switch, which is rare. The timer refreshes an idle
login a day before its access token expires, as often as a Codex that used it would. Usage
comes from `chatgpt.com/backend-api/wham/usage`, the endpoint Codex's `/status` reads, which is
undocumented too. A Codex weekly window always has a reset time, so `rcodex account` has no s
and no `start`. Accounts need Codex's logins in `auth.json`, the default: a `config.toml`
with another `cli_auth_credentials_store` is refused.

Account commands use `~/.claude` and `~/.codex`, as the sessions do, and refuse
`CLAUDE_CONFIG_DIR`, `CLAUDE_SECURESTORAGE_CONFIG_DIR` and `CODEX_HOME`, which the sessions may
not see. A test install never reaches those logins: its `~/.config/<install>/accounts.toml`
must give each agent it uses a dir of its own (`[claude]` or `[codex]`, `config_dir = "<dir>"`),
and only the `remote-agents` install refreshes saved logins.

## Security model

The box account is trusted with the laptop: anyone who can act as the box user can read and set
the laptop clipboard, open http(s) URLs in the laptop browser, send cmux status events and write the
files below while the link is up. Use a box account nobody else uses.

Files: on its own, the box writes on the laptop only the files pasted into a session in the last
two weeks and their `name (box).ext` beside them, with symlinks and `..` resolved on the laptop,
and only where the file is a regular, non-executable one inside your home, not under `~/Library`,
with no component starting with `.` (such as `~/.zshrc`), whose folder still leads where it did at
the paste. The wrapper records each paste itself, so only a paste adds a file to that list; but
pasting a path the box put on the laptop clipboard counts too, which is why anything else waits
for you: the link daemon shows the macOS authentication sheet (Touch ID, or your password),
naming the session, whether it creates or replaces, and the full path where the file lands. Every
`laptop-save` asks the same way. Prompts show one at a time; one declined, failed or unanswered
for 10 minutes is refused with a notice, and the box keeps the agent's version: a new change, a
new paste or a new save offers it anew. A daemon that cannot ask, as with no one logged in at the
screen, refuses. An approval holds for that file (or save destination) and session until the link
daemon restarts: it lives only in the daemon's memory, as a grant kept in a file could be forged
by a write to it. The place is checked again where the write lands; one that changed since
the prompt is refused. Sessions name themselves to the laptop, and every box session runs as the
same user, so one box session can claim another's name and with it that session's pasted files
and approvals: the approvals scope prompts, not box sessions from each other. A file replaced
goes to the Trash. A write never follows a symlink at the file's own name: a write-back goes
beside it, a save replaces the link. The
laptop fetches the content over an ssh of its own (`remote-agents-box send-file`), up to 100 MB.
A refused write is not retried until the copy changes again.

URLs: link-local and multicast hosts are refused after resolving the host; LAN and tailnet
addresses open. The browser resolves again, so DNS rebinding remains possible.

Logins: a login on the box (Claude's `/login`, kubelogin, any OAuth CLI) opens a URL on
`localhost:<port>` or carries one in its query, such as `redirect_uri`. Before opening it, the
daemon listens on that laptop port (127.0.0.1 and ::1) and carries each connection to the box's
`localhost:<port>` over its own `ssh -W`. The forward ends once nothing on the box listens on the
port (checked every 15 seconds and after each connection), or 10 minutes after the last URL for the
port. It also binds the port's wildcard addresses, so a laptop program starting a login on that
port meanwhile fails to bind instead of handing its callback to the box. Refused: a port a laptop
program already holds (named in the error), ports below 1024, and other loopback addresses
(127.0.0.2, 0.0.0.0). Only the opened URL is judged: a page it loads can still redirect the
browser to a laptop service.

## Tests

    make test                      # unit tests, with the race detector
    make lint                      # go vet, staticcheck, shellcheck
    make e2e HOST=<ssh host>       # deploys to the box under a separate install name and
                                   # drives the laptop binaries against it in ~/remote-agents-test

## License

MIT, see [LICENSE](LICENSE).
