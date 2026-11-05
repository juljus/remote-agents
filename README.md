# remote-agents

Runs Claude Code and Codex on a remote Linux box and shows them in your Mac terminal.

`rclaude` starts Claude Code on the box, `rcodex` starts Codex; the Mac only displays the
session. Your local `claude` and `codex` are not changed.

## What it does

- Sessions run on the box in [shpool](https://github.com/shell-pool/shpool). When the laptop sleeps, changes network or loses the
  connection, the agent keeps running, and `rclaude` reconnects to it when the link is back.
- Box sessions use the Mac's clipboard (text and images) and open links in the Mac's browser.
  Logins that call back to `localhost`, such as Claude's `/login` or `rcodex login`, complete
  through the link.
- A Mac file pasted or dragged into a session is uploaded, and the agent works on its own copy.
  Edits to that copy are written back to the Mac file. `laptop-save` on the box saves a new file
  to the Mac. New or risky writes on the Mac ask for Touch ID first.
- The box can hold several Claude and ChatGPT logins; `rclaude account` and `rcodex account`
  switch between them and show each one's remaining weekly usage.
- Each session has its own memory limit, so the kernel kills only the process that exceeded it.
- A cloud in the terminal's bottom-right corner marks a remote session. With cmux, its tabs show
  the remote sessions' status.

## Setup

You need:

- a Mac with Go and an `ssh` login to the box that asks for no password (keys or an agent);
- a Linux x86_64 box with systemd, and a user on it who can run docker and has lingering enabled
  (`loginctl enable-linger <user>`), with the folder your sessions should work in. For `rcodex`,
  Codex on the box's `/usr/local/bin`.

[box/bootstrap](box/bootstrap/README.md) sets up such a box on a Hetzner dedicated server, with SSH
reachable only over Tailscale.

Clone this repo on the Mac and run these from its root.

1. Install the box side:

       make deploy-box HOST=<ssh host>

2. Install the Mac side and point it at the box:

       make install
       mkdir -p ~/.config/remote-agents
       cp config.example.toml ~/.config/remote-agents/config.toml   # set host and workdir
       cp bin/com.remote-agents.link.plist ~/Library/LaunchAgents/
       launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.remote-agents.link.plist

   `rclaude` and `rcodex` land in `~/.local/bin`; put it on your `PATH` if it is not
   (`make install BINDIR=<dir>` picks another).

3. Log in on the box: run `rclaude` and type `/login`, and run `rcodex login`. The login pages
   open in your Mac browser.

The example config runs both agents without permission prompts (the box is the sandbox). Remove
the `args` lines to get the prompts back.

## Everyday use

    rclaude                        # new Claude session on the box
    rclaude --resume <id>          # resume one; reattaches if it still runs
    rcodex                         # new Codex session
    rcodex resume <id>
    remote-agents ls               # sessions on the box
    remote-agents attach <session> # reattach to one
    rclaude account                # pick or add a Claude account (rcodex account for Codex)

- When the agent exits, `rclaude` prints the command that resumes the session, once it knows the
  session's id: after `--resume`, or inside cmux.
- A detached session that has been idle for 5 minutes is ended; it resumes when you reconnect.
- While the connection is down, Ctrl+C detaches; the session keeps running on the box.
- On the box, `laptop-save <file>` puts a file in your Mac's `~/Downloads`.

## More

- [Install and operations](docs/operations.md): every install, deploy and test detail,
  configuration, logs and known pitfalls. Written to be precise; agents work from it.
- [Accounts](docs/accounts.md): the account picker and how logins are kept and switched.
- [Security model](docs/security.md): what the box may do on your Mac, and what asks you first.
- [Box bootstrap](box/bootstrap/README.md): building a box from a bare Hetzner server.

## License

MIT, see [LICENSE](LICENSE).
