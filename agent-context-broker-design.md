---
title: "Agent Context Broker"
subtitle: "Design document for a local-first personal knowledge service for coding agents"
date: "3 September 2026"
lang: en-US
---

| Field | Value |
|---|---|
| Status | Draft for implementation review |
| Version | 0.1 |
| Primary scope | Single-user, local-first deployment |
| Primary implementation language | Go |
| Agent-facing protocol | MCP tools |
| Default local transport | MCP over stdio |
| Persistent store | SQLite with FTS5 |

**Decision at a glance.** Coding agents interact with the system through a small set of task-oriented MCP tools. The knowledge model, retrieval engine, and storage layer are transport-independent. A bespoke RPC protocol may be introduced between local processes if a daemon becomes necessary, but it is not exposed to agents and does not become part of the compatibility contract.

```{=openxml}
<w:p><w:r><w:br w:type="page"/></w:r></w:p>
```

# Contents

| Sections 1-10 | Sections 11-Appendices |
|---|---|
| 1. Executive summary | 11. Ingestion and curation |
| 2. Context and problem statement | 12. Operational design |
| 3. Goals, non-goals, and success criteria | 13. End-to-end workflows |
| 4. Design principles | 14. Testing and evaluation |
| 5. Key architectural decisions | 15. Implementation plan |
| 6. System architecture | 16. Alternatives considered |
| 7. Agent-facing MCP interface | 17. Open questions |
| 8. Domain and storage model | 18. Final recommendation |
| 9. Retrieval and context-packing design | Appendix A. Suggested Go domain interface |
| 10. Security and privacy | Appendix B. MCP schema excerpts |
|  | Appendix C. References |

```{=openxml}
<w:p><w:r><w:br w:type="page"/></w:r></w:p>
```

# 1. Executive summary

Coding agents repeatedly lose context that materially affects their work: personal preferences, project history, rejected alternatives, prior review findings, and the rationale behind decisions. Existing repository instructions solve only the subset that should travel with the repository. Vendor-specific agent memories solve only the subset available to one product. Conversation archives contain more history, but they do not provide a stable, scoped, authoritative interface.

This document proposes an **Agent Context Broker**: a local-first service that gives coding agents controlled access to durable personal and project context. It centralizes retrieval, policy, correction, and audit, while leaving repository files, ADRs, commits, issues, and current user instructions as the authoritative sources.

The public agent-facing interface is **Model Context Protocol (MCP) tools**. MCP is selected because it is an agent-oriented, schema-described RPC layer supported by major coding-agent environments. The system does not expose its database, vector index, or generic CRUD operations. It exposes intent-level operations:

- prepare context for a concrete task;
- search relevant prior decisions, preferences, and history;
- retrieve the evidence behind selected records; and
- optionally propose a new record for user review.

The default local integration is MCP over stdio. Each client launches a small server process that uses the same local store and policy configuration. The initial implementation does not require an always-running daemon. If centralized indexing, remote access, or stronger process isolation later justifies a daemon, the same domain service can be exposed privately over a Unix-domain socket or equivalent local IPC. That private RPC remains an implementation detail.

The system stores typed records rather than undifferentiated memories. Every record has explicit scope, authority, provenance, lifecycle state, freshness metadata, and sensitivity. Agent-generated conclusions are proposals or inferences; they never silently become authoritative user preferences or project decisions.

The principal operational rule is:

> **Centralized retrieval, decentralized authority.**

The broker provides one query surface, but it does not become a competing source of truth.

# 2. Context and problem statement

## 2.1 Problem

A coding agent begins most sessions with limited knowledge of earlier work. The user consequently repeats instructions such as:

- review the latest branch rather than a released version;
- give implementation correctness more weight than API stability for a pre-v1 project;
- do not reopen a design concern whose mitigation has already been implemented;
- preserve a deliberately chosen conformance-test workflow;
- distinguish an acknowledged trade-off from an unresolved defect; and
- remember why a cryptographic representation or protocol choice was made.

The information exists, but is fragmented across conversations, repositories, ADRs, issue trackers, commits, notes, and vendor-specific memory systems. The fragmentation causes four recurring failures:

1. **Repetition.** The user must restate the same context to every agent and in every new session.
2. **Inconsistency.** Different agents infer different preferences or use different parts of the history.
3. **Staleness.** An old decision is presented as current after the project has moved on.
4. **Overreach.** An agent receives personal or project information unrelated to the task.

A generic semantic-search database does not solve these failures by itself. It can retrieve similar text, but it does not know whether a statement is current, authoritative, scoped to this repository, contradicted by a newer decision, or safe to disclose to this client.

## 2.2 Proposed solution

The Agent Context Broker is a user-owned knowledge and policy layer that:

- stores curated assertions and references to source material;
- derives the active scope from the task and workspace;
- filters unauthorized or irrelevant records before retrieval;
- ranks records by relevance, scope, authority, and freshness;
- reports conflicts and stale information explicitly;
- packs the result into a bounded context payload; and
- provides provenance so the agent can inspect supporting evidence.

The broker is primarily read-oriented. Coding agents may propose new records, but activation, editing, supersession, and deletion occur through a user-controlled administrative interface.

## 2.3 Terminology

**Record** means a typed, scoped assertion such as a preference, decision, constraint, historical event, rejected alternative, or open question.

**Evidence** means the source material supporting a record: a conversation span, ADR, issue, commit, file revision, note, or other artifact.

**Context pack** means the bounded result of preparing context for a task. It contains the most applicable records, warnings, conflicts, and source references.

**Authority** describes why a record should be trusted, not how confidently an embedding model matched it. An explicit user decision has higher authority than an agent inference even when the inference has a higher retrieval score.

**Scope** describes where a record applies: user-wide, organization-wide, repository-specific, branch-specific, path-specific, task-specific, or some combination.

**Agent client** means Codex, Claude Code, GitHub Copilot, a custom coding agent, or another MCP-capable host.

# 3. Goals, non-goals, and success criteria

## 3.1 Goals

The system shall:

1. Provide a vendor-neutral agent-facing interface usable by multiple coding agents.
2. Return context that is relevant to a concrete task and workspace, rather than dumping a personal profile.
3. Preserve provenance, authority, scope, and lifecycle state for every durable assertion.
4. Make stale, superseded, disputed, and conflicting records visible rather than silently resolving them.
5. Keep the user in control of durable writes and corrections.
6. Operate offline for local coding tasks.
7. Support repository instructions and source documents as higher-authority inputs instead of replacing them.
8. Bound response size and permit the caller to request a context budget.
9. Prevent one repository or client from retrieving unrelated personal or project context.
10. Keep the MCP contract stable while storage, ranking, and process topology evolve.

## 3.2 Non-goals

The initial system will not:

