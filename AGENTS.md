# adanext-ada, Ada's core

One Go module: Ada's conversation loop, model provider layer, sessions and memory, the host for
channel plugins (pairing, allowlists, delivery), approvals, scheduled jobs and learning
proposals. Coding work is delegated to hrn (cristiangirlea/harness); chat apps are plugins from
cristiangirlea/adanext-channels; commands go through cristiangirlea/adanext-exec.

Issue #1 is the epic and lists the slices in order; take one slice per branch. `docs/` arrives
with the first slice; until then the issues are the design.

## Layout

| Path | Holds |
|---|---|
| `doc.go` | The package comment. Packages arrive with their slices. |
| `.claude/hooks/` | gofmt, push and hidden-character checks; their tests run in CI. |
| `.github/workflows/ci.yml` | CI on GitHub's hosted Ubuntu, Windows and macOS runners (public repository). |

## Build and test

Go as pinned in `go.mod`. What CI runs; run the same before a PR (`/check`):

```bash
go vet ./...
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...                                  # Linux only in CI
test -z "$(gofmt -l .)"
for f in .claude/hooks/*.sh; do bash -n "$f"; done                # the hooks parse
sh .claude/hooks/go-rules.test.sh && sh .claude/hooks/push-guard.test.sh
sh .claude/hooks/hidden-chars.test.sh && sh .claude/hooks/hidden-chars.sh
```

`-race` needs cgo; where it is missing run `go test -count=3 ./...` and leave `-race` to CI.
Never attach self-hosted runners: a public repository's pull requests must never reach a
maintainer's machines.

## Rules of this repository

- **Standard library first.** A dependency is argued in its PR; this module decides who may talk
  to Ada and what she may do, so every dependency is trusted with that.
- **Plan and local by default.** No code path sends a request to a metered API unless the user
  enabled it in Ada's settings; a refusal names the setting, never a key.
- **Ask before effects.** Writes, commands and outgoing messages wait for the user's approval.
  Text from chats, emails, web pages or tool output never approves anything or widens a
  permission.
- **Fail closed.** An unknown sender is ignored, an unclassified effect is refused, a limit that
  cannot be enforced is reported.
- **Loopback by default.** Anything that listens binds loopback unless the user turns on the
  network.
- **Tests never call a real model, chat service or account.** Fake providers and fake channels
  only; no tokens in tests or fixtures.
- Every PR updates `docs/` for a behaviour it adds or changes, and ticks its slice in the epic.

## Working rules

- **A branch delivers what it is for, nothing else.** One purpose per branch (one issue; short
  name, as in `provider-layer`) and one PR that closes it, listing the files it touches and why.
  Larger work is a feature branch whose pieces merge into it. A branch finishes everything its
  issue asks.
- **Review findings:** every defect, security point or missing test in what the branch touches is
  fixed in the branch before the push. Nits are dropped. Only work outside the branch, or a
  decision a maintainer must make, becomes an issue.
- **Everything is tested before a merge**: the full local checks (`/check`), tests for every
  behaviour the change adds or alters, CI green, an independent review with its findings fixed,
  and a security read of the diff with nothing open, all on the head being merged. Merge the
  default branch in once before those final checks, or for a conflict. What could not be tested
  is said in the PR and waits for a maintainer. Merge only through the PR
  (`gh pr merge <n> --merge --delete-branch --match-head-commit <sha>`). Never `--auto` or
  `--admin`, never push to `main`, never tag or release: hand over the command. Rulesets protect
  `main` and `v*` tags.
- Commit messages: the area first, lower case, then a plain sentence (`provider: refuse a metered
  endpoint unless enabled`). Findings and reasons go into the PR review, not one commit per
  finding. No emoji, no AI attribution anywhere.
- Tests first, seen failing. Files are LF (`.gitattributes`). Secrets are never read, printed or
  committed. Small diffs: don't restyle what you didn't change. Not sure? Ask, with one proposal.
