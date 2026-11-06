# Security model

What a box session may do on the laptop while the link is up, and what waits for you first.
Back to the [README](../README.md).

## Trust

The box account is trusted with the laptop: anyone who can act as the box user can read and set
the laptop clipboard, open http(s) URLs in the laptop browser, send cmux status events and write the
files below while the link is up. Use a box account nobody else uses.

## Files

### What the box writes on its own

On its own, the box writes on the laptop only the files pasted into a session in the last
two weeks and their `name (box).ext` beside them, with symlinks and `..` resolved on the laptop,
and only where the file is a regular, non-executable one inside your home, not under `~/Library`,
with no component starting with `.` (such as `~/.zshrc`), whose folder still leads where it did at
the paste. The wrapper records each paste itself, so only a paste adds a file to that list; but
pasting a path the box put on the laptop clipboard counts too, which is why anything else waits
for you.

### What asks for Touch ID

For anything else, the link daemon shows the macOS authentication sheet (Touch ID, or your
password), naming the session, whether it creates or replaces, and the full path where the file
lands. Every `laptop-save` asks the same way.

Prompts show one at a time; one declined, failed or unanswered for 10 minutes is refused with a
notice, and the box keeps the agent's version: a new change, a new paste or a new save offers it
anew. At most 8 prompts wait at once; a request past that, or one that waited its turn 10
minutes, fails without a refusal, and the box tries again later. A daemon that cannot ask, as
with no one logged in at the screen, refuses.

### The pause after a decline

Once you decline (cancel) a prompt, no prompt shows for 2 minutes: every request that would ask,
from any session and for any path, is refused with a notice, and the box keeps its version. The
box picks paths and session names, so a narrower pause would let it keep fresh prompts coming
until one touch slips. The pause stops only prompts: writes approved already, and ordinary
write-backs that need none, go on. A prompt nobody answered, or one that could not be shown,
starts no pause.

### How long an approval holds

An approval holds for that file's write-backs (or that save destination) and session, on the
terms its prompt named, until the link daemon restarts: what makes the place risky changing, or a
save that would now replace the file it was approved to create, asks again. A save's approval
also covers the later changes of the box file it saved, to that laptop file and from that
session only, where they land under the conflict rule; another box file or destination is a
save of its own. It lives only in the
daemon's memory, as a grant kept in a file could be forged by a write to it. The place is checked
again where the write lands; one that changed since the prompt is refused.

Sessions name themselves to the laptop, and every box session runs as the same user, so one box
session can claim another's name and with it that session's pasted files and approvals: the
approvals scope prompts, not box sessions from each other.

### How a write lands

A file replaced goes to the Trash. A write never follows a symlink at the file's own name: a
write-back goes beside it, a save replaces the link. The laptop fetches the content over an ssh of
its own (`remote-agents-box send-file`), up to 100 MB. A refused write is not retried until the
copy changes again.

## URLs

Link-local and multicast hosts are refused after resolving the host; LAN and tailnet addresses
open. The browser resolves again, so DNS rebinding remains possible.

## Logins

A login on the box (Claude's `/login`, kubelogin, any OAuth CLI) opens a URL on
`localhost:<port>` or carries one in its query, such as `redirect_uri`. Before opening it, the
daemon listens on that laptop port (127.0.0.1 and ::1) and carries each connection to the box's
`localhost:<port>` over its own `ssh -W`. The forward ends once nothing on the box listens on the
port (checked every 15 seconds and after each connection), or 10 minutes after the last URL for the
port. It also binds the port's wildcard addresses, so a laptop program starting a login on that
port meanwhile fails to bind instead of handing its callback to the box. Refused: a port a laptop
program already holds (named in the error), ports below 1024, and other loopback addresses
(127.0.0.2, 0.0.0.0). Only the opened URL is judged: a page it loads can still redirect the
browser to a laptop service.