- replace `AGENTS.md`, `CLAUDE.md`, ADRs, project documentation, or issue trackers;
- act as a general-purpose personal assistant memory for every life domain;
- store credentials, private keys, access tokens, or other secrets;
- autonomously convert every conversation into permanent high-authority memory;
- expose raw SQL, unrestricted vector search, or generic database CRUD to agents;
- guarantee that every agent host will invoke the tools without an accompanying instruction or hook;
- provide multi-tenant SaaS operation in the MVP;
- make remote cloud-agent access a prerequisite for the local product; or
- use an LLM as the sole mechanism for access control, precedence, or conflict resolution.

## 3.3 Success criteria

The MVP is successful when all of the following hold:

- A supported coding agent can call `context.prepare_task` before a nontrivial task and receive a useful context pack.
- Records outside the authorized user, repository, sensitivity, or task scope are never returned.
- Superseded records are excluded from active guidance but remain retrievable as history.
- Every returned durable assertion identifies its authority and at least one source reference.
- The user can inspect, approve, edit, supersede, reject, and delete proposed records.
- A repeated evaluation set shows materially fewer repeated corrections and fewer stale-context errors than an agent operating without the broker.
- Local read operations remain fast enough that agents can call the service routinely rather than treating it as an expensive fallback.

# 4. Design principles

## 4.1 Centralized retrieval, decentralized authority

The broker is the common query point. It is not automatically the canonical source for project facts. Current repository instructions and source documents remain authoritative and can invalidate a broker record.

## 4.2 Task-oriented, not database-oriented

The agent asks, “What context matters for this task?” It does not operate a knowledge database. Storage details remain hidden behind domain operations.

## 4.3 Scope before search

Authorization and scope filtering occur before lexical or semantic retrieval. A disallowed record must not enter the candidate set, result count, snippet generation, or ranking pipeline.

## 4.4 Provenance over plausible summaries

A record without evidence may still exist as a proposal or inference, but it cannot masquerade as an explicit user decision. The service always distinguishes a source-derived assertion from an agent-generated interpretation.

## 4.5 Missing context is safer than stale authority

When the service cannot validate a time-sensitive project assertion, it marks the record stale or omits it from active guidance. It does not confidently return an obsolete decision because it is semantically similar.

## 4.6 Read and write paths are separate

Read tools may be broadly enabled for trusted local agents. Durable writes require a proposal and user review. This prevents self-reinforcing agent fiction.

## 4.7 Bounded context is a product feature

The service returns the smallest useful set of records that fits the requested budget. More retrieved text is not inherently better; excessive context reduces signal and increases disclosure risk.

## 4.8 Explicit precedence

Current user instructions outrank all stored context. Current repository instructions outrank personal historical observations. An authority model is applied independently from retrieval relevance.

# 5. Key architectural decisions

| ID | Decision | Rationale |
|---|---|---|
| D1 | MCP tools are the public agent-facing interface. | MCP provides a standardized, schema-described tool contract across multiple agent hosts. A custom protocol would require a separate adapter and tool description for each host. |
| D2 | Tools are the required MCP primitive; resources and prompts are optional additions. | Tools are model-invokable and have the broadest current compatibility. Some agent surfaces support tools but not resources or prompts. |
| D3 | `context.prepare_task` is the primary operation. | An agent does not know which historical terms to search for. Task preparation lets the service perform scope resolution, ranking, conflict detection, and budget packing in one call. |
| D4 | Responses use `structuredContent` with an output schema and a concise text fallback. | Agents receive machine-readable fields while clients with limited structured-result support still receive a usable summary. |
| D5 | Agent writes are disabled by default; an optional write tool creates pending proposals only. | An agent should not promote its own inference into authoritative memory. |
| D6 | The semantic contract is stateless. | Every call carries scope or an opaque, expiring context handle. Correctness never depends on an implicit conversational session. |
| D7 | SQLite plus FTS5 is the initial store and retrieval substrate. | The expected workload is local, read-heavy, structured, and rich in exact identifiers. SQLite is inspectable, portable, transactional, and sufficient for the MVP. |
| D8 | Lexical and structured retrieval precede optional embeddings. | Repository names, symbols, versions, issue numbers, and negated decisions require exact matching and metadata filters. Vector similarity is a supplementary signal, not the authority mechanism. |
| D9 | The core is a transport-independent Go package. | The same domain operations can back MCP stdio, Streamable HTTP, a CLI, tests, and a future private daemon RPC. |
| D10 | No bespoke agent RPC is exposed. | Private RPC may be useful between local processes, but exposing it to agents creates an unnecessary compatibility surface. |
| D11 | Repository sources are validated against revision or content hash when possible. | A decision copied from an older revision must not be treated as current merely because it remains in the database. |
| D12 | Tool output classifies context as constraint, preference, or informational history. | The agent must not treat every retrieved sentence as an instruction. |

# 6. System architecture

## 6.1 System context

```text
+----------------------+     +----------------------+     +----------------------+
| Codex / IDE agent    |     | Claude Code         |     | Copilot / other MCP  |
+----------+-----------+     +----------+-----------+     +----------+-----------+
           \                         |                            /
            \                        | MCP tools                 /
             +-----------------------+---------------------------+
                                     |
                          +----------v-----------+
                          | MCP gateway          |
                          | stdio by default     |
                          | HTTP when enabled    |
                          +----------+-----------+
                                     |
                          +----------v-----------+
                          | Context service core |
                          +----+-----+-----+------+
                               |     |     |
                 +-------------+     |     +----------------+
                 |                   |                      |
        +--------v--------+  +-------v--------+    +--------v--------+
        | Policy/scope    |  | Retrieval and |    | Conflict,       |
        | authorization  |  | context packer|    | freshness,      |
        +--------+--------+  +-------+--------+    | provenance      |
                 |                   |             +--------+--------+
                 +-------------------+----------------------+
                                     |
                          +----------v-----------+
                          | SQLite + FTS index   |
                          +-----+-----------+----+
                                |           |
                    +-----------v--+     +--v----------------+
                    | Curated       |     | Source registry   |
                    | records       |     | docs, chats, Git  |
                    +--------------+     +-------------------+

                +-------------------+
                | contextctl / UI   |
                | review and writes |
                +---------+---------+
                          |
                          +----------> Context service core
```

## 6.2 Components

### MCP gateway

The gateway advertises the tool set, validates JSON Schema inputs, translates tool calls into domain requests, applies result-size limits, and returns schema-conformant structured results. It contains no ranking policy beyond safe request validation.

### Context service core

The core implements the use cases independently of MCP. It owns transaction boundaries, record lifecycle, retrieval orchestration, conflict analysis, context packing, and audit events.

### Scope and policy engine

This component derives the effective user and client identity from trusted configuration or authentication, canonicalizes repository identity, applies path and task filters, and removes unauthorized candidates before retrieval.

### Retrieval engine

The retrieval engine performs structured filtering, exact/FTS matching, optional semantic ranking, deduplication, and grouping. It does not decide authority by similarity score.

### Conflict and freshness analyzer

This component identifies supersession chains, contradictory active records, expired assertions, source revision mismatches, and evidence that can no longer be validated.

### Context packer

