# Seed records

A starting set to adapt, not to import as-is: the repositories and rationale
here are examples. Rewrite them for your own work, then:

```sh
mecp import ./seed --dry-run   # see what would be created
mecp import ./seed
```

Records imported this way get `sourced_import` authority. Promote the ones you
actually authored:

```sh
mecp record list --json | jq -r '.[].id'
```

then re-add them with `--authority explicit_user`, or edit the YAML to declare
it before importing.

## What is worth seeding

The categories that repay curation first, from the design document:

- persistent review preferences;
- recurring repository-specific constraints;
- decisions with their rationale;
- rejected alternatives you expect to be proposed again;
- release and conformance workflows;
- important historical reviews; and
- open questions that are still open.

Fifty to a hundred high-value records is enough to evaluate whether the broker
is earning its place. Scope each one as narrowly as is actually true — a
preference that only holds for pre-v1 library reviews should say so, or it will
turn up during unrelated work.
