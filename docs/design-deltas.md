# Where the implementation departs from the design

The design document is
[agent-context-broker-design.md](../agent-context-broker-design.md). This file
records every place the code does something other than what that document
says, and why. Anything not listed here follows the design.

## Tool names use underscores, not dots

The design names the tools `context.prepare_task`, `context.search`,
`context.get_records`, and `context.propose_record`. The implementation uses
`context_prepare_task` and so on.

Several agent hosts pass an MCP tool name straight into a function-calling API
whose name grammar is `[A-Za-z0-9_-]{1,64}`; OpenAI's is one of them, and Codex
is a named target host. A dot makes the tool unusable there, silently. The
names are otherwise identical, and `mcpserver/server_test.go` asserts that
every advertised name stays inside that grammar.

## Configuration is YAML, not TOML

The design's suggested filesystem layout names `config.toml`. The
implementation reads `config.yaml`.

Record source files are already YAML, so this keeps one serialization format
and one dependency instead of two. The layout is otherwise as designed:
platform config, data, and state directories, under `mecp/`.

## An unsupplied task kind is unknown, not "other"

The design's input schema gives `task_kind` a default of `other`. In the
implementation, an omitted `task_kind` stays empty and the task-kind dimension
of a scope is simply not evaluated: a record scoped to `release` still applies,
but earns no specificity bonus for it.

Defaulting to `other` meant that any host which omitted the field could never
see a task-scoped record, because `other` matches no declared task kind. That
turned an optional field into a silent filter. An explicitly stated `other` is
still a claim about the work and is matched strictly.

## Audit events go to JSONL by default, with a SQLite table available

The design lists `audit_events` in the relational schema and `audit.jsonl` in
the filesystem layout. Both exist. The table is created by the migration and
backs `sqlite.AuditSink`; the default is the JSONL file, selected by `audit:` in
the configuration.

The default matters for the agent-facing process: it opens the database
read-only unless the client profile can propose, and a read-only store cannot
accept the SQLite sink. Rather than silently dropping the audit trail, that
configuration falls back to JSONL and says so on stderr.

`mecp audit` reads back from whichever sink `audit:` selects, so the default
installation is readable without opening the log file by hand. Because the
fallback above can leave events in both places, a run against the SQLite sink
says on stderr when the JSONL log is non-empty as well.

## Audit events also record the interface the call came through

The design's audit event lists the client profile and the principal, which
between them do not say whether a call arrived over MCP or from the CLI. The CLI
runs the same code path on purpose, so `mecp prepare --client claude-code`
writes a line identical to the one that agent's own call writes, and the trail
cannot answer what actually happened.

Every event therefore carries an `origin`, stamped by the boundary the call came
through: `mcpserver.New` sets `mcp` over whatever it was handed, and the CLI
sets `cli` where it resolves an identity. The service copies it from the caller
in one place, so a new operation cannot ship an event without one.

The field is absent from events written before it existed. Those decode to an
empty origin and display as `unknown`, which is neither interface: an old line
is never read as an agent call, and no migration is needed for either sink.

## Context handles are in-process

The design describes a context-pack cache keyed by principal, client, revision,
task, budget, database content version, and ranker version. The implementation
issues context handles from an in-memory map with a TTL, and does not yet cache
the packs themselves. `Store.ContentVersion` and `Ranker.Version` exist and are
what such a cache would key on.

Each MCP process is short-lived and per-client, so a handle does not need to
outlive it. Nothing about the tool contract changes when a real cache is added.

## Semantic retrieval is absent

The design makes embeddings an optional supplementary ranking signal, after
structured filters and FTS. None are implemented. The retrieval pipeline is
structured filtering, FTS5, and a deterministic ranker.

This is the design's stated priority order, not a departure from it — noted
here only because it is the most visible unimplemented option.

## Not built in this pass

These are named in the design and deliberately left out of the initial version.

- **Daemon mode and Streamable HTTP.** Milestone 6, gated on measurements that
  do not exist yet. `mecp mcp` refuses any transport but stdio rather than
  pretending.
- **Remote access and signed context capsules.** Excluded from the MVP by the
  design itself.
- **Conversation and issue-tracker adapters.** The file importer and the Git
  validator are in; chat exports, GitHub issues, and agent-memory imports are
  not.
- **Model-assisted distillation.** Extraction from source material into
  candidate proposals is manual for now. The proposal and review machinery it
  would feed is complete.
- **A local review UI.** `mecp review` shows the proposed statement beside the
  quoted evidence, which is the side-by-side comparison the design asks for.

## There are no privacy labels

The design gives every record one of four sensitivity levels, gives every
client profile a ceiling, and gates verbatim evidence through a second set of
capabilities. None of that is implemented. A record has no privacy field, and a
profile has no ceiling.

The reasoning is that every record exists to be sent to a model. A record you
would never send does nothing, so the rule is not to store it in the first
place. That is the same reasoning the design already applies to credentials,
carried to its conclusion. A ladder of levels also assumes agents can be ranked
from less to more trusted, and Claude Code and Codex are not ranked. They are
two different companies.

Removing the ladder removes something real, so what remains is worth stating.
The principal and the repository allowlist still contain disclosure, and they
are what keep one project's context out of another. `context:evidence` still
separates reading a record's normalized statement from reading the verbatim
source text it was made from, because that text is the raw material and it is
the field a prompt injection arrives in.

Two capabilities went with the levels. `context:search:project` and
`context:search:personal` became `context:search`; `context:evidence:project`
and `context:evidence:personal` became `context:evidence`.

## A client profile is not a security control

The profile is chosen by a command-line flag in the MCP host's configuration,
so anyone who can edit that file can select any profile. On stdio that is not a
weakness, because the process boundary is already the identity: whoever can
launch the server runs as the user and can open the database directly.

It stops being true the moment a socket or an HTTP endpoint exists. MCP's
authorization model is built on OAuth and applies to the HTTP transports; the
specification deliberately leaves stdio out and says credentials should come
from the environment. So authentication and per-client limits arrive together
with the first remote transport, and not before.
