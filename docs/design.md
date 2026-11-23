# Ada's conversation loop

Run `ada chat [-config FILE]` to read a message per line and print each answer.
The default settings path is `adanext-ada/settings.json` under the user
configuration directory. `ada version` prints the build version.

A turn adds the user's message to the conversation and sends the conversation
and configured tool specs to the chat provider. If the model asks for tools,
Ada records the assistant's reply and sends the results together in one user
message. A failed tool call becomes an error result the model can respond to.
The turn returns the assistant's text once there are no further tool calls.
Chat history is stored locally. See [Ada's memory](memory.md) for sessions,
profile facts, local search and project knowledge. Ctrl-C during
a turn cancels it. Failed turns report to stderr and leave the previous
conversation intact. EOF or Ctrl-C between turns ends the chat.

## Providers and billing

`chat_provider` selects a named entry in `providers`. Each entry has a `kind`
(`anthropic` or `openai`), `base_url` and `model`. The adapters use non-streaming
Anthropic Messages or OpenAI-compatible Chat Completions and translate their
text, tool calls, tool results and usage into shared Go types.

A local base URL uses HTTP or HTTPS, has no user information, query or fragment,
and has host `localhost` or an IP literal in `127.0.0.0/8`, `::1`, `10.0.0.0/8`,
`172.16.0.0/12`, `192.168.0.0/16` or `fc00::/7`. Ada treats every other base URL
as metered, including hostnames that happen to resolve to local IPs.
Set `allow_metered` to true to use a metered endpoint. Its API key is required
and comes from the environment variable named by `api_key_env`. A local
Anthropic-compatible endpoint gets the literal `local` when no key is set.

Provider HTTP calls follow the turn's deadline, cap response bodies at 8 MiB
and retry rate limits, server errors and connection failures up to twice.
Provider redirects are refused so the request stays on the configured endpoint.

## Tools and delegation

- `read_file` has the `read` effect. It reads regular files up to 256 KiB under
  `read_roots`. Ada resolves symlinks before checking containment and uses a
  rooted open for the read. Missing paths and paths outside the roots receive
  the same error.
- `fetch_url` has the `read` effect. It fetches public HTTP or HTTPS addresses,
  checking each actual connection, including across redirects. It accepts
  text and JSON and strips markup, scripts and styles from HTML. The limits
  are 20 seconds, 1 MiB and five redirects.
- `delegate_to_hrn` has the `delegate` effect in readonly mode. Use it for coding work and tasks
  that need a stronger coding model. The working directory must be within
  `workspace_roots`. Ada starts `hrn exec --protocol v1`, checks the handshake
  and numbered messages, and returns the run ID, status, backend and result
  text. A status other than `ok` becomes an error result with the status, run
  ID and final text. On turn cancellation, Ada gives hrn up to 10 seconds
  to finish after sending a cancel message, then kills its process tree.
  The task timeout defaults to 30 minutes. Readonly is the default `hrn.mode`. The `auto` mode asks for [write approval](approvals.md).
  Settings with `workspace_roots` must name `hrn.backend`, or they fail to
  load, since hrn's own routing could pick a metered backend. The `api`
  backend requires `allow_metered`.
  The optional purpose is one of `review`, `debug`, `implement`, `refactor`,
  `test`, `research`, `plan` or `ops`. Ada removes every configured
  `api_key_env` variable from hrn's environment. On Windows, delegation
  refuses to launch if a provider names `SYSTEMROOT` as its key variable.
  Protocol errors include the available stderr tail.
- Configured MCP stdio servers must declare a `read` or `write` effect. Their tools are
  exposed as `<server>__<tool>` after initialization and discovery, with
  names limited to 64 ASCII letters, digits, underscores and hyphens.
  Ada replaces other characters with underscores and refuses duplicate
  exposed names. Missing or null input schemas default to a JSON object.
  Each server has 30 seconds for startup and discovery. Servers receive
  common system environment variables and their configured `env`. If a
  call is canceled, Ada stops the server and initializes a fresh process
  for the next call. Ada ignores server notifications and answers server
  requests with JSON-RPC method not found. Ada trusts each configured command
  to uphold its declared effect and stops servers when chat exits.

## Budgets

Each turn defaults to 25 model calls, 200,000 input and output tokens
in total, and 60 minutes. The per-request output limit defaults to 4,096 tokens.
Ada checks usage after each model response. If both reported counts are zero,
Ada divides the combined UTF-8 byte count by four. The count includes the
request system prompt, message text, tool inputs and tool results, plus the
response text and tool inputs. A reached budget returns a short message naming
the budget and skips any pending tool calls. Caller cancellation returns a
context error.
Timeout settings use Go duration strings such as `30m` and `1h`.
During serve, `learning.propose_every` sets the delay between proposal
reviews (default `1h`, `"0"` disables them). `learning.offer_ttl` sets how
long chat buttons accept a decision (default `168h`).

## Later slices

[Channels](channels.md) describes the plugin host and pairing.
[Scheduled jobs](schedule.md) run prompts or scripts during serve and deliver results to their configured chat.
[Learning](learning.md) proposes profile facts for the owner's approval and supports rollback and staleness.
Isolation (#8) adds stronger execution
boundaries.
