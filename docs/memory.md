# Ada's memory

The default data directory is `adanext-ada/data` under the OS user
configuration directory. Override it with `data_dir` in settings. Ada creates
directories with mode 0700 and files with mode 0600 on Unix. Windows uses the
directory's inherited access controls. Rewritten files are synced to a
temporary file in the same directory and renamed into place.

## Sessions

`identities.json` maps each channel and sender to a user. The CLI links itself
to `owner` (default user ID `owner`). Other channels obtain identity links
through [pairing or per-channel settings](channels.md). Two linked senders for one user can
continue the same conversation across channels.

One `ada chat` or `ada serve` per data directory holds the exclusive `sessions.lock` until it
exits. A second writer reports that the directory is in use. `ada sessions` and
`ada search` can read the same directory while a chat is running.

`ada chat` prints the session ID to stderr and continues the user's most
recently updated session within `session_idle` (default `12h`). Otherwise it
starts a session. Use `ada chat -new` to start fresh or `ada chat -resume ID`
to reopen a particular session belonging to the CLI owner.

Under `sessions/<id>/`, `meta.json` contains ownership, timestamps and each
channel/chat/sender origin. `messages.jsonl` records each message with its
time and origin. Ada appends the user's text, assistant messages and tool
results after a successful turn. Readers ignore unfinished turns and leave the
log intact. The session writer recovers an interrupted append before continuing
the session.
Each turn includes the last `history_messages` (default 40), trimmed to the
first user text message in that window. If the window contains only a turn's
continuation, Ada extends it back to that turn's user text message.
`ada sessions [-n N]` lists the owner's sessions, newest first, with their
origins and first user line.

## Profile

`ada profile add "TEXT"` saves a single-line fact for the CLI owner in
`profiles/<user>.json` and prints its ID. Ada encodes IDs that could be unsafe
or ambiguous as filenames. Profile writers briefly hold `profile.lock`, so
facts can be edited during a chat. Use `ada profile list` and `ada profile remove ID` to
manage facts. Keeping a [learning proposal](learning.md) in a paired chat
also adds its fact. Each model request includes the current
user's profile in its system prompt. Requests without a user include no
profile. The limits are 40 facts, 300 UTF-8 bytes per fact and 4,000 bytes of
text altogether.

## Search

`ada search QUERY` and the read-only `search_memory` tool search text and tool
results in the current user's sessions (the owner for CLI search). Tool calls
require a user identity. All users can also search this machine's hrn run
prompts, final answers and statuses.
Run records come from the `runs` directory under `hrn.home`, then `HRN_HOME`,
then `~/.hrn` in precedence order. When set, `hrn.home` overrides `HRN_HOME`
for both delegated runs and knowledge searches. Ada scans the 500 newest run
directories by name and skips malformed, unreadable or larger-than-1-MiB run files.
Matches use whole Unicode words of two or more characters. Rarer words weigh
more. Ties favor newer records. Results default to eight hits, capped at 20,
with excerpts up to 300 bytes. Session references use zero-based line indices.

## Project knowledge

To enable the read-only `search_knowledge` tool, configure all five fields:

```json
{"knowledge":{"tenant_id":"team","project_id":"project","backend":"local","model":"model","revision":"main"}}
```

Ada runs `hrn knowledge search` with a private temporary request file and
removes it afterwards. hrn reads `HRN_TENANT_TOKEN` and `HRN_SERVER_URL` from
its environment. Ada filters configured provider key variables before launch.
Memory stays on the machine except for content sent in configured model
requests and hrn's own calls. Each CLI subcommand accepts `-config FILE`
before its positional arguments.

The same scope is used to [propose and review project knowledge](learning.md#project-knowledge-from-delegated-runs) from successful delegated runs.

