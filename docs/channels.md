# Chat channels

A channel is a separate executable. Install the binary built for your OS and
its matching `channel.json` together in a directory you control. Use one
directory per channel. Get both files from the same version of
[adanext-channels](https://github.com/cristiangirlea/adanext-channels).
On Unix, give the binary execute permission. Set the environment variable
named by the manifest before starting Ada.

Add channels to your existing provider settings. For example, a Telegram
installation could use this entry (use the actual executable path on Windows).

```json
{
  "channels": [{
    "name": "telegram",
    "path": "/opt/ada/telegram/ada-channel-telegram",
    "enabled": true,
    "allow": [],
    "groups": false
  }]
}
```

Run `ada channels [-config FILE]` to check paths, manifests and which binaries
are present. Each manifest accepts `name`, `label`, `secret_env` and `hosts`.
The configured name, manifest name and v2 channel handshake must agree. A
plugin receives the common system variables used for MCP and its own declared
secret. Configured provider key variables are excluded. The hosts list
documents intended network destinations. Network enforcement belongs to the
isolation slice.

Run `ada serve [-config FILE]` to start enabled plugins. Status goes to stderr.
Only one serve or chat process can write sessions in a data directory. Ctrl-C
or SIGTERM on Unix cancels active turns and closes plugin stdin. Ada kills a
plugin tree that is still running after four seconds. A missing or mismatched
manifest or an incompatible handshake stops that channel. Serve continues until
all enabled channels have failed or it receives a shutdown signal.

## Pairing and conversations

An unknown sender gets a six-character code once per ten minutes. On your
computer, run `ada pair [-config FILE] CODE` to link that sender ID on that
channel to the owner. Codes expire after ten minutes. Run
`ada unpair [-config FILE] CHANNEL SENDER` to remove a link. Entries in
`allow` are linked to the owner whenever serve starts. Ada records links it
creates this way in `channels/<name>/allow.json`. Removing an entry revokes
its link on the next start. Links made with `ada pair` survive allow-list edits.

By default, only chats explicitly identified as private are processed.
With `groups: true`, pairing still applies to each sender.
Replies and approval prompts are visible in
the originating chat. Linked identities share the user's active session and
take turns in order. Send `/new` to start a session or `/stop` from any paired
channel to cancel the current turn.

When a tool needs approval, Ada sends its summary with **Approve** and **Deny**
buttons. Only the original sender in the same channel and chat can answer that
prompt. Typed text cannot grant approval. The deadline is `approvals.timeout`.
`ada approvals` shows the decision and the channel that made it.

When groups are disabled, Ada can offer [learning proposals](learning.md)
in the session's most recent chat if its origin was recorded as private and
the sender is still linked to the same user. Older origins without a chat
type and group chats leave proposals for the CLI. While a plugin reconnects,
Ada queues eligible offers in its outbox and rechecks them before delivery.
The **Keep** (or **Keep anyway**) and **Forget it** buttons survive a serve
restart until the offer expires or the proposal is decided. If the profile
refuses a keep, Ada replies in that chat with a short reason and leaves the
proposal pending.

## Delivery and recovery

Under `data_dir/channels/<name>/`, Ada syncs each inbound to `inbox.jsonl`
before acknowledging it. Replays of that ID receive another acknowledgement
and skip processing. A plugin restart leaves queued and running turns in Ada.
Ada marks each inbound processed when its turn finishes. After Ada restarts,
Ada queues unfinished messages from currently paired senders across all channels
before starting the plugins. It uses the stored receive time, then channel name,
then journal order. If shutdown interrupted a turn before Ada saved its answer
in the session, the turn restarts from the beginning and tools may ask for
approval again. Once Ada saves the answer, it sends that answer on recovery
without running the turn twice. A replayed `/new` reuses the session it created.

Ada records outgoing deliveries and outcomes in `outbox.jsonl`. After a
disconnect, outstanding deliveries keep their IDs. Restarting Ada discards
approval prompts from its previous run. Startup compacts both journals,
retaining unfinished messages, pending deliveries with their retry state,
and the most recent 10,000 inbound IDs for duplicate detection. A service can
show a duplicate if its plugin cannot recognize a previous send with the same ID.
Rate-limited sends wait for the requested delay, with up to three tries.
Other delivery failures are terminal. Ada retries a plugin exit, a handshake
timeout, or a launch failure after an earlier successful launch. Backoff starts
at one second and doubles after short-lived attempts, up to one minute.
