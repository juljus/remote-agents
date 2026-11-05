# remote-agents

Run Claude Code and Codex on a remote Linux box, and use them from your Mac terminal as if they
ran on your laptop.

Type `rclaude` instead of `claude`, or `rcodex` instead of `codex`. The agent runs on the box;
your terminal only shows it. Your local `claude` and `codex` stay as they are.

## Why

Long agent runs want a machine that stays on and has cores, memory and disk to spare. You still
want to work with them from your own terminal, clipboard and browser. remote-agents gives you
both.

## What you get

- **Sessions that outlive your laptop.** Close the lid, switch networks, lose the connection: the
  agent keeps working on the box, and `rclaude` reconnects to it on its own when you are back.
- **Your clipboard and browser.** The agent copies to and pastes from your Mac clipboard, images
  included, and links it opens open in your Mac browser. Logins that call back to `localhost`,
  such as Claude's `/login` or `rcodex login`, finish through the same link.
- **Your files.** Paste or drag a Mac file into the session and the agent gets its own copy. When
  the agent edits it, the change goes back to the file on your Mac. The agent can also hand you a
  new file with `laptop-save`. Anything new or risky waits for your Touch ID first.
- **Several accounts.** Keep more than one Claude or ChatGPT login on the box and switch between
  them in a picker that shows how much of each one's weekly limit is left.
- **No session takes the box down.** Each session has its own memory cap, so a runaway process is
  killed alone.
- A small cloud in the terminal's bottom-right corner tells you a session is remote. cmux works
  too: its tabs show the remote sessions' status.

## Setup

You need:

- a Mac with Go and an `ssh` login to the box that asks for no password (keys or an agent);
- a Linux x86_64 box with systemd and docker, and a user on it with lingering enabled
  (`loginctl enable-linger <user>`). For `rcodex`, Codex on the box's `/usr/local/bin`.

Starting from a bare Hetzner server? [box/bootstrap](box/bootstrap/README.md) builds such a box
for you, locked down to your tailnet.

1. Install the box side, from the Mac:

       make deploy-box HOST=<ssh host>

2. Install the Mac side and point it at the box:

       make install
       mkdir -p ~/.config/remote-agents
       cp config.example.toml ~/.config/remote-agents/config.toml   # set host and workdir
       cp bin/com.remote-agents.link.plist ~/Library/LaunchAgents/
       launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.remote-agents.link.plist

3. Log in on the box: run `rclaude` and type `/login`, and run `rcodex login`. The login pages
   open in your Mac browser.

The example config runs both agents without permission prompts (the box is the sandbox). Remove
the `args` lines to get the prompts back.

## Everyday use

    rclaude                      # new Claude session on the box
    rclaude --resume <id>        # resume one; reattaches if it still runs
    rcodex                       # new Codex session
    rcodex resume <id>
    remote-agents ls             # sessions on the box
    remote-agents attach <name>  # reattach to one
    rclaude account              # pick or add a Claude account (rcodex account for Codex)

- When the agent exits, `rclaude` prints the command that resumes the session.
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
