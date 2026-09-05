package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/lestrrat-go/rasql/dialect"
	"github.com/lestrrat-go/rasql/query"
	"github.com/lestrrat-go/rasql/render"
)

// sqliteDialect is the one dialect this package renders for. render.Select
// refuses MATCH for any other, which is what keeps the full-text statements in
// search.go from being built against a database that cannot answer them.
var sqliteDialect = dialect.SQLite()

// execer and queryer are the parts of *sql.DB and *sql.Tx these helpers need,
// so the same statement runs inside a transaction or outside one.
type execer interface {
	ExecContext(ctx context.Context, sql string, args ...any) (sql.Result, error)
}

type queryer interface {
	QueryContext(ctx context.Context, sql string, args ...any) (*sql.Rows, error)
}

// execWrite renders an insert, update, delete, or upsert and runs it. Every
// value it carries travels as a bound argument, so no caller-supplied text
// reaches the SQL.
func execWrite(ctx context.Context, ex execer, w query.WriteStatement) (sql.Result, error) {
	rendered, err := render.Write(sqliteDialect, w)
	if err != nil {
		return nil, fmt.Errorf(`failed to render statement: %w`, err)
	}
	return ex.ExecContext(ctx, rendered.SQL(), rendered.Args()...)
}

// querySelect renders a select and runs it. The caller closes the rows.
func querySelect(ctx context.Context, q queryer, s query.Select) (*sql.Rows, error) {
	rendered, err := render.Select(sqliteDialect, s)
	if err != nil {
		return nil, fmt.Errorf(`failed to render statement: %w`, err)
	}
	return q.QueryContext(ctx, rendered.SQL(), rendered.Args()...)
}

// deleteWhere builds a single-predicate delete, the shape most of this
// package's cleanup statements take.
func deleteWhere(table query.TableRef, where query.Expression) (query.Delete, error) {
	statement, err := query.NewDelete(table)
	if err != nil {
		return query.Delete{}, err
	}
	return statement.WithWhere(where)
}

// selectWhere builds a select over one table with an optional predicate.
func selectWhere(table query.TableRef, where query.Expression, projections ...query.Projection) (query.Select, error) {
	statement, err := query.NewSelect(table, projections...)
	if err != nil {
		return query.Select{}, err
	}
	if where == nil {
		return statement, nil
	}
	return statement.WithWhere(where)
}

// scanOne reads exactly one row into dest and closes the rows. It backs the
// aggregate reads that used to go through QueryRowContext, which the builder
// has no equivalent of: a rendered statement is always run through Query.
func scanOne(rows *sql.Rows, dest ...any) error {
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	return rows.Err()
}

// withPaging applies the caller's limit and offset. An offset without a limit
// is not valid SQLite, so an offset only takes effect alongside one, which is
// the rule the structured queries already followed.
func withPaging(s query.Select, limit, offset int) (query.Select, error) {
	if limit <= 0 {
		return s, nil
	}
	s, err := s.WithLimit(limit)
	if err != nil {
		return query.Select{}, err
	}
	if offset <= 0 {
		return s, nil
	}
	return s.WithOffset(offset)
}
