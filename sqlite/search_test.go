package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/sqlite"
	"github.com/stretchr/testify/require"
)

func TestQueryTerms(t *testing.T) {
	testcases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "auxiliary verbs and generic filler words are dropped",
			query: "should this function use a named return",
			want:  []string{"function", "named", "return"},
		},
		{
			name:  "a possessive pronoun is dropped",
			query: "can my library panic on bad input",
			want:  []string{"bad", "input", "library", "panic"},
		},
		{
			name:  "meaningful terms survive untouched",
			query: "context.Context is always the first parameter",
			want:  []string{"always", "context.context", "first", "parameter"},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sqlite.QueryTerms(tc.query))
		})
	}
}

func TestSearchRecordsRelevanceFloor(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	strong := mustRecord(t, &mecp.Record{
		ID:        "rec_strong",
		Kind:      mecp.KindConstraint,
		Subject:   "widget rendering pipeline",
		Statement: "The widget rendering pipeline draws every widget exactly once per frame.",
		Authority: mecp.AuthorityUser,
	})
	// weak shares exactly one generic word with the query and nothing else; a
	// real search for the strong record's topic should not surface it.
	weak := mustRecord(t, &mecp.Record{
		ID:        "rec_weak",
		Kind:      mecp.KindObservation,
		Subject:   "onboarding checklist",
		Statement: "New engineers should read the onboarding widget before their first day.",
		Authority: mecp.AuthorityUser,
	})
	for _, rec := range []*mecp.Record{strong, weak} {
		require.NoError(t, store.PutRecord(ctx, rec))
	}

	t.Run("a hit far below the best in the same result set is dropped", func(t *testing.T) {
		hits, err := store.SearchRecords(ctx, mecp.SearchQuery{Text: "widget rendering pipeline"})
		require.NoError(t, err)
		require.Len(t, hits, 1)
		require.Equal(t, "rec_strong", hits[0].Record.ID)
	})

	t.Run("disabling the floor lets the weak hit back in", func(t *testing.T) {
		lenient, err := sqlite.Open(filepath.Join(t.TempDir(), "lenient.db"), sqlite.WithMinSearchRelevance(0))
		require.NoError(t, err)
		t.Cleanup(func() { lenient.Close() })
		require.NoError(t, lenient.Migrate(t.Context()))
		for _, rec := range []*mecp.Record{strong, weak} {
			require.NoError(t, lenient.PutRecord(ctx, rec))
		}

		hits, err := lenient.SearchRecords(ctx, mecp.SearchQuery{Text: "widget rendering pipeline"})
		require.NoError(t, err)
		require.Len(t, hits, 2)
	})
}

// TestSearchRecordsMinScoreOption demonstrates both the mechanism and why it
// defaults to off. bm25's magnitude comes from an inverse-document-frequency
// term that shrinks toward zero as a corpus gets smaller, so a store with a
// single record scores every hit near zero regardless of how well it matches;
// a floor tuned for a large corpus would silently empty this one.
func TestSearchRecordsMinScoreOption(t *testing.T) {
	ctx := t.Context()
	rec := mustRecord(t, &mecp.Record{
		ID:        "rec_onboarding",
		Kind:      mecp.KindObservation,
		Subject:   "onboarding checklist",
		Statement: "New engineers should read the onboarding widget before their first day.",
		Authority: mecp.AuthorityUser,
	})

	t.Run("disabled by default, so a tiny store still returns its only match", func(t *testing.T) {
		store := newStore(t)
		require.NoError(t, store.PutRecord(ctx, rec))

		hits, err := store.SearchRecords(ctx, mecp.SearchQuery{Text: "onboarding widget"})
		require.NoError(t, err)
		require.Len(t, hits, 1)
	})

	t.Run("opting in with a floor tuned for a larger corpus empties this one", func(t *testing.T) {
		// This floor is tiny in absolute terms but still well above what a
		// one-record store's bm25 ever produces, which is the point.
		const floor = 0.01

		strict, err := sqlite.Open(filepath.Join(t.TempDir(), "strict.db"), sqlite.WithMinSearchScore(floor))
		require.NoError(t, err)
		t.Cleanup(func() { strict.Close() })
		require.NoError(t, strict.Migrate(ctx))
		require.NoError(t, strict.PutRecord(ctx, rec))

		hits, err := strict.SearchRecords(ctx, mecp.SearchQuery{Text: "onboarding widget"})
		require.NoError(t, err)
		require.Empty(t, hits)
	})
}
