# Approvals

Ada asks before each tool call that changes files, runs commands with write
access, or sends a message or post. An approval authorizes one call. Read tools and
readonly hrn delegations run directly. Ada always refuses tools that touch
money or accounts, and refuses any unclassified effect.

Set `hrn.mode` to `auto` to allow a write-capable delegation after approval.
The prompt shows the backend, mode, resolved working directory, timeout,
purpose and full task prompt. The default mode is `readonly`. Ada refuses
`yolo` because hrn's own commands would run without individual approvals.
An MCP server can declare `read` or `write`. Each write prompt identifies
the server and tool and shows the JSON arguments. Ada rejects other MCP
effect declarations when loading settings.

## Answering in the CLI

During `ada chat`, Ada prints the request to stderr and waits for a line on
the same stdin used for chat.

    Ada asks to write: MCP server="notes" tool="add" arguments={"text":"Call Jo"}
    Type K7RD to approve, anything else to deny (expires in 10m):

Ada generates a fresh four-character code for each prompt. Type the displayed
code and press Enter to approve. Letter case and surrounding whitespace are
ignored. Every other answer and EOF deny the request. The code appears only
on stderr and is kept out of the conversation and approval log.

If chat input is already buffered or an earlier line read is pending, Ada
denies the request as `typed ahead`. The waiting input stays available to chat.
`approvals.timeout` sets the wait using a Go duration such as `"2m"` and
defaults to ten minutes. On expiry Ada prints `Approval expired; nothing was run.`
You can continue chatting. A late confirmation code is discarded.
Ada shows only one prompt at a time. Without an approver, the call is refused.

Only the surface asking the user supplies a decision. Chat history, model
messages, pages, email and tool output cannot grant approval. [Channel plugins](channels.md) present buttons bound to the original sender, chat and delivery.
The desktop will ask on its own surface.

## History

Ada appends the request and decision to `<data_dir>/approvals/<session>.jsonl`
before running an approved tool. Sessionless requests use `none.jsonl`. The
log includes the full summary, timestamps, decision reason and deciding
surface when available. On Unix, directories use mode 0700 and files use
0600. Windows inherits directory access controls.

Run `ada approvals` to list the owner's latest 20 decisions, newest first.
Use `ada approvals -n 5` to show five. As with chat, `-config FILE` selects
a settings file.
