package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/lestrrat-go/rasql/query"
	"github.com/stretchr/testify/require"
)

// TestTableDefsMatchMigratedSchema is what keeps tables.go honest. The query
// builder resolves every column reference against those hand-written
// descriptions, so a migration that renames or drops a column would otherwise
// leave this package building statements the database cannot answer, and
// nothing would say so until one ran.
//
// The live side comes from PRAGMA table_info rather than from rasql's own
// catalog package, which would pull the rasql-sqlite SQL parser into mecp as a
// dependency for one test. The pragma answers the only question this test
// asks: which columns the migrated table actually has.
//
// It is an internal test because the descriptions are unexported, and there is
// no way to reach them from sqlite_test.
func TestTableDefsMatchMigratedSchema(t *testing.T) {
	store := newMigratedStore(t)

	// records_fts is included: FTS5 declares its columns to the pragma like
	// any other table, minus the hidden rowid the descriptions do not name.
	described := map[string]query.TableRef{
		"records":              recordsTable,
		"record_scopes":        recordScopesTable,
		"sources":              sourcesTable,
		"record_sources":       recordSourcesTable,
		"record_relationships": recordRelationshipsTable,
		"record_tags":          recordTagsTable,
		"proposals":            proposalsTable,
		"proposal_sources":     proposalSourcesTable,
		"audit_events":         auditEventsTable,
		"records_fts":          recordsFTSTable,
	}

	for name, ref := range described {
		t.Run(name, func(t *testing.T) {
			live, err := liveColumnNames(t.Context(), store.db, name)
			require.NoError(t, err)
			require.NotEmpty(t, live, "the migration did not create %s", name)

			want := make([]string, 0, len(ref.Definition().Columns))
			for _, column := range ref.Definition().Columns {
				want = append(want, column.Name)
			}
			require.Equal(t, live, want, "tables.go and the migration DDL disagree about %s", name)
		})
	}
}

// TestBM25WeightsCoverEveryIndexedColumn guards the one place a column list
// carries meaning beyond its names. bm25 takes one weight per indexed column
// in declaration order, so a column added to records_fts without a weight
// beside it would silently reweigh every column after it.
func TestBM25WeightsCoverEveryIndexedColumn(t *testing.T) {
	require.Len(t, bm25Weights, len(recordsFTSTable.Definition().Columns))
}

// liveColumnNames reads a table's columns out of the database, in the order it
// declares them.
func liveColumnNames(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	// The table name is not caller-supplied: every name reaches here from the
	// map above, and PRAGMA takes no bound arguments in any case.
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func newMigratedStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "context.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, store.Migrate(t.Context()))
	return store
}
