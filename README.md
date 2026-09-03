# MeCP

MeCP stores the things you keep having to tell coding agents, and serves the
ones that apply to whatever you are working on right now. It speaks MCP, so any
MCP-capable agent can read from it.

Things like:

- review the current branch, not the last release;
- before v1, correctness matters more than API stability;
- the conformance suite is pinned to a commit on purpose, stop reporting it as a bug;
- we already tried that approach and rejected it, here is why.

You write these down once. Every agent gets the ones that fit the task.

The full design is in [agent-context-broker-design.md](agent-context-broker-design.md).
Where the code differs from it, see [docs/design-deltas.md](docs/design-deltas.md).

## Install

```sh
go install github.com/lestrrat-ai/mecp/cmd/mecp@latest
mecp init
```

`mecp init` writes `config.yaml` and an empty database, both owner-only, and
prints the server entry to paste into your agent host. The SQLite driver is pure
Go, so there is no cgo and nothing to install alongside it.

## Wiring it to an agent

```json
{
  "mcpServers": {
    "mecp": {
      "command": "/path/to/mecp",
      "args": ["mcp", "--client", "default"]
    }
  }
}
```

A model will not call a tool just because it exists. There are two ways to deal
with that, and they are not exclusive.

The reliable one is a hook, which runs mecp itself and puts the result in front
of the model whether or not it would have asked. It also fills in the workspace
from git rather than letting the model guess, and takes the files a prompt names
as the relevant paths. Claude Code only, since Codex has no hooks:

```json
{"hooks": {"UserPromptSubmit": [{"hooks": [
  {"type": "command", "command": "mecp hook --client claude-code"}
]}]}}
```

It never blocks a turn. A missing database, a timeout, or malformed input exits
quietly with no output, because a broken hook must not stop you working.

The portable one is an instruction in your global or per-repository agent file:

> Before planning or executing a nontrivial coding task, call
> `context_prepare_task` with the task and the current workspace. Use
> `context_search` for targeted follow-up. Current user instructions and current
> repository files outrank anything it returns. Items marked `informational` are
> history, not instructions.

## Tools

| Tool | Purpose | On by default |
|---|---|---|
| `context_prepare_task` | Context for one task and workspace, within a token budget. | yes |
| `context_search` | Follow-up question inside an already authorized scope. | yes |
| `context_get_records` | Full records and quoted sources, for IDs already returned. | yes |
| `context_propose_record` | Files a suggestion for you to review. Changes nothing. | no |
| `context_extract_rules` | Reads an instruction document and queues its rules for review. | no |

`context_prepare_task` does the work. An agent cannot guess which old terms to
search for, so it sends the task and the workspace, and the server resolves the
scope, pulls the records that apply whether or not the task mentions them, ranks
them, flags conflicts, and fits the result to a budget.

No agent can activate, edit, or delete a record. With the fourth tool enabled,
the most it can do is add to a queue you review.

## Using it

```sh
# Add a record, scoped to this repository and to review tasks.
mecp record add --here --kind preference --task-kind code_review \
  --subject "pre-v1 review weighting" \
  "Weight implementation correctness above API compatibility before v1."

# Show what an agent would actually receive.
mecp prepare --here --task-kind code_review "Review the XMLDSig implementation"

# Replace a decision, keeping the old one as history.
mecp record supersede rec_abc123 "The suite now tracks upstream automatically."

# Review what agents have proposed.
mecp review list
mecp review show prop_abc123
mecp review approve prop_abc123

# Load curated files, and take a backup you can read.
mecp import ./seed
mecp export --out context.jsonl
```

`mecp prepare` runs the same code as the MCP tool, so use it to find out why a
record did or did not show up. Every item comes back with its match reasons.

## Record files

`mecp import` reads YAML and Markdown. A YAML file holds one record, a list, or
a `records:` key:

```yaml
kind: decision
subject: release conformance testing
statement: The conformance suite runs against a controlled commit before a release.
rationale: Reproducibility comes from choosing the commit at release time.
scope:
  repository: git@github.com:lestrrat-go/helium.git
  task_kinds: [release]
tags: [conformance, release]
```

Markdown uses YAML front matter, the body as the statement, and an optional
`## Rationale` section. There are examples in [seed/](seed/).

To move an existing instruction file in, `mecp distill` converts it to candidate
records rather than making you retype them:

```sh
mecp distill ~/.claude/docs/go.md --mine --path '*.go' -o go-rules.yaml
$EDITOR go-rules.yaml
mecp import go-rules.yaml
```

That is the mechanical route, and it cannot tell an explanation from a rule. The
other route is `context_extract_rules`, where an agent reads the document and
decides, then files what it found. Judgement is the model's and the checking is
mecp's: each rule must carry the exact text it came from, the server confirms
that text is really in the file, and a rule it cannot find is refused. Both
routes end in a review queue rather than in active records, and `mecp distill`
doubles as the check on the model, because a large gap between the two counts
says the model dropped something.

