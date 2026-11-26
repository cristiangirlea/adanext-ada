# adanext-ada

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

## Status

Early: the first release, v0.1.0, is source only, installed with `go install`. The plan is the
epic, [issue #1](https://github.com/cristiangirlea/adanext-ada/issues/1).

| Works today | Still to come |
|---|---|
| Chat on a local or hosted model, with tools ([design](docs/design.md)) | Isolation through adanext-exec ([#8](https://github.com/cristiangirlea/adanext-ada/issues/8)) |
| Sessions, profile facts, search and project knowledge ([memory](docs/memory.md)) | Ada in AdaNext OS and hrn watch ([#9](https://github.com/cristiangirlea/adanext-ada/issues/9)) |
| Chat-app plugins, pairing and allowlists ([channels](docs/channels.md)) | |
| Approvals before effects ([approvals](docs/approvals.md)) | |
| Scheduled jobs ([schedule](docs/schedule.md)) | |
| Learning proposals behind gates ([learning](docs/learning.md)) | |

## Quick start

You need Go as pinned in `go.mod` and a model endpoint. A local OpenAI-compatible server (for
example llama.cpp's `llama-server`) needs no key and no billing.

1. Install a release, or build from a clone:

   ```bash
   go install github.com/cristiangirlea/adanext-ada/cmd/ada@latest   # into $(go env GOPATH)/bin
   ada version

   go build ./cmd/ada            # in a clone: writes ada, or ada.exe on Windows
   ./ada version
   ```

2. Write `adanext-ada/settings.json` under your user configuration directory
   (`%APPDATA%` on Windows, `~/Library/Application Support` on macOS, `~/.config` on Linux),
   or pass another file with `-config FILE`:

   ```json
   {
     "providers": [
       {"name": "local", "kind": "openai", "base_url": "http://127.0.0.1:8081", "model": "my-model"}
     ],
     "chat_provider": "local"
   }
   ```

   `base_url` is the server's origin: Ada adds `/v1/chat/completions` (or `/v1/messages` for
   `kind` `anthropic`) itself. Any endpoint other than `localhost` or a loopback or private IP
   address (a hostname counts as metered even on your own network) also needs
   `"allow_metered": true` and `api_key_env`, the name of the environment variable that holds
   its key.

3. Chat, one message per line. Ctrl-C cancels a running turn; Ctrl-C between turns or end of
   input quits:

   ```bash
   ./ada chat            # -new starts a new session, -resume ID continues one
   ```

`ada serve` runs the channel plugins, scheduled jobs and the learning-proposal sweep. Only one
`ada chat` or `ada serve` can use a data directory at a time. Run `ada` alone for the full
command list.

## A real run

Recorded on 2026-11-26 with a local Gemma 4 26B model served by `llama-server`, a fresh data
directory, `read_roots` set to a `notes` folder holding one `todo.txt`, and `ada` on the PATH.
Messages were piped in, so they are not echoed; the answers are the model's, unedited, and the
commands are shown in the order they ran.

```console
$ printf '%s\n' "Hi Ada, what can you do? Two sentences." \
    "What is on my shopping list in todo.txt in my notes folder?" | ada chat -config settings.json
Session 20261126T203251-JAYQNJNHDEXU7YR2KSXIH47QDT
I can assist you with a wide range of tasks, such as answering questions, writing creative content, and solving complex problems. I can also help with coding, translating languages, and summarizing information to make your work more efficient.
Your shopping list in `todo.txt` is: milk, eggs, and coffee beans.
```

The second answer came from the `read_file` tool. Everything stays in the local data directory,
where the CLI can list and search it:

```console
$ ada sessions -config settings.json
20261126T203251-JAYQNJNHDEXU7YR2KSXIH47QDT	2026-11-26T20:33:14.5433379Z	cli/local/owner	Hi Ada, what can you do? Two sentences.
$ ada profile -config settings.json add "Prefers short answers"
Q5353LEKVA3HOLER2L3MFLE76V
$ ada profile -config settings.json list
Q5353LEKVA3HOLER2L3MFLE76V	Prefers short answers
$ ada search -config settings.json coffee
session:20261126T203251-JAYQNJNHDEXU7YR2KSXIH47QDT#4  session  2026-11-26T20:33:14.5433379Z
Shopping: milk, eggs, coffee beans.

session:20261126T203251-JAYQNJNHDEXU7YR2KSXIH47QDT#5  session  2026-11-26T20:33:14.5433379Z
Your shopping list in `todo.txt` is: milk, eggs, and coffee beans.
```

A later `ada chat` within `session_idle` (12 hours by default) continues the same session:

```console
$ echo "Which file did you read for me earlier?" | ada chat -config settings.json
Session 20261126T203251-JAYQNJNHDEXU7YR2KSXIH47QDT
I read `notes/todo.txt`.
```

## Documentation

- [Conversation loop, providers and tools](docs/design.md)
- [Memory: sessions, profile, search, knowledge](docs/memory.md)
- [Channels and pairing](docs/channels.md)
- [Approvals](docs/approvals.md)
- [Scheduled jobs](docs/schedule.md)
- [Learning](docs/learning.md)

Part of the AdaNext tools, with [AdaNext OS](https://github.com/cristiangirlea/adanext-os) as
the desktop. Contributors and agents: read [AGENTS.md](AGENTS.md) first.

MIT licensed.