The packer selects the highest-value records under the requested budget. It prioritizes applicable constraints and decisions, then preferences, open questions, and concise historical background.

### Record store and indexes

SQLite stores normalized records, sources, scopes, relationships, proposals, and audit metadata. FTS5 indexes statements, rationale, tags, and evidence excerpts. An optional vector table may be added later without changing the public API.

### Administrative interface

`contextctl` is the initial user-controlled write surface. A local web or desktop UI may later provide review queues, evidence inspection, conflict resolution, and editing.

## 6.3 Deployment model

### MVP deployment

The MVP is a single executable with distinct commands:

```text
context mcp --stdio        # MCP server launched by a local agent host
context search ...         # human-facing diagnostic query
context review             # review pending proposals
context import ...         # ingest or index source material
context export ...         # portable backup/export
```

Each MCP process opens the same local SQLite database. Agent-facing processes use read-only database access unless the proposal tool is explicitly enabled. SQLite WAL mode permits concurrent readers and controlled administrative writes.

This is centralized at the data, policy, and schema layer without requiring a permanently running daemon.

### Optional daemon deployment

A later `context daemon` mode may be introduced when one or more of these requirements become material:

- centralized background indexing;
- long-lived caches or local embedding models;
- stronger per-client authentication;
- many concurrent agent processes;
- remote MCP over Streamable HTTP; or
- event subscriptions for an administrative UI.

In daemon mode, a stdio shim remains available for hosts that expect a client-launched subprocess:

```text
Agent host -> MCP stdio shim -> private local RPC -> context daemon
```

The private RPC should use a Unix-domain socket on Unix-like systems and a named pipe or loopback socket with equivalent access controls on Windows. ConnectRPC, gRPC, or a narrow HTTP/JSON protocol are acceptable internal transports because they do not affect agents. The implementation should choose the smallest option justified by actual daemon requirements.

## 6.4 Cloud-agent boundary

A local-only process is not reachable from a remote agent sandbox. Cloud integration is therefore a separate deployment concern, not a reason to weaken the local design. Supported future options are:

- an authenticated, minimized remote MCP endpoint;
- a user-initiated secure tunnel to the local service; or
- a signed, task-specific context capsule exported into the cloud workspace.

The MVP excludes general remote access. Context-capsule export is the preferred first cloud mechanism because it makes disclosure explicit and reviewable.

# 7. Agent-facing MCP interface

## 7.1 Protocol selection

MCP is the public compatibility layer. The current MCP specification defines JSON-RPC-based messages, schema-described tools, stdio and Streamable HTTP transports, and structured tool output [1][2][3]. Codex and Claude Code support connecting to MCP servers, and GitHub Copilot exposes MCP integration across its agent surfaces [6][7][8].

The implementation should target MCP specification version `2026-07-28` through the official Go SDK while retaining compatibility with earlier negotiated protocol versions supported by the SDK [4][5]. The domain behavior described here does not rely on connection-local state.

## 7.2 Why tools, rather than resources or prompts

Tools are the required interface because the model can discover and invoke them in response to a task. Prompts are generally user-triggered templates, and resources depend more heavily on host-controlled retrieval behavior. In addition, some GitHub cloud-agent surfaces currently support MCP tools but not MCP resources or prompts [8].

Resources may later expose user-visible records or evidence in clients that support them, but no core workflow depends on that feature.

## 7.3 General tool contract

Every tool follows these rules:

- The authenticated user and client identity are derived by the server, not accepted as ordinary tool arguments.
- Workspace and task scope are explicit.
- Current user instructions and current repository files override returned context.
- Results distinguish constraints, preferences, decisions, and informational history.
- Results include authority, status, scope, freshness, and source references.
- Read operations are idempotent and have no open-world side effects.
- Result size is bounded by caller budget and server limits.
- Partial results are returned with warnings when nonessential evidence cannot be validated.
- Tool descriptions explicitly prohibit broad enumeration of unrelated personal information.

## 7.4 Tool inventory

| Tool | Purpose | Default availability |
|---|---|---|
| `context.prepare_task` | Build a context pack for a concrete coding task and workspace. | Enabled |
| `context.search` | Search within an authorized scope for relevant decisions, preferences, rationale, or history. | Enabled |
| `context.get_records` | Retrieve full details and bounded evidence for record IDs already discovered. | Enabled |
| `context.propose_record` | Create a pending proposal for user review; never activates a record. | Disabled unless explicitly configured |

The server should return tools in deterministic order and keep names stable.

## 7.5 `context.prepare_task`

### Intended use

This is the normal first call before planning or executing a nontrivial coding task. It should also be called when the task materially changes.

### Suggested tool description

> Prepare personal and project context relevant to a concrete coding task. Call this before planning or executing a nontrivial task. Returns scoped preferences, prior decisions, constraints, relevant history, conflicts, and stale-record warnings. Current user instructions and current repository files override all returned context. Do not use this tool to retrieve unrelated personal information.

### Input

```json
{
  "task": "Review the XMLDSig implementation for production readiness",
  "workspace": {
    "root_uri": "file:///work/helium",
    "repository": "https://github.com/lestrrat-go/helium",
    "revision": "8f3b2c1",
    "branch": "main",
    "relevant_paths": ["xmldsig1/", "xmlenc/"]
  },
  "task_kind": "code_review",
  "token_budget": 3000,
  "include_evidence_summaries": true
}
```

`task` is required. `workspace` is strongly recommended and may be completed from MCP roots or trusted process configuration where the client provides them. `repository` and `revision` should be canonicalized and verified against the workspace when local Git metadata is accessible.

### Output

```json
{
  "context_id": "ctx_01J...",
  "generated_at": "2026-09-03T00:15:00Z",
  "expires_at": "2026-09-03T01:15:00Z",
  "scope": {
    "repository": "https://github.com/lestrrat-go/helium",
    "revision": "8f3b2c1",
    "paths": ["xmldsig1/", "xmlenc/"]
  },
  "summary": "Implementation and conformance risks should be prioritized over pre-v1 API stability.",
  "items": [
    {
      "record_id": "rec_review_preference_001",
      "kind": "preference",
      "effect": "preference",
      "statement": "For pre-v1 production-readiness reviews, weight implementation correctness more heavily than API compatibility.",
      "authority": "explicit_user",
      "status": "active",
      "scope_specificity": "repository_and_task",
      "last_verified_at": "2026-07-24T00:00:00Z",
      "source_refs": ["src_conversation_2026_07_24"]
    }
  ],
  "conflicts": [],
  "warnings": [
    {
      "code": "historical_revision_mismatch",
      "message": "A prior review refers to v0.7.0 rather than the supplied revision."
    }
  ],
  "budget": {
    "requested_tokens": 3000,
    "estimated_tokens_used": 720,
    "truncated": false,
    "omitted_item_count": 2
  }
}
```

### Context handles

`context_id` is an optional optimization for follow-up calls. It identifies a cached, authorized task scope and expires quickly. It is not an authorization credential. The service rechecks the caller and scope on every use.

