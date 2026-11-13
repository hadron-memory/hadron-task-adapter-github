# Design, as built

[hadron-cli#1963](https://github.com/hadron-memory/hadron-cli/issues/1963): the
GitHub intake adapter, published in this public repository.

Governing: `cor:agt:120` r10 (Task integrations: D2 the team's own credential,
D5 a public contract, D7 the label model in the adapter's configuration, D11 an
opaque cursor, D14 the mark never makes a claim), `cor:agt:120:04` (the intake
adapter contract), `cor:agt:120:02` (the external id and its checks). The
invocation format is the hadron CLI owner's, built in hadron-cli#1961 (merged
585bd9d6): `internal/intake` and `docs/plans/1961-intake-adapter-runner.md`
there.

## Shape

One Go executable with no dependencies beyond the standard library, so
`go install` is the whole installation. `cmd/` is a thin `main`;
`internal/adapter` is the protocol, the calls, the configuration and the
GitHub client; `internal/fakegithub` is the test stand-in; `conformance/` is
the suite against the runner's rules.

GitHub is read through GraphQL: one query per repository page, issues in
creation order with their labels and per-issue cursors. Labels are written
through REST (add, remove), and REST also reads labels past GraphQL's first
page of 100.

## Decisions taken here, and why

**The mark is a label.** Removing a label is a clean `untrack`, the label is
visible on GitHub without reading comments, and it is what `list-untracked`
filters on. A comment naming the task and worker is opt-in (`comment: true`)
because on a public repository it publishes the team's worker names and links.
Hadron text in a comment is quoted as inline code on one line, so a name cannot
mention, link or reflow. The portal URL is linked only as the server printed
it, and only a plain https URL.

**Filtering happens in the adapter, after GitHub's paging.** The GitHub list
is all open issues in creation order. The tracked label, the excluded labels,
the team and the ready label are applied to each issue as it is read. So a
mark (a label) never removes an issue from the sequence GitHub pages over, and
a cursor taken before others claimed still resumes at the right place. This is
the property `cor:agt:120:04` asks the opaque cursor to protect ("the set
shrinks while others claim"); `TestMarksBetweenPagesSkipNothing` pins it. Using
GitHub's search API with `-label:hadron-tracked` instead would page over a set
that shrinks between pages, and skip.

**One order across repositories.** Oldest first, then repository, then number:
the order of today's server-side integration (cor:agt:120:03, D7), so a team
moving from it sees the same list. Each call merges the repositories' next
issues; the cursor keeps GitHub's cursor per repository (or `null` once one is
done), so nothing is re-read or skipped.

**The cursor carries its query.** It embeds a fingerprint of the
configuration's filters, its repositories and `for`. A cursor resumed under a
different query is refused (exit 2), not silently resumed at a position that
means something else. With 20 repositories, the configuration's cap, and GitHub
positions up to about 140 bytes each (real ones are about 60), the cursor
stays inside the runner's 4096-byte bound (pinned with padded positions). A
call whose cursor would not fit is refused, never answered with a cursor the
runner would refuse.

**Bounded work per call.** A call reads at most one GitHub page per repository
plus 20 more, then answers with the eligible issues it has and a cursor. A long
run of ineligible issues therefore costs reads, not a timeout. Every call
consumes at least one issue, so the cursor always moves and the runner never
sees a repeated cursor.

**Never a partial answer.** Any GitHub error, a GraphQL `errors` entry, a rate
limit, an issue that lacks a field, or an empty page that still promises more
refuses the whole call. A list is never cut short and presented as whole.

**The external id is GitHub's URL, checked.** Each listed id is the issue URL
GitHub printed, checked to be `https://<webHost>/<owner>/<name>/issues/<n>`
for the repository and number it was read as, and against `cor:agt:120:02`'s
checks. The ids handed back (`mark-tracked`, `untrack`, `get`) are parsed
strictly to that shape and must name a configured repository, so the adapter
acts on nothing else.

**`for` is mapped, or refused.** The runner passes `--for X` as written. The
team map lists, per team, the worker names and URNs it covers, compared without
regard to case. A `for` that no team maps is refused: silently listing every
team's issues for an unknown worker would hand work to the wrong team. Without
`for`, the list is not narrowed by team.

**The credential.** The token comes from `GITHUB_TOKEN`, `GH_TOKEN` or the
variable `tokenEnv` names, never a `HADRON_` variable (the CLI strips those
from an adapter's environment, and `tokenEnv` refuses one). It goes only into
the `Authorization` header, to an https endpoint (plain http only to the
loopback interface, for a local fake or proxy). GitHub's own error text is
scrubbed of it before it is repeated.

**`describe` needs nothing.** It answers without a configuration or a
credential, and whatever contract version the request names: it is how the
CLI learns the adapter's version before asking anything else.

**Request shapes the runner does not yet send.** At 585bd9d6 the runner calls
`describe`, `list-untracked` and `mark-tracked`. `untrack` and `get` (core) and
`normalize` and `check` (optional) are answered here with the shapes the README
states: `{externalId}` for `untrack` and `get`, `{ref}` for `normalize`, nothing
for `check`. hadron-cli#1962 (reconcile and release) is the first caller of
`untrack` and `get`; if it settles another shape, this adapter follows it.

## Conformance

The runner's checks live in hadron-cli's `internal/intake`, which another
module cannot import. `conformance/runner_rules.go` copies them (the bounds,
one JSON object on stdout, the external id's checks, the walk's cursor rules)
and names the hadron-cli revision they come from. The suite builds the adapter
and runs it as the runner does, against `internal/fakegithub`.

Exposing the runner's checks as an importable package in hadron-cli, so this
suite imports them instead of copying, is a hadron-cli change and is left for
a follow-up there.

## Not built

- `report` and `changed-since` (optional calls).
- A release pipeline (goreleaser, tags). `go install` works from a tag or `main`.
