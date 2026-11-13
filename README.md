# hadron-task-adapter-github

The GitHub **intake adapter** for [Hadron](https://hadronmemory.com) teams. The
`hadron` CLI runs it to find the GitHub issues no task of the team tracks yet,
and to mark the issues a task claims. Hadron itself never calls GitHub: the
adapter runs on the team's host with the team's own GitHub credential.

The adapter implements the public, versioned intake adapter contract
(`cor:agt:120:04` in the Hadron specs), contract version **1**, in the
invocation format of the hadron CLI's adapter runner
([hadron-cli#1961](https://github.com/hadron-memory/hadron-cli/issues/1961)).

## Install

```sh
go install github.com/hadron-memory/hadron-task-adapter-github/cmd/hadron-task-adapter-github@latest
```

It needs a `hadron` CLI that has `team task find-untracked --adapter`
(hadron-cli#1961).

## Use

```sh
export GITHUB_TOKEN=…        # the team's own GitHub token, never a Hadron one
export TASK_ADAPTER_GITHUB_CONFIG=~/.config/hadron-task-adapter-github/config.json

hadron team task find-untracked --adapter hadron-task-adapter-github            # list
hadron team task find-untracked --adapter hadron-task-adapter-github --track    # claim and mark
```

`HADRON_INTAKE_ADAPTER=hadron-task-adapter-github` saves passing `--adapter`.

You can also call the adapter directly to try a configuration:

```sh
echo '{"contractVersion":1}' | hadron-task-adapter-github check
echo '{"contractVersion":1,"limit":5}' | hadron-task-adapter-github list-untracked
```

## Configuration

The configuration is a JSON file, read from `$TASK_ADAPTER_GITHUB_CONFIG`, or
by default from `hadron-task-adapter-github/config.json` under the user
configuration directory (`~/.config` on Linux, `~/Library/Application Support`
on macOS). It holds the team's label model and no credential. **Keep it out of
this repository**, and out of any public one: it names the team's repositories,
labels and workers. [`config.example.json`](config.example.json) shows every
key.

| Key | Meaning |
|---|---|
| `repositories` | `"owner/name"`, 1 to 20. Their **open issues** are the intake (pull requests are not). |
| `readyLabel` | The label that marks an issue ready for intake. |
| `ready` | `only` (list only ready issues; the default when `readyLabel` is set), `exclude`, or `any` (the default without one). |
| `excludeLabels` | An issue carrying any of these is never listed. |
| `teams` | Maps the CLI's `--for X` to a team: `{"for": ["Name", "<worker URN>"], "label": "team-x"}` lists the issues carrying `team-x`; `{"for": [...], "noTeam": true}` lists the issues carrying no team's label. A `--for` that no team maps is refused. Without `--for`, the list is not narrowed by team. |
| `trackedLabel` | The adapter's mark. Default `hadron-tracked`. |
| `comment` | `true` also posts a comment on mark naming the task (and its portal link, as the server printed it) and the worker. Off by default: on a public repository it publishes the team's worker names. |
| `tokenEnv` | The environment variable holding the token. Default: `GITHUB_TOKEN`, then `GH_TOKEN`. |
| `apiUrl`, `graphqlUrl`, `webHost` | For GitHub Enterprise Server: `https://HOST/api/v3`, `https://HOST/api/graphql`, `HOST`. Default github.com. Only `https` is accepted (plain `http` only to the loopback interface). |

Labels are compared without regard to case. Unknown keys are refused, so a
misspelt key fails loudly instead of widening the intake.

### The credential

The adapter reads the team's GitHub token from the environment. It is sent only
in the `Authorization` header to the configured API, and never printed: error
text from GitHub is scrubbed of it. The hadron CLI never passes its own
`HADRON_*` credentials to an adapter, so a `HADRON_` variable cannot be the
token.

A fine-grained token needs, on each configured repository: **Issues: read and
write** (to read issues and to add or remove the mark) and **Metadata: read**.

Create the `trackedLabel` in each repository before the first claim
(`gh label create hadron-tracked -R owner/name`). `check` reports a repository
where it is missing.

## The calls

Every call is `hadron-task-adapter-github <call>`, one argument and no shell.
The request is one JSON object on stdin, always with `"contractVersion": 1`;
the answer is one JSON object on stdout and exit 0. A refusal is a non-zero
exit, a one-line reason on stderr and nothing on stdout.

| Call | Request | Answer |
|---|---|---|
| `describe` | (nothing else) | `name`, `version`, `contractVersion` (1), `calls`, `filters` (`["for"]`), `order` (`created-asc`). Answered whatever version the caller speaks. |
| `list-untracked` | `limit` (1 to 100, default 20), `cursor`, `for` | `items`: `[{externalId, title}]`; `nextCursor`, or `null` at the end of the list. |
| `mark-tracked` | `externalId`, `task` {`id`, `handle`, `portalUrl`}, `worker` {`id`, `name`, `role`, `model`} | `{"marked": true, …}` |
| `untrack` | `externalId` | `{"untracked": true, …}`. An issue with no mark is not an error. |
| `get` | `externalId` | `item`: `externalId` (as asked), `title`, `body`, `state` (`open`/`closed`), `url` (GitHub's canonical URL now), `tracked`. |
| `normalize` (optional) | `ref`: an issue URL, `owner/name#N`, or `#N` with one repository configured | `externalId`: GitHub's canonical issue URL. |
| `check` (optional) | (nothing else) | `ok`, and `checks`: the configuration, the credential, and for each repository that it is readable and has the `trackedLabel`. Always exit 0. |

The optional calls `report` and `changed-since` are not answered yet.

- **The external id** is the issue's canonical URL exactly as GitHub prints it,
  for example `https://github.com/hadron-memory/hadron-server/issues/4084`:
  one spelling for one issue, every time. The CLI writes it as given, and hands
  the same string back to `mark-tracked`, `get` and `untrack`, which accept
  only that shape and only for a configured repository.
- **The list's order** is oldest first across all repositories, then
  repository, then issue number.
- **The cursor** is opaque. It records, for each repository, GitHub's own
  position, and the query it belongs to: a cursor from another `--for` or from
  before the configuration changed is refused, not resumed somewhere wrong. The
  set of open issues does not change when an issue is marked (the mark is a
  label, and filtering happens in the adapter), so a cursor taken before others
  claimed still resumes at the right place and skips nothing.
- **The mark** is the `trackedLabel`, and optionally a comment. It never makes
  or decides a claim: the claim is the CLI's task create. A second mark of the
  same issue (after a reassignment) is an ordinary call, never a refusal.
- **Never partial.** A GitHub error, a rate limit, or an answer the adapter
  does not understand refuses the whole call; a list is never cut short and
  presented as whole. Labels past GitHub's first page of 100 are read too,
  since they decide the filters.
- **Labels never reach Hadron.** No answer carries an issue's labels.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | answered |
| 1 | GitHub refused, failed, or answered something unusable |
| 2 | the request: an unknown call, not one JSON object, another contract version, a bad argument or cursor |
| 3 | the configuration or the credential |

The hadron CLI reads any non-zero exit as the adapter's refusal.

## Development

```sh
go test -race ./...    # includes the conformance suite
golangci-lint run      # the CI lint, gofmt included
```

[`conformance/`](conformance) builds the adapter and drives it the way the
hadron CLI's runner does, against a fake GitHub
([`internal/fakegithub`](internal/fakegithub)), checking every answer against
the runner's rules. Those rules are copied from hadron-cli (its runner package
is internal to the CLI module), and `conformance/runner_rules.go` names the
hadron-cli revision they were copied from. When the runner changes a bound,
change it there too.

The design, and the decisions it rests on, is in [`docs/design.md`](docs/design.md).

## License

MIT, as hadron-cli.