## 7.6 `context.search`

### Intended use

This tool supports a targeted follow-up after task preparation, for example:

- Why was the conformance-test repository pinned to a controlled commit?
- Has this implementation alternative already been rejected?
- What did the user previously say about untrusted stylesheets?
- Which records discuss this protocol representation?

### Suggested tool description

> Search decisions, preferences, rationale, rejected alternatives, and project history within an authorized task or repository scope. Use for targeted follow-up questions after `context.prepare_task`. This tool is not an unrestricted personal-data search and will not enumerate unrelated records.

### Input

```json
{
  "context_id": "ctx_01J...",
  "query": "Why is the conformance suite run against a controlled commit?",
  "kinds": ["decision", "rejected_alternative", "historical_event"],
  "include_stale": true,
  "limit": 8
}
```

A caller may provide an explicit workspace instead of `context_id`. The service must reject a request that provides neither an authorized handle nor sufficient scope.

### Output

The result contains ranked record summaries, match reasons, authority, lifecycle state, and source IDs. Search scores are diagnostic only and never replace authority or precedence.

## 7.7 `context.get_records`

### Intended use

This tool retrieves complete records and evidence after the agent has identified relevant record IDs. It is batch-oriented to avoid one tool call per record.

### Suggested tool description

> Retrieve full details and bounded supporting evidence for record IDs previously returned by this server. Does not perform broad search. Evidence may be omitted or redacted according to client policy and sensitivity.

### Input

```json
{
  "record_ids": [
    "rec_review_preference_001",
    "rec_test_decision_004"
  ],
  "include_evidence": true,
  "max_evidence_characters_per_record": 2000
}
```

### Output

The output includes the complete record, supersession relationships, validation status, and source excerpts or source locators. Evidence is bounded and may be replaced by a locator when disclosure is not permitted.

## 7.8 `context.propose_record`

### Intended use

This optional tool allows an agent to state that a new preference, decision, rejected alternative, or project fact may be worth preserving. It does not modify active records.

### Suggested tool description

> Propose a new or superseding context record for explicit user review. The proposal remains inactive and cannot override existing context. Use only when the current interaction contains clear supporting evidence.

### Input

```json
{
  "proposal_key": "session-123:decision:controlled-test-commit",
  "kind": "decision",
  "statement": "The release process intentionally executes the conformance suite against a controlled commit.",
  "rationale": "The user confirmed that reproducibility is achieved by selecting a definite commit before release.",
  "scope": {
    "repository": "https://github.com/lestrrat-go/helium"
  },
  "evidence": [
    {
      "type": "current_interaction",
      "locator": "host-provided-turn-reference"
    }
  ],
  "supersedes_record_ids": []
}
```

### Output

```json
{
  "proposal_id": "prop_01J...",
  "status": "pending_review",
  "created": true
}
```

`proposal_key` provides idempotency. Repeating the same request returns the existing proposal rather than creating duplicates.

## 7.9 Tool annotations

The three read tools should advertise:

```json
{
  "readOnlyHint": true,
  "destructiveHint": false,
  "idempotentHint": true,
  "openWorldHint": false
}
```

`context.propose_record` should advertise `readOnlyHint: false`, `destructiveHint: false`, and `openWorldHint: false`. With a required idempotency key, it may also advertise `idempotentHint: true`. MCP defines these annotations as hints rather than security controls [2].

## 7.10 Structured output and text fallback

Each tool defines an `outputSchema` and returns the complete machine-readable payload in `structuredContent`. It also returns a concise `content` text summary for clients that do not make full use of structured output. MCP permits output schemas and requires conforming structured results when one is declared [2].

The text fallback should summarize the number and categories of records, major warnings, and whether truncation occurred. It should not duplicate every record verbatim.

## 7.11 Error model

Protocol and malformed-request failures use ordinary MCP/JSON-RPC errors. Domain-level failures return a tool execution error with a stable code and actionable message.

| Code | Meaning | Retry guidance |
|---|---|---|
| `invalid_scope` | Required workspace or task scope is missing or malformed. | Supply repository/workspace details. |
| `unauthorized_scope` | The client is not permitted to query the requested scope. | Do not retry with broader scope. |
| `ambiguous_repository` | The repository identity cannot be canonicalized safely. | Supply canonical URL and revision. |
| `context_expired` | The supplied context handle expired. | Call `context.prepare_task` again. |
| `record_not_found` | A requested record is absent or inaccessible. | Remove the ID or repeat scoped search. |
| `source_unavailable` | Required evidence could not be loaded. | Retry later or continue with warning. |
| `budget_too_small` | The requested budget cannot fit mandatory metadata. | Increase the budget. |
| `proposal_disabled` | The write/proposal tool is not enabled for this client. | Use the administrative interface. |

Conflicts, stale records, and partial source validation are usually warnings in a successful result, not errors.

## 7.12 Agent instruction

Availability does not guarantee invocation. Each agent environment should include a global or repository-level instruction equivalent to:

> Before planning or executing a nontrivial coding task, call `context.prepare_task` with the task and current workspace. Use `context.search` only for targeted follow-up. Treat current user instructions and current repository documents as higher priority than returned context. Do not infer that informational history is an instruction.

Where the host supports hooks or wrappers, the integration may call `context.prepare_task` automatically and inject the resulting context pack before planning.

# 8. Domain and storage model

## 8.1 Record kinds

| Kind | Meaning | Typical durability |
|---|---|---|
| `constraint` | A requirement that applies in a defined scope. | Durable while source remains current |
| `preference` | A user preference, often conditional rather than universal. | Durable, reviewable |
| `decision` | A selected approach with rationale and optional rejected alternatives. | Durable with supersession history |
| `rejected_alternative` | An approach considered and rejected, with reason and conditions. | Durable historical context |
| `historical_event` | Something that happened at a specific time or revision. | Immutable history |
| `project_fact` | A sourced fact about project workflow, architecture, or state. | Requires freshness validation |
| `observation` | A lower-authority pattern inferred from behavior. | Expiring and visible |
| `open_question` | An unresolved issue that should not be represented as a decision. | Active until resolved or closed |
| `artifact_reference` | A pointer to an ADR, issue, review, chat, commit, or document. | Durable locator, source-dependent |

## 8.2 Canonical record shape

```yaml
id: rec_test_decision_004
kind: decision
subject: release conformance testing
statement: >
  The project intentionally runs the conformance-test repository against
  a controlled, definite commit before a release.
rationale: >
  Reproducibility is achieved by selecting the commit as part of the release
  process. Automatically following upstream is not intended.

scope:
  user: local-user
  repository: https://github.com/lestrrat-go/helium
  branch_patterns: ["*"]
  path_patterns: []
  task_kinds: [release_review, production_readiness_review]

authority: explicit_user
status: active
confidence: 1.0
sensitivity: project

valid_from: 2026-07-03T00:00:00Z
valid_until: null
review_after: null
last_verified_at: 2026-07-03T00:00:00Z
validation_policy: evidence_exists

supersedes: []
superseded_by: null
conflict_group: null

tags: [conformance, release, testing]

sources:
  - source_id: src_conversation_2026_07_03
    type: conversation
    locator: host-specific-reference
    revision: null
    content_hash: sha256:...
    exact_excerpt: >
      The test repository is executed before releases against a definite
      commit controlled by the project.
```

