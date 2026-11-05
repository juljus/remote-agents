# Accounts

The box can hold several Claude logins and switch between them; a switch applies to every
session at once, running ones included, from their next request to Claude. `rcodex account` does
the same for Codex's ChatGPT logins, with the differences [below](#codex).
Back to the [README](../README.md).

## Commands

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

## Claude: where logins live and how a switch works

Claude Code keeps the box's login in `~/.claude/.credentials.json` (`claudeAiOauth`) and
`~/.claude.json` (`oauthAccount`). A switch replaces only those two keys, under Claude Code's own
locks, and only if no session refreshed or replaced the login meanwhile; running sessions notice
the changed file. The other accounts wait in `~/.config/remote-agents/accounts/claude/<name>.json`
(mode 0600, directory 0700) on the box.

## Claude: refreshes and whose login is live

Nothing here refreshes the live login: Claude Code does. Its refresh tokens change with every
refresh, so each account command asks Anthropic whom the live login signs in as and copies it
back to that account. When it can't (the access token expired while no session ran, or was
refused), the login is unverified: its account is the one whose saved copy holds the same
token, or the one Claude Code's record and the last switch agree on; a switch then keeps it
there, unless that copy is newer. When nothing says whose it is, no account switches or refreshes
until a session has refreshed the login.

A daily timer (`remote-agents-accounts-refresh.timer`) refreshes the inactive logins a week before
they would expire, and each day those whose expiry is unknown; it fails, refreshing none, while
nothing says whose the box's login is.

Usage comes from `api.anthropic.com/api/oauth/usage`, the endpoint Claude Code's `/usage` reads,
and the live login's account from `/api/oauth/profile`; both are undocumented, so a change in
their shape shows as an error. A weekly window with no reset time is taken as not started. To
start an inactive account's window, Claude Code runs with a copy of its login in a config dir of
its own, under the vault's lock; the copy is refreshed first so that Claude Code need not rotate
it, and any tokens it rotates there all the same go back to the account.

## Codex

`rcodex account` does the same for Codex's ChatGPT logins. The main difference: a running Codex
keeps the login it started with, so a switch reaches only the Codex sessions started after it.
It names the running ones; restart each with `rcodex resume <id>` to move it over. A Codex
refreshes its login only while `auth.json` still holds that account, so a session left on the old
account never spends its saved copy; once that session's access token expires, it stops until it
is restarted.

Codex keeps its login in `~/.codex/auth.json`, which a switch replaces whole under Codex's
`auth.lock`; the saved ones wait in `~/.config/remote-agents/accounts/codex/`. The login names its
own account, so the vault always knows whose the live one is.

A running Codex refreshes its login outside its lock and keeps the new tokens only if `auth.json`
still holds that login, so a switch that lands in between costs that account its login. A switch
is therefore refused while the live access token is expired or within 15 minutes of expiry, when
a Codex refreshes it; a Codex that refreshes because a request was refused can still race a
switch, which is rare. The timer refreshes an idle login a day before its access token expires,
as often as a Codex that used it would.

Usage comes from `chatgpt.com/backend-api/wham/usage`, the endpoint Codex's `/status` reads,
which is undocumented too. A Codex weekly window always has a reset time, so `rcodex account` has
no s and no `start`. Accounts need Codex's logins in `auth.json`, the default: a `config.toml`
with another `cli_auth_credentials_store` is refused.

The vault's safety rests on the pinned Codex refreshing a login only while `auth.json` still holds
that account. Re-check that in Codex's source before bumping Codex; the pin in
`box/bootstrap/host/tools.sh` says where.

## Config dirs and test installs

Account commands use `~/.claude` and `~/.codex`, as the sessions do, and refuse
`CLAUDE_CONFIG_DIR`, `CLAUDE_SECURESTORAGE_CONFIG_DIR` and `CODEX_HOME`, which the sessions may
not see. A test install never reaches those logins: its `~/.config/<install>/accounts.toml`
must give each agent it uses a dir of its own (`[claude]` or `[codex]`, `config_dir = "<dir>"`),
and only the `remote-agents` install refreshes saved logins.