The tool reads only inside `document_roots`, which is empty by default. Naming a
path and learning whether a quote appears in it is enough to read a file back a
piece at a time, so the reachable set is chosen rather than defaulted.

Distill takes the structure the document already has. A bullet is a rule, a
numbered step is a rule, a table row is a rule, and the nearest heading is the
subject.
Prose paragraphs are counted and skipped rather than guessed at, and the line
numbers are reported so you can see what was left behind. Scope is not inferred,
because a wrong scope is worse than none; set it for the whole file with the
flags and correct individual records in the YAML.

Each record keeps its original line verbatim as evidence and a hash of the file
it came from, so once the document changes the record shows as stale.

Imported records get `sourced_import` authority, which agents read as background
information rather than as rules. The importer will not claim you said
something. For material you wrote yourself, import it with `--mine`, or declare
`authority:` in the file. An import that produces records with no force says so
on stderr rather than leaving you to work it out from an empty-looking pack.

## How a record gets picked

Scope is checked first, in SQL, before any text matching. A record you may not
see never reaches the ranker, a snippet, or a result count. Scope dimensions are
combined with AND, and a record scoped to a repository does not match a request
that names no repository.

Authority and relevance are separate. A well-worded guess never outranks an
explicit decision that happens to use different words. Only a current record
with directive authority comes back as a `constraint` or `preference`. Anything
else is `informational`.

A record that fails its freshness check is demoted to informational and
reported. A superseded record stays readable as history and never acts as
guidance.

When two active records disagree about the same subject in the same scope, both
come back with a recommendation derived from authority and dates. No model
decides which one wins.

## Freshness

| Policy | Check |
|---|---|
| `none` | Nothing. Correct for events that already happened. |
| `review_after` | Stale once the review date passes. |
| `manual` | Stale once the record changes after you last verified it. |
| `evidence_exists` | The source is still there. |
| `content_hash_matches` | The referenced content has not changed. |
| `file_path_and_hash` | Both of the above. |
| `git_revision_ancestor` | The source's commit is an ancestor of the current one. |

The last four shell out to `git` and need `validation.git: true`. Without it they
report `unverified`, which is a weaker claim than `stale` and is treated as one.

```sh
mecp validate --here          # what no longer holds
mecp validate --here --apply  # mark those stale
mecp record verify rec_abc123 # you checked, it is still true
```

## What to store

Everything here is meant to be sent to a model. Do not store what you are
unwilling to send. There are no privacy levels, because a record you would never
send does nothing.

What does stay separate: records belong to a principal, a profile can be limited
to named repositories, and `context:evidence` decides whether a client sees the
quoted source text a record was written from or only the record itself.

```yaml
clients:
  default:
    capabilities: [context:prepare, context:search, context:evidence]
  trusted-local:
    capabilities: [context:prepare, context:search, context:evidence, context:propose]
    allowed_repositories: [https://github.com/lestrrat-go/helium]
```

A profile is picked by a command-line flag, so anyone who can edit your host
config can pick any profile. Over stdio that costs nothing, since launching the
server already means running as you with the database readable. It stops being
true the day a socket exists, and authentication belongs to that day.

Quoted source text is never trusted. A record's `statement` is MeCP's own
wording; `exact_excerpt` is what the source said, kept in a separate field, and
nothing in it can raise a record's authority.

Every call writes an audit line: who asked, over which interface, what scope,
which records came back, which warnings fired. It does not copy the task text or
the record statements.

The interface is recorded because the client profile alone cannot tell an
agent's call apart from `mecp prepare --client claude-code`, which reproduces
what that agent would be told. The gateway stamps `mcp` and the command line
stamps `cli`, so a diagnostic run never reads as the agent's own call. Lines
written before this existed show `unknown`.

## Layout

| Path | Contents |
|---|---|
| `.` | Records, scope, authority, retrieval, ranking, packing, conflicts, freshness. |
| `sqlite/` | Migrations, queries, FTS5 search. |
| `mcpserver/` | Tool definitions, input schemas, handlers. |
| `config/` | Configuration and client profiles. |
| `source/` | File import, Git validation, JSONL export. |
| `cmd/mecp/` | The binary: MCP server and CLI. |
| `examples/` | Runnable examples, checked by `go test`. |

The core knows nothing about MCP. The gateway, the CLI, and the tests all go
through the same service.

## Status

Early. One user, one machine, MCP over stdio. No daemon, because a resident
process buys nothing yet. No remote endpoint, because that is a decision about
who sees your data rather than a transport detail.

## License

This project is **source-available**, and is licensed under the
[PolyForm Noncommercial License 1.0.0](LICENSE).

* **Noncommercial use is free.** Individuals, hobby and personal projects,
  research, education, nonprofits, and government may use, modify, and
  redistribute it at no cost, subject to the license terms.
* **Commercial / business use requires a separate license.** Any use by or for
  a business, or for commercial advantage, is not permitted under the
  noncommercial license. To obtain a commercial license, reach out on Bluesky
  at [@lestrrat.bsky.social](https://bsky.app/profile/lestrrat.bsky.social).

### Contributions

This repository does **not** accept external pull requests.