## 8.3 Scope model

Scopes are conjunctive unless explicitly represented as alternatives. A record may be:

- user-wide;
- organization-specific;
- repository-specific;
- branch- or release-specific;
- path-specific;
- task-kind-specific; or
- valid only when a structured condition is met.

Personal preferences should not automatically be universal. For example:

```yaml
scope:
  user: local-user
  conditions:
    repository_type: library
    language: go
    task_kind: production_change
```

The system should prefer no match over applying a vaguely related global preference.

Repository identity must be canonicalized. Aliases such as SSH and HTTPS Git remotes should resolve to one repository identity, while forks remain distinguishable unless the user explicitly links them.

## 8.4 Authority and precedence

Retrieval relevance and authority are independent axes. The default precedence order is:

1. current user instruction in the active interaction;
2. current checked-in repository instructions and ADRs;
3. current task-, branch-, or release-specific authoritative material;
4. active explicit user or project records;
5. sourced historical records and imported observations;
6. agent-generated inference.

The broker does not receive the complete active interaction in every host, so it cannot enforce item 1 by itself. Tool output and integration instructions must remind the agent of this precedence.

Suggested stored authority values are:

```text
repository_authoritative
explicit_user
explicit_project
sourced_import
observed_behavior
agent_inferred
unverified_import
```

## 8.5 Lifecycle state

```text
proposed -> active -> superseded -> archived
             |           |
             v           v
          disputed      restored (new record)
             |
             v
            stale
```

A superseded record is not edited to look as if the old decision never existed. A new record references what it supersedes. Historical queries can reconstruct the decision path.

`stale` means the record may have been true but failed freshness validation or passed its review date. `disputed` means credible current sources disagree and no authoritative resolution has been recorded.

## 8.6 Provenance model

Each source includes:

- source type;
- stable locator;
- source revision, commit, message ID, or timestamp where available;
- content hash;
- exact supporting excerpt or a bounded extracted span;
- capture time;
- access policy; and
- validation policy.

A record may summarize multiple sources. The exact excerpt exists to let the user and agent distinguish the original statement from the broker's normalized wording.

## 8.7 Relational schema

The initial SQLite schema should contain at least:

```text
records
record_scopes
sources
record_sources
record_relationships
proposals
proposal_sources
audit_events
schema_migrations
records_fts
```

`records` contains the normalized assertion and lifecycle fields. Scope dimensions are normalized sufficiently to support pre-search filtering. `record_relationships` represents supersession, support, contradiction, and derivation. `records_fts` indexes only data that the current database policy permits to be searched.

Sensitive evidence may be stored outside the primary database in encrypted files referenced by source ID. The MVP may instead rely on operating-system disk encryption and strict filesystem permissions, but the schema must permit later separation.

## 8.8 Instruction safety

Imported text may contain prompt-like instructions. The broker must never treat arbitrary source text as executable instruction merely because it was retrieved. Only normalized records with an appropriate kind and authority can appear as constraints or preferences.

Evidence excerpts are clearly labeled as quotations or source material. The output schema separates:

```text
statement            normalized broker assertion
rationale            normalized rationale
source_excerpt       untrusted evidence text
```

The agent integration should instruct the model that `source_excerpt` is data, not a command.

# 9. Retrieval and context-packing design

## 9.1 Pipeline

```text
1. Validate request and derive trusted client identity
2. Canonicalize task and workspace
3. Compute permitted scopes and sensitivity ceiling
4. Select mandatory scoped records
5. Retrieve lexical/FTS candidates
6. Optionally retrieve semantic candidates
7. Remove inactive or unauthorized candidates
8. Score applicability, authority, freshness, and relevance
9. Detect conflicts, supersession, and revision mismatch
10. Deduplicate and group related records
11. Pack records into the requested budget
12. Emit structured result, warnings, and audit event
```

## 9.2 Candidate selection

Candidate generation begins with structured filters:

- principal and client policy;
- repository identity;
- branch/revision applicability;
- relevant path patterns;
- task kind;
- lifecycle state;
- sensitivity; and
- validity interval.

FTS search then operates only over permitted rows. Query expansion may add repository aliases, package names, symbols, and known topic tags. Semantic retrieval, when enabled, runs over the same prefiltered candidate set or over an equivalently partitioned index.

## 9.3 Ranking

The initial ranker should be deterministic and inspectable. Suggested components are:

- task/query textual relevance;
- scope specificity;
- authority tier;
- freshness and successful source validation;
- record-kind priority for the current operation;
- explicit path or symbol match;
- conflict or supersession penalties; and
- redundancy penalty.

A possible initial scoring model is:

```text
score = relevance
      + scope_specificity_bonus
      + authority_bonus
      + freshness_bonus
      + exact_identifier_bonus
      - stale_penalty
      - superseded_penalty
      - redundancy_penalty
```

The numerical weights are configuration and evaluation details, not part of the public contract. The output may include human-readable match reasons rather than exposing a misleading universal score.

## 9.4 Conflict detection

Two records are candidates for conflict when they:

- apply to overlapping scopes;
- concern the same normalized subject;
- have incompatible predicates or decisions; and
- are both active or one is treated as current despite missing supersession metadata.

The service should not use an LLM alone to resolve conflicts. It can use a model to propose that two records conflict, but deterministic rules and explicit relationships govern the returned state.

A conflict result contains both records, authority, dates, sources, and a recommended handling such as:

```text
prefer_newer_authoritative_source
ask_user_if_material
ignore_historical_record
repository_source_requires_revalidation
```

## 9.5 Freshness validation

Validation policies include:

- `none`: suitable for immutable historical events;
- `evidence_exists`: confirm the source still exists;
- `content_hash_matches`: confirm referenced content is unchanged;
- `git_revision_ancestor`: confirm the source revision is applicable to the current revision;
- `file_path_and_hash`: validate a checked-in instruction or ADR;
- `review_after`: mark stale after a date unless reapproved; and
- `manual`: require explicit user review.

Validation should be lazy for expensive sources and cached with a bounded TTL. A failed optional validation produces a warning. A failed validation for a high-impact project fact removes it from active constraints.

## 9.6 Context packing

The packer uses the requested token budget as an approximate limit. Because the server does not necessarily know the host model's exact tokenizer, it uses a conservative character-based estimate and reports that the value is approximate.

Packing order is:

1. security or correctness constraints directly applicable to the task;
2. active decisions and rejected alternatives that prevent repeated work;
3. strong explicit preferences;
4. open questions and conflicts;
5. concise relevant history;
6. lower-authority observations.

Each item is represented once. Evidence excerpts are normally omitted from `prepare_task` and fetched through `get_records` when needed. This preserves budget and reduces accidental disclosure.

