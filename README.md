# hadron-task-adapter-github

The GitHub intake adapter for [hadron-cli](https://github.com/hadron-memory/hadron-cli).
`hadron team task find-untracked --adapter <this executable>` asks it which
GitHub issues nobody is working on yet, and tells it which ones a worker has
claimed. Hadron makes no GitHub call of its own: this adapter reads GitHub with
**your team's own credential** and never sees a Hadron one.

It is a single static binary with no dependencies. The contract between the CLI
and an adapter is public and versioned (`contractVersion` 1; the spec is
`cor:agt:120:04`, the runner is `hadron-cli/internal/intake`, and the rules the
runner enforces are published as `hadron-cli/pkg/intakecontract`).

## Install

```sh
go install github.com/hadron-memory/hadron-task-adapter-github@latest
# or: git clone ... && go build -o hadron-task-adapter-github .
```

Put the binary on the host that runs the CLI, then name it:

```sh
hadron team task find-untracked --adapter /path/to/hadron-task-adapter-github
# or once, for the host:  export HADRON_INTAKE_ADAPTER=/path/to/hadron-task-adapter-github
hadron team task find-untracked --adapter ... --track --for Iris     # claim and mark
```

The CLI removes every `HADRON_*` variable from the adapter's environment, so the
adapter is configured under its own names, below.

## Credential

`GITHUB_TOKEN` is the only credential. Give it the least it needs:

| What you use | Token permission |
|---|---|
| `list-untracked`, `get`, `normalize`, `changed-since` | Issues: read (and read on every repository you list) |
| `mark-tracked` / `untrack` with `trackedLabel`, `report` with labels | Issues: write |
| `mark-tracked` with `comment` | Issues: write (comments) |

The adapter never prints the token: it is scrubbed from any error text that could
carry it, and GitHub's response bodies for HTTP errors are never echoed. For GitHub Enterprise Server set `GITHUB_API_URL`
(e.g. `https://ghe.example.com/api/v3`), `GITHUB_GRAPHQL_URL` if it differs from
`<api minus /v3>/graphql`, and `GITHUB_SERVER_URL` (e.g. `https://ghe.example.com`).

The token is sent **only over https**: an endpoint that is not an `https` URL with
a host, or that carries userinfo, is refused before any request, and a redirect
that downgrades to http or leaves the endpoint's host is refused.

## Config

A JSON file, read from `$GITHUB_ADAPTER_CONFIG`, else
`$XDG_CONFIG_HOME/hadron-task-adapter-github/config.json`, else
`~/.config/hadron-task-adapter-github/config.json`. It holds your team's label
model and **no secret**: keep it out of any public repository. An unknown key is
an error, so a typo cannot silently turn a filter off.

```json
{
  "repositories": ["acme/app", "acme/lib"],
  "excludeLabels": ["wontfix", "blocked"],
  "readyLabel": "ready",
  "requireReady": false,
  "teams": [{"label": "team:alpha", "lead": "Iris"}, {"label": "team:beta", "lead": "Rufus"}],
  "trackedLabel": "hadron:tracked",
  "comment": true,
  "commentAuthor": "",
  "defaultRepository": "acme/app",
  "report": {"FINISHED": {"addLabels": ["done"], "removeLabels": ["hadron:tracked"]}},
  "maxPagesPerRepository": 20
}
```

| Key | Meaning |
|---|---|
| `repositories` | `owner/name` repositories whose **open issues** are listed (pull requests never are). Required. |
| `excludeLabels` | An item carrying any of these (case-insensitive) is never listed. |
| `readyLabel`, `requireReady` | With `requireReady`, only items carrying `readyLabel` are listed. |
| `teams` | Maps a label to the worker who leads that team. `--for <worker>` lists only items carrying the label of the team that worker leads; a worker who leads none is refused. |
| `trackedLabel` | Added by `mark-tracked`, removed by `untrack`. Unset: no label is written. |
| `comment` | `mark-tracked` leaves one comment naming the task and worker; a re-mark (a reassignment) edits that comment, `untrack` marks it released. Unset: no comment. |
| `commentAuthor` | The GitHub login the token acts as. With `comment`, the adapter edits only comments that start with its marker line (`<!-- hadron-task:<id> -->`) **and** were written by this login: a marker in someone else's comment is only text. Unset, the login is read from `GET /user` (a personal or fine-grained token); an installation token (a GitHub App) has none, so name the app's bot login (for example `my-app[bot]`) or `comment` refuses. When a token does name its account, `commentAuthor` must be that account on every comment write, edit and release, or the call refuses and changes nothing; if GitHub cannot be asked (an outage), the call also refuses and can be retried. Only GitHub itself saying the credential has no account (HTTP 404, or the 403 "Resource not accessible by integration" an installation token gets, on `GET /user`) trusts the configured login; a rate limit (including a secondary limit), a plain 403, a 5xx or a network failure refuses. |
| `defaultRepository` | Lets `normalize` accept a bare `#123`. |
| `report` | Maps a Hadron task state to label changes for the `report` call. A state not listed does nothing. |
| `maxPagesPerRepository` | Bound on the tracker read (100 issues a page, default 20). A repository past it is **refused**, never listed in part. |

With neither `trackedLabel` nor `comment` set the adapter only **reads**:
`mark-tracked` and `untrack` check the request and succeed without touching
GitHub. When they do write, they first read the item and refuse unless it is an
**issue** (never a pull request: GitHub's label and comment endpoints accept
both) in a listed repository that exists, so a missing item is an error, not an
"absent label". The writes are idempotent: if one fails after another succeeded
(the label set, the comment not), the CLI keeps the claim and reports the failed
mark, and running `mark-tracked` again completes it. Two simultaneous marks end
with exactly one adapter comment (the oldest is kept and the adapter's own extras
are deleted). The comment scan completes only below 1,000 comments; at 1,000 or more it refuses
rather than guess the adapter has none.

## The calls

Run as `hadron-task-adapter-github <call>`: one JSON object on stdin (it must
carry `contractVersion`: 1; a `null`, a second document, trailing text or more
than 1 MiB is refused), one JSON object on stdout, exit 0. Any other exit is a
refusal and the first line of stderr says why: `1` GitHub or the config refused
or failed, `2` the request is malformed, `4` the item does not exist. Stderr is
always **one line** (an upstream message's line breaks and control characters
become spaces, 1,000 bytes at most). Request fields not named below are ignored.

| Call | Request | Answer |
|---|---|---|
| `describe` | – | `{name, contractVersion, calls, filters: ["for"], order: "oldest-first"}`. Needs no config or token. |
| `list-untracked` | `{limit?, cursor?, for?}` | `{items: [{externalId, title}], nextCursor}`; `nextCursor` is `null` on the last page. |
| `mark-tracked` | `{externalId, task: {id, handle, portalUrl}, worker: {id, name, role, model}}` | `{}` |
| `untrack` | `{externalId}` | `{}` |
| `get` | `{externalId}` | `{externalId, title, body, bodyTruncated, state, url}` (body cut at 64 Ki characters). |
| `report` | `{externalId, task, state}` | `{}` (states match the config's `report` keys without regard to case; two keys that differ only in case are a config error). |
| `changed-since` | `{since: RFC 3339, limit?, cursor?}` | `{items: [{externalId, title, state, updatedAt}], nextCursor}` (open and closed, oldest update first). Each page is read live, so an issue updated between two pages of one traversal can come again on a later page: that is a change, and the consumer deduplicates by `externalId`. |
| `normalize` | `{reference}` | `{externalId}`: `owner/repo#N`, `#N`, or any issue URL form (http, `www.`, a trailing slash, a query or fragment) to the canonical URL. |
| `check` | – | `{credentialPresent, credentialValid, reachable, configValid, detail?, configDetail?}`. Always exit 0, and it runs **without a config** (a bad or missing one is `configValid: false` with `configDetail`), because it is how a bad setup is diagnosed. `credentialValid` is true only when GitHub answered the token's request 2xx (a 401, 403, 404 or 500 is not proof); it never prints the token. |

`normalize`, `report`, `changed-since` and `check` are the optional calls of the
contract; their request and answer shapes above are this adapter's reading of
the Proposed text and are versioned with it.

### Items and ordering

- `externalId` is the issue's **canonical URL exactly as GitHub gives it**
  (`https://github.com/acme/app/issues/12`). It is returned the same way every
  time and passes the id rules (1 to 512 characters, no whitespace, control or
  invisible characters, no userinfo).
- `list-untracked` returns oldest issue first, then repository, then number.
  The CLI, not the adapter, drops what Hadron already holds, so the set shrinks
  while it is paged. The `cursor` is therefore **opaque and positional** (a
  `v1.` token naming the last item's creation time, repository and number),
  never an offset: closing or claiming items between pages neither repeats nor
  skips an item.
- Every call reads GitHub afresh (the adapter keeps no state), so a page costs
  one GraphQL read per 100 open issues of each listed repository.
- Titles are cleaned (control and invisible characters become spaces) and cut at
  1,024 bytes; they are tracker text, shown by the CLI as data.

### Refusals instead of partial answers

The adapter never returns a list it knows is incomplete. It refuses (exit 1)
when GitHub is unreachable or rate limited, when the credential cannot read a
listed repository, when a repository has more pages than `maxPagesPerRepository`,
or when one issue has more than 100 labels (its filters could not be decided).

## Tests

```sh
go test -race ./...                      # unit tests against a fake GitHub
cd conformance && go test ./...          # builds the adapter and checks its answers with
                                         # hadron-cli's published pkg/intakecontract rules
```

The conformance module pins hadron-cli in its own `go.mod`, so the adapter itself
stays dependency-free and builds with an older Go than the CLI's.

## License

MIT, see [LICENSE](LICENSE).
