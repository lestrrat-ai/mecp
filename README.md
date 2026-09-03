# MeCP

MeCP is a local-first context broker for coding agents. It stores your durable
preferences, decisions, constraints, and project history, and serves the slice
of that which applies to a concrete task — over MCP, so any MCP-capable agent
can use it.

It is not a memory dump. Every record carries scope, authority, provenance,
lifecycle state, and freshness metadata, and the service uses all of them to
decide what to return and how much weight the agent should give it.

The full design is in [agent-context-broker-design.md](agent-context-broker-design.md).
Where this implementation departs from that document, the reasons are in
[docs/design-deltas.md](docs/design-deltas.md).

## The problem

An agent starts most sessions knowing nothing about earlier work, so you repeat
yourself: review the branch and not the release, weight correctness above API
stability before v1, stop reopening a concern that was already settled, the
conformance suite is pinned on purpose. The information exists — in
conversations, repositories, ADRs, issues, commits — but it is scattered, and a
plain semantic search over it cannot tell you whether a statement is still
current, whether it applies here, or whether it is safe to disclose.

MeCP's operating rule is **centralized retrieval, decentralized authority**. It
is one place to ask. It does not become a competing source of truth: your
current instructions and your repository's checked-in files still win.

## Install

```sh
go install github.com/lestrrat-ai/mecp/cmd/mecp@latest
mecp init
```

`mecp init` writes `config.yaml` and an empty database with owner-only
permissions, and prints the MCP server entry to paste into your agent host.

The SQLite driver is pure Go, so there is no cgo and no system SQLite to
install.

## Wire it into an agent

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

Then tell the agent when to call it. Availability does not make a model use a
tool; add something like this to your global or repository instructions:

> Before planning or executing a nontrivial coding task, call
> `context_prepare_task` with the task and the current workspace. Use
> `context_search` only for targeted follow-up. Treat current user instructions
> and current repository files as higher priority than anything it returns.
> Records marked `informational` are history, not instructions.

## The tools

| Tool | What it does | Enabled by default |
|---|---|---|
| `context_prepare_task` | Builds a bounded context pack for one task and workspace. | yes |
| `context_search` | Targeted follow-up inside an already authorized scope. | yes |
| `context_get_records` | Full records and bounded evidence for IDs already returned. | yes |
| `context_propose_record` | Files an inactive proposal for you to review. | no |

`context_prepare_task` is the important one. An agent does not know which
historical terms to search for, so the service does the work: it resolves the
scope, pulls the records that apply whether or not the task text mentions them,
ranks them, detects conflicts, and packs the result into a token budget.

Agents cannot activate, edit, or delete anything. The most a write-enabled
agent can do is file a proposal that sits in a queue until you approve it.

## Curating records

```sh
# Add a record scoped to this repository and to review tasks.
mecp record add --here --kind preference --task-kind code_review \
  --subject "pre-v1 review weighting" \
  "Weight implementation correctness above API compatibility before v1."

# See exactly what an agent would receive.
mecp prepare --here --task-kind code_review "Review the XMLDSig implementation"

# Replace a decision without losing the history.
mecp record supersede rec_abc123 "The suite now tracks upstream automatically."

# Work the proposal queue.
mecp review list
mecp review show prop_abc123
mecp review approve prop_abc123

# Import curated files, and take a portable backup.
mecp import ./seed
mecp export --out context.jsonl
```

`mecp prepare` runs the same code as the MCP tool, so it is the way to find out
why a record did or did not apply. Every returned item carries its match
reasons.

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

A Markdown file uses YAML front matter, with the body as the statement and an
optional `## Rationale` section. See [seed/](seed/) for a starting set.

Imported records get `sourced_import` authority. An importer never claims
`explicit_user` on your behalf.

## How a record is chosen

**Scope before search.** Authorization and scope filtering happen in SQL,
before any text matching, so a record you may not see never reaches the ranker,
a snippet, or even a result count. Scope dimensions are conjunctive: a record
scoped to a repository does not match a request that names no repository.

**Authority is not relevance.** They are separate axes. A well-worded agent
inference never outranks an explicit user decision that happens to use
different words. Only a current record with directive authority is presented as
a `constraint` or `preference`; everything else is `informational`.

**Missing beats stale.** A record that fails freshness validation is demoted to
informational and reported, rather than returned as if it were current. A
superseded record stays retrievable as history but never acts as guidance.

**Conflicts are surfaced, not resolved.** When two active records disagree
about the same subject in the same scope, both are returned with a
deterministic recommendation. No language model decides which one wins.

## Freshness

Each record declares how its continued truth is checked:

| Policy | Check |
|---|---|
| `none` | Nothing; correct for immutable historical events. |
| `review_after` | Stale once the review date passes, whatever else holds. |
| `manual` | Stale once the record changes after its last manual verification. |
| `evidence_exists` | The source is still there. |
| `content_hash_matches` | The referenced content has not changed. |
| `file_path_and_hash` | Both of the above. |
| `git_revision_ancestor` | The source's commit is an ancestor of the current one. |

The policies that touch the filesystem or Git need `validation.git: true` in
the configuration; they shell out to `git`. Without it they report
`unverified`, which is a weaker claim than `stale` and is treated as such.

```sh
mecp validate --here          # report what no longer holds
mecp validate --here --apply  # mark the failures stale
mecp record verify rec_abc123 # you checked; it is still true
```

## Privacy and disclosure

Each client profile declares capabilities and a sensitivity ceiling, and the
effective ceiling is the lower of what it is granted and what its capabilities
imply — a misconfigured profile cannot widen disclosure by accident.

```yaml
clients:
  default:
    capabilities: [context:prepare, context:search:project, context:evidence:project]
    max_sensitivity: project
  trusted-local:
    capabilities: [context:prepare, context:search:personal, context:evidence:personal, context:propose]
    max_sensitivity: personal
    allowed_repositories: [https://github.com/lestrrat-go/helium]
```

Verbatim evidence is gated more tightly than record statements: a client can
learn that a preference applies without being handed the conversation it came
from. An unrecognized client falls back to the `default` profile, and with no
`default` it gets no capabilities at all.

Imported source text is never trusted. A record's `statement` is the broker's
normalized assertion; `exact_excerpt` is quoted material kept in its own field,
and no wording inside it can raise a record's authority or change tool policy.

Every call writes an audit event — who, what scope, which record IDs, which
sensitivity classes, which warnings — without copying the task text or the
record statements into the log.

## Layout

| Path | What lives there |
|---|---|
| `.` | Domain model, scope and authority rules, retrieval, ranking, packing, conflicts, freshness. |
| `sqlite/` | The store: schema migrations, structured queries, FTS5 search. |
| `mcpserver/` | The MCP gateway: tool definitions, input schemas, handlers. |
| `config/` | Configuration loading and client profiles. |
| `source/` | Adapters: file import, Git validation, portable JSONL. |
| `cmd/mecp/` | The single executable, both server and administrative CLI. |
| `examples/` | Runnable examples, verified by `go test`. |

The core is transport-independent. The same service backs the MCP gateway, the
CLI, and the tests, so a second transport cannot bypass the disclosure rules.

## Status

Initial implementation. Single user, local, MCP over stdio. There is no daemon
and no remote endpoint: an always-running process buys nothing yet, and remote
access is a deliberate disclosure decision rather than a transport detail.

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
