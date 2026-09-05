# rasql follow-ups to investigate

Open questions left over from moving the store's SQL onto rasql's query
builder. Neither item blocks anything: the store works as committed, and this
file exists so the findings behind them are not rediscovered from scratch.

The version in use is `github.com/lestrrat-go/rasql
v0.0.0-20260905014932-72db98c52453`, commit `72db98c`.

## Splitting rasql's `inspect` package per dialect

`inspect/inspect.go:24` imports `github.com/lestrrat-go/rasql-sqlite/query` at
package scope, and roughly forty call sites in that file use it. The import is
unconditional, so any caller of `inspect` or of `catalog`, which wraps it,
links a SQLite DDL parser even when it only ever inspects PostgreSQL or MySQL.

SQLite is the only dialect that needs a parser at all. It stores raw DDL text
in `sqlite_master`, so the inspector parses that text back into descriptors,
while PostgreSQL and MySQL answer the same questions from real catalog tables.

rasql already solves this problem one level over, in `migrate/diff`. That
package declares a dialect-neutral `Analyzer` interface in `diff.go` and puts
each dialect in its own sub-package, each importing only its own parser. Only
`cli/rasqlmigrate` imports all three, because a CLI genuinely needs them.
`inspect` is the one package that keeps every dialect's logic in one file.

The shape of a fix is to give `inspect` that same layout: `inspect/sqlite`,
`inspect/postgresql`, and `inspect/mysql` sub-packages, with `Queryer`,
`TableName`, the error types, and the shared metadata reads staying in
`inspect`. The open decision is `catalog.FromDatabase`, which picks its path at
run time from `Options.Dialect`. Either it takes the implementation as a
parameter, so `Options` grows an inspector field the caller builds from the
sub-package it chose, or the sub-packages register themselves in `init` and
callers blank-import the ones they want, as `database/sql` does for drivers.
The first keeps the choice compile-time checked and matches what `diff` already
does; the second leaves `Options` alone but turns a missing dialect into a
run-time error. Either way it breaks `inspect`'s current API.

### What this is worth

Little, for mecp. The cost measured rather than assumed: a scratch module
importing only `rasql/catalog` resolves exactly one extra require,
`github.com/lestrrat-go/rasql-sqlite`, which is 180 KB across six non-test Go
files and has no dependencies of its own. `rasql-pg` and `rasql-mysql` stay out
entirely, because nothing reaches them except `migrate/diff/postgresql` and
`migrate/diff/mysql`, which a SQLite-only program never imports.

So the argument is consistency with `migrate/diff`, plus not linking a parser
into a binary that cannot run it. It is not a build-weight argument, and mecp
is SQLite-only, so the one module it would ever pull is the one it could use.
This is worth raising upstream on its own merits rather than for anything mecp
needs.

## Reading the schema drift test through `catalog`

`sqlite/tables_test.go` checks the hand-written table descriptions in
`sqlite/tables.go` against the migrated database, because the query builder
resolves every column reference against those descriptions and a migration that
renamed a column would otherwise break statements at run time with nothing
saying so earlier.

It reads the live side with `PRAGMA table_info`. That choice was originally
made to avoid pulling parser modules in, on the mistaken belief that `catalog`
dragged in all three. It pulls one, and mecp wants that one anyway, so the
reason no longer holds.

`catalog` is the better source on the merits: it returns full `schema.TableDef`
values, so the test could assert column types, primary keys, and unique
constraints instead of only column names.

Two worries recorded here earlier were both wrong, checked by running
`catalog.FromDatabase` against an in-memory database holding mecp's own
`records_fts` DDL.

- The isolation level is not a problem. `FromDatabase` begins its read at
  `sql.LevelRepeatableRead`, and modernc accepts it silently, because
  `newTx` in `modernc.org/sqlite/tx.go` reads only `opts.ReadOnly` and ignores
  `opts.Isolation` entirely. `FromQueryer` is not needed.
- `records_fts` is reachable, but its column list differs. A sweep with an
  empty `Include` skips it, as inspection enumerates base tables only. Naming
  it in `Include` does describe it, and it comes back with eight columns rather
  than six: rasql's SQLite path reads `PRAGMA table_xinfo`, which exposes the
  hidden `records_fts` and `rank` columns that FTS5 adds, where
  `pragma_table_info` reports only the six declared ones.

What is left to decide is a real cost rather than a blocker. mecp's `go.mod`
does not require `rasql-sqlite` today, and `catalog` would appear only in
`sqlite/tables_test.go`, so switching adds a module the shipped binary never
links. Weigh that against the wider assertions the switch buys. Either way the
FTS5 table needs the pragma or a filter, since the descriptions in `tables.go`
name the six declared columns and not the two hidden ones.
