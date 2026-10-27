# remote-agents

Run Claude Code on a remote Linux box while it feels local on your Mac.

`rclaude` in a laptop terminal starts Claude Code on the box; the laptop only displays it.
`claude` stays your local Claude Code. Sessions live in [shpool](https://github.com/shell-pool/shpool)
on the box, so a closed lid, a network change or a dropped SSH link does not stop them: the laptop
reconnects on its own. While the link is up, a launchd daemon on the laptop gives box sessions the
laptop's clipboard and browser, carries OAuth logins started on the box back to it, and forwards
status events to cmux if you use it.

Why: long agent runs want a machine that stays on, has cores and disk to spare, and is not your
laptop; you still want to work with them from your own terminal, clipboard and browser.

## Requirements

- Laptop: macOS (the link daemon runs under launchd), Go to build, `ssh` to the box without a
  password prompt (keys or an agent). cmux is optional; `make lint` needs staticcheck and
  shellcheck.
- Box: Linux x86_64 with systemd, a user with lingering enabled (`loginctl enable-linger <user>`).
  `make deploy-box` installs shpool and Claude Code there.

`box/bootstrap/` builds such a box from scratch on a Hetzner dedicated server: one key-only SSH
user, Tailscale and a closed public NIC. See [its README](box/bootstrap/README.md).

## Install

Box, from the laptop; idempotent, and it never restarts the shpool daemon:

    make deploy-box HOST=<ssh host>

It installs shpool (pinned), `remote-agents-box`, the clipboard, browser and hook shims, a
`systemctl --user` socket for shpool, and Claude Code if missing. Log in to Claude once on the box
through a session (`rclaude`, then `/login`).

Laptop:

    make install                                   # ~/.local/bin/remote-agents and rclaude
    mkdir -p ~/.config/remote-agents
    cp config.example.toml ~/.config/remote-agents/config.toml   # then set host and workdir
    cp bin/com.remote-agents.link.plist ~/Library/LaunchAgents/
    launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.remote-agents.link.plist

No shell setup is needed. Arguments every run should get go in the agent's `args` in the config.
Stop the link daemon with `launchctl bootout gui/$(id -u)/com.remote-agents.link`.
Logs: `~/Library/Logs/remote-agents/link.log` (daemon), `sessions.log` (hook relay problems).

## Use

- `rclaude [args]` starts a session in the configured workdir; `rclaude --resume <id>` resumes
  one, reattaching if it is still running. After Claude exits, `rclaude` prints how to resume the
  session on the box (Claude's own `claude --resume` line would look for it locally). With
  `--continue` or the resume picker the id comes from the SessionStart hook cmux injects; outside
  cmux only `rclaude --resume <id>` gets the line, since a new session may never have started.
- `remote-agents ls` lists sessions on the box; `remote-agents attach <session>` reattaches.
- While the link is down, Ctrl+C detaches; the session keeps running.

## Security model

The box account is trusted with the laptop: anyone who can act as the box user can read and set
the laptop clipboard, open http(s) URLs in the laptop browser and send cmux status events while the
link is up. Use a box account nobody else uses.

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
