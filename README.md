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
| `defaultRepository` | Lets `normalize` accept a bare `#123`. |
| `report` | Maps a Hadron task state to label changes for the `report` call. A state not listed does nothing. |
| `maxPagesPerRepository` | Bound on the tracker read (100 issues a page, default 20). A repository past it is **refused**, never listed in part. |

With the defaults above the adapter only **reads**: `mark-tracked` and `untrack`
succeed without touching GitHub until you set `trackedLabel` or `comment`.

## The calls

Run as `hadron-task-adapter-github <call>`: one JSON object on stdin (it always
has `contractVersion`), one JSON object on stdout, exit 0. Any other exit is a
refusal and the first line of stderr says why: `1` GitHub or the config refused
or failed, `2` the request is malformed, `4` the item does not exist. Request
fields not named below are ignored.

| Call | Request | Answer |
|---|---|---|
| `describe` | – | `{name, contractVersion, calls, filters: ["for"], order: "oldest-first"}`. Needs no config or token. |
| `list-untracked` | `{limit?, cursor?, for?}` | `{items: [{externalId, title}], nextCursor}`; `nextCursor` is `null` on the last page. |
| `mark-tracked` | `{externalId, task: {id, handle, portalUrl}, worker: {id, name, role, model}}` | `{}` |
| `untrack` | `{externalId}` | `{}` |
| `get` | `{externalId}` | `{externalId, title, body, bodyTruncated, state, url}` (body cut at 64 Ki characters). |
| `report` | `{externalId, task, state}` | `{}` |
| `changed-since` | `{since: RFC 3339, limit?, cursor?}` | `{items: [{externalId, title, state, updatedAt}], nextCursor}` (open and closed, oldest update first). |
| `normalize` | `{reference}` | `{externalId}`: `owner/repo#N`, `#N`, or any issue URL form (http, `www.`, a trailing slash, a query or fragment) to the canonical URL. |
| `check` | – | `{credentialPresent, credentialValid, reachable, detail?}`. Always exit 0; it never prints the token. |

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
