---
kind: constraint
subject: untrusted stylesheets
authority: repository_authoritative
sensitivity: project
scope:
  repository: https://github.com/example/your-project
  path_patterns: ["parser/", "xslt/"]
tags: [security, parsing]
validation_policy: file_path_and_hash
sources:
  - type: adr
    locator: file://docs/adr/0003-no-stylesheet-execution.md
---

Untrusted XSLT stylesheets must never be executed during parsing.

## Rationale

Stylesheet execution is a remote code execution primitive. The decision is
recorded in a checked-in ADR, so it carries repository authority and is
revalidated against that file's content hash.