## 9.7 Determinism and caching

For the same authenticated client, database version, task, workspace, and configuration, read results should be stable. Stable ordering improves testability and agent behavior.

A context-pack cache key may include:

```text
principal policy version
client policy version
repository canonical ID
revision
relevant paths
task hash
task kind
requested budget
database content version
ranker version
```

Cached packs never bypass authorization or freshness checks. Context handles point to cache entries but are not bearer credentials.

# 10. Security and privacy

## 10.1 Threat model

The system handles personal preferences and cross-project history, so its risk is not limited to ordinary code search. Principal threats include:

- a compromised or over-permissioned agent enumerating personal data;
- accidental disclosure of one project's context to another repository;
- prompt injection embedded in imported evidence;
- an agent writing false preferences that reinforce future behavior;
- forged repository identity or path scope;
- replay of another client's context handle;
- remote exposure of a local HTTP endpoint;
- logs or telemetry containing sensitive record text; and
- stale records causing incorrect security or architecture decisions.

## 10.2 Authorization model

Authorization is capability- and scope-based. Example capabilities are:

```text
context:prepare
context:search:project
context:search:personal
context:evidence:project
context:evidence:personal
context:propose
context:admin
```

A local coding agent commonly receives project-scoped search and limited personal preferences, but not unrestricted personal evidence. Tool availability may vary by authenticated capability.

Filtering occurs before retrieval. Unauthorized rows are excluded before FTS, embeddings, snippets, counts, or result grouping.

## 10.3 Local stdio security

For a stdio server launched by a trusted host:

- the executable and configuration files are owned by the user;
- the database directory uses restrictive filesystem permissions;
- stdout is reserved exclusively for MCP messages;
- logs are written to stderr or a protected file;
- environment variables may identify the configured client profile, but must not contain long-lived secrets when avoidable; and
- the server refuses workspace paths outside configured roots unless explicitly allowed.

The MCP build guidance specifically warns that stdout logging corrupts stdio protocol messages [9].

## 10.4 Daemon and HTTP security

A private daemon socket uses owner-only permissions and per-client credentials when client distinction matters. A loopback TCP endpoint still requires authentication because browser processes, containers, or other local users may reach it.

Remote Streamable HTTP requires TLS and the MCP authorization model based on OAuth conventions [3][10]. The service must bind to loopback by default and require an explicit configuration change to listen on a non-loopback address.

## 10.5 Data minimization

The broker stores only context useful to supported agent workflows. It should avoid full conversation retention when a normalized record and bounded evidence span are sufficient.

Sensitivity levels are:

```text
public
project
personal
restricted
```

A client profile defines the maximum sensitivity and permitted projects. The service does not return personal-family, financial, employment, or unrelated project context merely because it belongs to the same user.

## 10.6 Prompt-injection resistance

Mitigations include:

- treat imported source text as untrusted data;
- never derive authority from wording intensity;
- normalize records through typed fields;
- preserve source excerpts separately from active statements;
- prevent evidence text from modifying tool policy;
- escape or label content that resembles tool instructions;
- require user approval before promoting an inferred instruction; and
- add adversarial tests containing malicious instructions in chat and repository documents.

## 10.7 Write safety

The agent-facing proposal tool is additive and non-authoritative. Direct activate, edit, delete, and supersede operations are not exposed to ordinary coding agents.

An approved record stores who approved it, when, and what changed from the proposal. Rejections are retained long enough to prevent repeated identical proposals, subject to retention policy.

## 10.8 Audit

Each call produces a local audit event containing:

- timestamp;
- client profile and authenticated principal;
- tool name;
- normalized scope;
- record IDs returned;
- sensitivity classes returned;
- truncation and warning codes;
- latency and result size; and
- proposal ID for writes.

Audit logs should avoid storing complete task text or record statements by default. A diagnostic mode may include more detail with explicit user consent.

# 11. Ingestion and curation

## 11.1 Manual-first strategy

The MVP begins with manually curated, high-value records. This avoids building a large ingestion pipeline before the authority and lifecycle model is proven.

Recommended seed categories are:

- persistent review preferences;
- recurring repository-specific constraints;
- decisions with rationale;
- rejected alternatives likely to be revisited;
- release and conformance workflows;
- important historical reviews; and
- open questions that remain unresolved.

A corpus of 50 to 100 high-value records is sufficient for initial evaluation.

## 11.2 Source adapters

Later adapters may index:

- local Markdown or YAML records;
- Git repositories and ADR directories;
- GitHub issues and pull requests;
- chat exports;
- email or notes;
- agent-specific memory exports; and
- manually selected files.

Adapters produce source objects and candidate proposals. They do not bypass review or assign `explicit_user` authority automatically.

## 11.3 Distillation workflow

```text
Source import
    -> extract candidate statements
    -> classify kind and suggested scope
    -> attach exact evidence
    -> detect duplicates/conflicts
    -> create pending proposals
    -> user approve/edit/reject
    -> activate normalized records
```

Model-assisted extraction is acceptable because its output is a proposal. The system must show the source excerpt and proposed normalized statement side by side during review.

## 11.4 Deduplication

Deduplication uses exact hashes, normalized subject, scope overlap, lexical similarity, and optional semantic similarity. Similar records are grouped for review rather than silently merged when their rationale, authority, or conditions differ.

## 11.5 Correction and forgetting

The user can:

- edit an active record by creating a new revision;
- supersede a record while preserving history;
- mark a record disputed or stale;
- delete evidence or the complete record;
- block future proposals derived from a rejected source pattern; and
- export all records in a portable format.

Deletion must remove the record from FTS and vector indexes, caches, and future context packs. Backup retention is documented separately so “delete” has a clear operational meaning.

# 12. Operational design

## 12.1 Suggested filesystem layout

```text
~/.config/agent-context-broker/config.toml
~/.local/share/agent-context-broker/context.db
~/.local/share/agent-context-broker/evidence/
~/.local/state/agent-context-broker/audit.jsonl
~/.cache/agent-context-broker/
```

Platform-specific equivalents should be used on Windows and macOS.

## 12.2 Configuration

Configuration includes:

- database and evidence paths;
- client profiles and capabilities;
- allowed workspace roots;
- repository aliases;
- maximum sensitivity by client;
- output and evidence limits;
- validation TTLs;
- optional semantic-retrieval provider;
- proposal-tool enablement; and
- audit detail level.

Configuration validation fails closed. An unknown client profile receives the minimum configured read scope or no tools.

## 12.3 Backup and portability

The service supports:

- transactional SQLite backup;
- deterministic JSONL export of records and relationships;
- Markdown/YAML export for human review;
- separate evidence export according to sensitivity; and
- schema-version metadata.

The portable format is part of user ownership. It should not depend on internal FTS or embedding representation.

## 12.4 Observability

Local metrics include:

