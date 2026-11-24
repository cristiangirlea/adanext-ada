# Learning profile facts

Run `ada learn propose` to review the CLI owner's finished conversations for
durable facts such as a preferred name, a language preference or a detail of
their setup. Proposing creates pending items. Only an explicit keep adds a
proposal to the profile used by chat. The review uses the configured chat
provider with no tools. Metered endpoints require `allow_metered`.

By default, a review considers sessions idle longer than `session_idle` and
updated within the last 24 hours. It reviews up to ten changed sessions,
newest first. Use `-since 48h` to widen the window, or `-session ID` to choose
one owned session regardless of age. Ada reads only committed messages and
skips a session whose message count is unchanged since its last successful
review. A new turn makes the session eligible again. If a provider reply is
malformed, Ada leaves that session eligible for retry and continues with
older sessions within the ten-session limit. The command prints any new
proposals, reports each failed reply by session ID and exits with an error.
Provider errors and cancellation stop the batch immediately.

Each review asks for up to five facts with zero-based message indices. It
instructs the provider to omit transient details and secrets. Ada drops empty
citations and invalid fact text. It also drops duplicates of the user's
profile facts and pending, kept, rejected or rolled-back proposals, ignoring
case and differences in whitespace. An archived fact can be proposed again.
The provider sees the newest messages that fit in 32 KiB, with each message's
text clipped to 2 KiB. Message numbers stay the same as in the session log.

## Outside evidence

Ada sets the outside flag in code from the messages it read and the indices
the model cites. The model supplies only fact text and citations. If any
message of the session read for the review contains anything but plain text (a tool
call, a tool result, a file), or comes from a sender who is unlinked or linked to a different
user, every proposal from that review is outside. This applies even when a
proposal cites only the user's own words, and even when that message was too
old to fit the window shown to the model, because a later reply may repeat it.

In a conversation containing only the user's text and assistant replies from the
same linked sender, a proposal is inside only if every citation points to a
shown, nonblank, text-only user message. Citing an assistant reply or a
missing message makes the proposal outside. Ada instructs the model to
ignore instructions in evidence, including tool results, web pages and
forwarded messages.

Use the session and cited messages to assess what the text supports. Keeping
an outside proposal from the CLI requires the additional `-outside` flag.

## Review in a paired chat

While `ada serve` runs, Ada waits `learning.propose_every` (default `1h`),
then reviews the configured owner and each user with a linked identity, one
user at a time. It uses the same idle-session rules, 24-hour window and
chat provider as `ada learn propose`. Metered endpoints require
`allow_metered`. After finishing, Ada waits a full interval before the next
review. Errors appear in serve's diagnostics. Set `learning.propose_every`
to `"0"` to disable automatic reviews. Shutdown cancels a running review.

A new proposal can appear in the session's most recent chat if the plugin
identified that chat as private when Ada recorded its origin. The channel
must have groups disabled, and the sender must still be linked to the
proposal's user. Group chats, chats of unknown type and older origins
without a privacy record leave the proposal available for CLI review.

If the channel is reconnecting, Ada records the offer in its outbox. Before
sending it, Ada checks the recorded chat, channel settings, sender link,
proposal status and expiry again.

The offer reads `Remember this about you? "<fact>"` with **Keep** and
**Forget it** buttons. An outside offer adds a second line:

> It came from a conversation that included content from outside (a tool, a file, a page or another sender).

**Keep anyway** explicitly confirms an outside fact. A keep adds the fact
with its learning source and replies `Kept.` If the profile refuses the
fact, the chat receives `Could not keep it: <reason>` and the proposal stays
pending. Choosing **Forget it** rejects
the proposal and replies `Forgotten.` Only the original linked sender in
that chat can use the buttons. Typed responses cannot decide a proposal.

Each offer has one delivery ID. Its buttons survive a restart, including
when automatic reviews have since been disabled. They expire after
`learning.offer_ttl` (default `168h`, or seven days). An expired or already
decided offer ignores further presses. A pending proposal remains available
to the CLI after an offer or its expiry. Ada never creates a second offer
for it.

## Commands

| Command | Effect |
| --- | --- |
| `ada learn propose [-session ID] [-since 24h]` | Print new pending proposals with their IDs, sessions, text and outside flags. |
| `ada learn list [-all]` | List pending proposals, or every status with `-all`. |
| `ada learn keep ID [-outside]` | Add the pending fact to the profile and record its confirmation time. |
| `ada learn reject ID` | Dismiss a pending proposal. |
| `ada learn rollback ID` | Remove the fact created by a kept, stale or archived proposal, if still present. |
| `ada learn confirm ID` | Refresh a kept or stale item's confirmation and mark it kept. |
| `ada learn restore ID` | Add an archived fact back with a new fact ID and confirmation time. |

Each command accepts `-config FILE`, for example
`ada learn list -config settings.json -all`. Decisions apply only to the
configured owner. A kept fact records its source as
`learn:<proposal id> session:<session id>`. Rollback uses the fact ID, so
unrelated facts and manually added replacements survive.

## Staleness and storage

Every `ada learn` command first checks the owner's proposals and prints any
status changes. After `learning.stale_after` since confirmation (default
`2160h`, or 90 days), a kept fact becomes stale and stays in the profile.
After another `learning.archive_after` (default `720h`, or 30 days), the
next check archives it and removes the profile fact. A check after both
deadlines catches up in one run. The archived proposal retains its text and
citations for restore. Both settings take Go durations.

Ada saves proposals and per-session review counts in
`learning/<encoded user>.json` under `data_dir`. Changes take
`learning.lock`. A private temporary decision record lets the next command
recover a profile change interrupted between file replacements. An invalid
learning file reports an error.

Project knowledge and skills are planned for later pieces of issue #7.
