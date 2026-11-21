# adanext-ada

**Status: planned. Nothing here is usable yet.** The work is the epic, issue #1, and its slices.

Ada is a personal agent you reach from the desktop and from chat apps. She keeps conversations
and a small memory, asks before anything with an effect, runs scheduled jobs, and gets better from
use only when an evaluation or your yes says a change helps. Coding work is not done here: Ada
hands it to [hrn](https://github.com/cristiangirlea/harness), which runs it on the agent backends
you already use, each task on its own branch with checks and a full record.

- **Plan and local by default.** Conversation runs on a local model; metered APIs only when you
  turn them on.
- **Channels are plugins.** Each chat app is a separate program from
  [adanext-channels](https://github.com/cristiangirlea/adanext-channels), holding only its own
  token. Who may talk to Ada is decided here, by pairing.
- **The operating system is the boundary.** Work a chat or a schedule starts runs isolated, and
  every command goes through [adanext-exec](https://github.com/cristiangirlea/adanext-exec).

Part of the AdaNext tools, with [AdaNext OS](https://github.com/cristiangirlea/adanext-os) as
the desktop. Contributors and agents: read [AGENTS.md](AGENTS.md) first.

MIT licensed.