- request count and latency by tool;
- candidate and returned record counts;
- truncation rate;
- stale/conflict warning counts;
- cache hit rate;
- proposal approval/rejection rate;
- retrieval precision evaluation results; and
- source-validation failures.

No external telemetry is enabled by default.

## 12.5 Performance targets

For a local database containing up to 100,000 normalized records:

- warm `prepare_task` p95 under 300 ms without remote source validation;
- warm targeted search p95 under 200 ms;
- startup under 1 second on a normal development workstation;
- default context pack under 3,000 approximate tokens; and
- hard server-side response limit independent of caller input.

These are design targets to validate, not protocol guarantees.

## 12.6 Schema migration and compatibility

Database migrations are explicit, ordered, and reversible where practical. Before a destructive migration, the tool creates a transactional backup.

The MCP tool names and core output fields follow additive compatibility rules. New optional fields may be added. Removing or changing field meaning requires a new tool or explicit API version.

# 13. End-to-end workflows

## 13.1 Task preparation and follow-up

```text
User asks agent to review a repository
              |
              v
Agent resolves current workspace and revision
              |
              v
Agent calls context.prepare_task
              |
              v
Broker authorizes scope and returns a bounded context pack
              |
              v
Agent plans review using active decisions and preferences
              |
              +---- needs rationale? ----> context.search
              |                                  |
              |                                  v
              |                         context.get_records
              |                                  |
              +----------------------------------+
              |
              v
Agent performs task and reports findings
```

## 13.2 Example: avoiding a repeated invalid review concern

1. The agent receives a production-readiness review task for a current repository revision.
2. `context.prepare_task` returns an active decision explaining that a test repository is intentionally pinned to a controlled commit for release reproducibility.
3. The context pack also reports that an older review criticized the arrangement before this rationale was recorded.
4. The agent verifies the current release workflow rather than repeating the old criticism as an unresolved defect.
5. If current code contradicts the stored decision, the agent reports a source mismatch rather than blindly obeying memory.

## 13.3 Proposal flow

```text
Agent identifies a potentially durable decision
              |
              v
Agent calls context.propose_record with exact evidence
              |
              v
Proposal enters pending review; active context is unchanged
              |
              v
User reviews statement, rationale, scope, and evidence
       +------+-------+
       |              |
    approve        edit/reject
       |              |
       v              v
new active record   retained review outcome
```

# 14. Testing and evaluation

## 14.1 Unit testing

Unit tests cover:

- scope intersection and repository aliasing;
- capability and sensitivity filtering;
- precedence and authority ordering;
- lifecycle transitions and supersession;
- freshness policies;
- conflict detection;
- context-budget packing;
- deterministic ordering;
- proposal idempotency; and
- deletion from all indexes and caches.

Property tests should assert that no output record violates the effective authorization predicate.

## 14.2 MCP integration testing

The MCP gateway is tested against the official SDK and conformance tooling where applicable. Tests include:

- tool listing and deterministic order;
- input and output schema validation;
- structured-result conformance;
- text fallback;
- cancellation;
- stdio framing with no stdout log corruption;
- negotiated legacy/current protocol behavior; and
- Streamable HTTP authorization when enabled.

## 14.3 Security testing

Adversarial cases include:

- a task asking to list all personal memories;
- a public repository trying to retrieve a private project's records;
- forged repository metadata inconsistent with the local Git workspace;
- a malicious evidence excerpt instructing the agent to exfiltrate data;
- replay of another client profile's context handle;
- path traversal and symlink escape;
- very large schemas, tasks, evidence, and result limits;
- repeated proposal creation; and
- local HTTP access without authentication.

## 14.4 Retrieval evaluation

Create a fixed evaluation corpus of real tasks. For each task, identify:

- records that must be returned;
- records that may be returned;
- records that must not be returned;
- expected conflicts or stale warnings; and
- an expected approximate budget.

Measure precision at K, mandatory-record recall, unauthorized-result rate, stale-authority error rate, and context tokens per useful record. Human evaluation should additionally assess whether the agent's final work required fewer corrections.

## 14.5 Acceptance criteria

The MVP release requires:

- zero unauthorized records across the security evaluation corpus;
- all mandatory records returned for at least 90% of the initial task corpus;
- no superseded record presented as active guidance;
- schema-valid output from every successful tool call;
- successful integration with at least two independent coding-agent hosts;
- complete local export and restore; and
- a usable review flow for proposals and conflicts.

# 15. Implementation plan

## Milestone 1: record store and administrative CLI

Implement schema migrations, record CRUD for the human interface, sources, scopes, lifecycle, JSONL export/import, FTS indexing, and basic diagnostics.

## Milestone 2: retrieval core

Implement canonical workspace identity, scope filtering, exact/FTS retrieval, deterministic ranking, supersession, conflict warnings, and budget packing. Build the fixed evaluation corpus concurrently.

## Milestone 3: MCP read interface

Implement `context.prepare_task`, `context.search`, and `context.get_records` using the official Go MCP SDK. Add stdio integration tests and host configuration examples.

## Milestone 4: proposal and review workflow

Add `context.propose_record`, idempotency, proposal review, approval audit, and source-side-by-side display in the CLI or a minimal local UI.

## Milestone 5: freshness validation and source adapters

Add Git revision/hash validation, Markdown/YAML source adapters, conversation import proposals, and scheduled stale-record review.

## Milestone 6: optional daemon and cloud bridge

Introduce a daemon only if measurements justify it. Add a private local RPC adapter, Streamable HTTP with authentication, or signed task-capsule export without changing the agent tool contract.

# 16. Alternatives considered

## 16.1 Bespoke RPC as the agent-facing interface

A custom gRPC, ConnectRPC, or HTTP API could model the domain precisely, but ordinary coding agents would not know how to discover or invoke it. Every host would require an adapter that re-exposes the calls as tools. This adds integration work without improving the agent contract.

**Decision:** reject as the public interface; permit only as private IPC behind MCP.

## 16.2 MCP resources as the primary interface

Resources are natural for browsing known documents, but task preparation requires model-invokable computation: scope resolution, ranking, conflict analysis, and bounded packing. Client support also varies.

**Decision:** tools first; resources may supplement evidence browsing later.

## 16.3 Prompt-only integration

A prompt or repository instruction can tell an agent what to do, but it cannot provide a common, dynamic, scoped retrieval service. Large static prompts also become stale and consume context on every task.

**Decision:** use instructions to require tool invocation, not to carry the knowledge base.

## 16.4 Repository files only

Repository instructions and ADRs remain essential, but personal preferences and cross-project history should not necessarily be committed to every repository. They also cannot conveniently represent private conversation evidence.

**Decision:** index and respect repository sources; do not rely on them exclusively.

## 16.5 Vector database with generic semantic search

A vector-only system makes exact identifiers, negation, authority, time, and scope difficult to enforce. Similarity does not answer whether a decision remains active.

**Decision:** structured filters and FTS first; embeddings are optional ranking support.

## 16.6 Full conversation archive RAG

Searching complete conversations can recover detail, but it returns raw, contradictory, and stale statements without a lifecycle model. It also creates a larger privacy surface.

**Decision:** retain bounded evidence and source locators; normalize high-value assertions into reviewed records.

## 16.7 Direct autonomous memory writes

Allowing agents to write active preferences and decisions creates feedback loops where an inference becomes “truth” through repetition.

**Decision:** proposals only, with explicit user activation.

## 16.8 Remote centralized SaaS first

A hosted service simplifies access from cloud agents but weakens privacy, offline operation, and user ownership before the knowledge model is proven.

**Decision:** local-first MVP; add deliberate remote disclosure mechanisms later.

# 17. Open questions

1. Which administrative format should be canonical: normalized SQLite with export, or human-readable Markdown/YAML files materialized into SQLite?
2. Should explicit personal preferences require periodic review, or remain active indefinitely until contradicted?
3. Which local embedding model, if any, provides enough retrieval benefit to justify its operational cost?
4. How should repository identity behave for forks, worktrees, vendored code, and monorepos?
5. Should context packs be signed so a cloud workspace can verify origin and detect modification?
6. What evidence types may be returned directly to agents versus only shown to the user?
7. Which host integrations can enforce `prepare_task` automatically through hooks rather than relying on instructions?
8. Should the service maintain separate profiles for review agents, implementation agents, and autonomous cloud agents?
9. What deletion guarantees should apply to backups and exported context capsules?
10. When multi-user support is introduced, is the correct abstraction a shared organization context layer above personal stores, or independent stores with federated retrieval?

# 18. Final recommendation

Implement the Agent Context Broker as a local-first, typed context service with a deliberately small MCP tool surface:

```text
context.prepare_task
context.search
context.get_records
context.propose_record   # optional and proposal-only
```

Use MCP over stdio for the MVP. Keep the core as transport-independent Go interfaces backed by SQLite and FTS5. Do not build a custom agent protocol, a vector-only memory store, or an autonomous write path.

The architecture should optimize for one property above all others: **the agent can understand not only what was remembered, but where it applies, why it is believed, whether it is still current, and how authoritative it is.**

That is the distinction between a useful context broker and a larger source of confident mistakes.

```{=openxml}
<w:p><w:r><w:br w:type="page"/></w:r></w:p>
```

# Appendix A. Suggested Go domain interface

The following interface is illustrative. It is not the MCP schema and should remain independent from transport-specific request types.

```go
package contextbroker

import (
    "context"
    "time"
)

type Service interface {
    PrepareTask(context.Context, PrepareTaskRequest) (ContextPack, error)
    Search(context.Context, SearchRequest) (SearchResult, error)
    GetRecords(context.Context, GetRecordsRequest) (RecordResult, error)
    ProposeRecord(context.Context, ProposeRecordRequest) (Proposal, error)
}

type Caller struct {
    PrincipalID  string
    ClientID     string
    Capabilities []string
}

type Workspace struct {
    RootURI       string
    Repository    string
    Revision      string
    Branch        string
    RelevantPaths []string
}

type PrepareTaskRequest struct {
    Caller                   Caller
    Task                     string
    TaskKind                 string
    Workspace                Workspace
    TokenBudget              int
    IncludeEvidenceSummaries bool
}

type ContextPack struct {
    ContextID  string
    Generated  time.Time
    Expires    time.Time
    Scope      EffectiveScope
    Summary    string
    Items      []ContextItem
    Conflicts  []Conflict
    Warnings   []Warning
    Budget     BudgetReport
}

type ContextItem struct {
    RecordID        string
    Kind            RecordKind
    Effect          Effect
    Statement       string
    Rationale       string
    Authority       Authority
    Status          RecordStatus
    ScopeSpecificity string
    LastVerified    *time.Time
    SourceRefs      []string
    MatchReasons    []string
}
```

The MCP adapter derives `Caller` from trusted connection configuration or authorization and never accepts it directly from ordinary tool arguments.

# Appendix B. MCP schema excerpts

## B.1 `context.prepare_task` input schema

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["task"],
  "properties": {
    "task": {
      "type": "string",
      "minLength": 1,
      "maxLength": 20000
    },
    "task_kind": {
      "type": "string",
      "enum": [
        "implementation",
        "code_review",
        "security_review",
        "design",
        "debugging",
        "release",
        "research",
        "other"
      ],
      "default": "other"
    },
    "workspace": {
      "$ref": "#/$defs/workspace"
    },
    "token_budget": {
      "type": "integer",
      "minimum": 256,
      "maximum": 12000,
      "default": 3000
    },
    "include_evidence_summaries": {
      "type": "boolean",
      "default": false
    }
  },
  "$defs": {
    "workspace": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "root_uri": {"type": "string", "format": "uri"},
        "repository": {"type": "string", "maxLength": 2048},
        "revision": {"type": "string", "maxLength": 256},
        "branch": {"type": "string", "maxLength": 512},
        "relevant_paths": {
          "type": "array",
          "maxItems": 128,
          "items": {"type": "string", "maxLength": 2048}
        }
      }
    }
  }
}
```

## B.2 Common output item

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": [
    "record_id",
    "kind",
    "effect",
    "statement",
    "authority",
    "status",
    "source_refs"
  ],
  "properties": {
    "record_id": {"type": "string"},
    "kind": {
      "type": "string",
      "enum": [
        "constraint",
        "preference",
        "decision",
        "rejected_alternative",
        "historical_event",
        "project_fact",
        "observation",
        "open_question",
        "artifact_reference"
      ]
    },
    "effect": {
      "type": "string",
      "enum": ["constraint", "preference", "informational"]
    },
    "statement": {"type": "string"},
    "rationale": {"type": "string"},
    "authority": {"type": "string"},
    "status": {"type": "string"},
    "scope_specificity": {"type": "string"},
    "last_verified_at": {
      "type": ["string", "null"],
      "format": "date-time"
    },
    "source_refs": {
      "type": "array",
      "items": {"type": "string"}
    },
    "match_reasons": {
      "type": "array",
      "items": {"type": "string"}
    }
  }
}
```

# Appendix C. References

The following references were current when this draft was prepared on 3 September 2026.

1. [Model Context Protocol specification, version 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28)
2. [MCP server tools specification](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)
3. [MCP transport specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports)
4. [Official MCP SDKs](https://modelcontextprotocol.io/docs/2026-07-28/sdk)
5. [Official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
6. [OpenAI Codex MCP documentation](https://developers.openai.com/codex/mcp)
7. [Anthropic Claude Code MCP documentation](https://docs.anthropic.com/en/docs/claude-code/mcp)
8. [GitHub: Configure MCP servers for Copilot cloud agent and code review](https://docs.github.com/en/copilot/how-tos/copilot-on-github/customize-copilot/configure-mcp-servers)
9. [MCP: Build an MCP server](https://modelcontextprotocol.io/docs/2026-07-28/develop/build-server)
10. [MCP authorization guidance](https://modelcontextprotocol.io/docs/2026-07-28/tutorials/security/authorization)
