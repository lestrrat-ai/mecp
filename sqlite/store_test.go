package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/sqlite"
	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(t.Context()))
	return store
}

func mustRecord(t *testing.T, rec *mecp.Record) *mecp.Record {
	t.Helper()
	rec.Normalize(time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC))
	require.NoError(t, rec.Validate())
	return rec
}

func TestRecordRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	rec := mustRecord(t, &mecp.Record{
		ID:        "rec_test_decision_004",
		Kind:      mecp.KindDecision,
		Subject:   "release conformance testing",
		Statement: "The project runs the conformance-test repository against a controlled commit before a release.",
		Rationale: "Reproducibility comes from choosing the commit as part of the release process.",
		Scope: mecp.Scope{
			User:       "local-user",
			Repository: "git@github.com:lestrrat-go/helium.git",
			TaskKinds:  []mecp.TaskKind{mecp.TaskRelease},
		},
		Authority:   mecp.AuthorityUser,
		Sensitivity: mecp.SensitivityProject,
		Tags:        []string{"conformance", "release"},
		Sources: []mecp.Source{{
			ID:           "src_conversation_2026_07_03",
			Type:         mecp.SourceConversation,
			Locator:      "conversation://2026-07-03",
			ExactExcerpt: "The test repository is executed before releases against a definite commit.",
		}},
	})
	require.NoError(t, store.PutRecord(ctx, rec))

	t.Run("get returns every field", func(t *testing.T) {
		got, err := store.GetRecord(ctx, rec.ID)
		require.NoError(t, err)
		require.Equal(t, rec.Statement, got.Statement)
		require.Equal(t, rec.Rationale, got.Rationale)
		require.Equal(t, mecp.AuthorityUser, got.Authority)
		require.Equal(t, mecp.StatusActive, got.Status)
		require.Equal(t, []string{"conformance", "release"}, got.Tags)
		require.Equal(t, []mecp.TaskKind{mecp.TaskRelease}, got.Scope.TaskKinds)
		require.Len(t, got.Sources, 1)
		require.Equal(t, "conversation://2026-07-03", got.Sources[0].Locator)
	})

	t.Run("scope repository is canonicalized on write", func(t *testing.T) {
		got, err := store.GetRecord(ctx, rec.ID)
		require.NoError(t, err)
		require.Equal(t, "https://github.com/lestrrat-go/helium", got.Scope.Repository)
	})

	t.Run("put is idempotent", func(t *testing.T) {
		require.NoError(t, store.PutRecord(ctx, rec))
		recs, err := store.QueryRecords(ctx, mecp.RecordQuery{IDs: []string{rec.ID}})
		require.NoError(t, err)
		require.Len(t, recs, 1)
		require.Len(t, recs[0].Sources, 1)
	})

	t.Run("delete removes the record from the index too", func(t *testing.T) {
		require.NoError(t, store.PutRecord(ctx, rec))
		require.NoError(t, store.DeleteRecord(ctx, rec.ID))

		_, err := store.GetRecord(ctx, rec.ID)
		require.ErrorIs(t, err, mecp.ErrNotFound)

		hits, err := store.SearchRecords(ctx, mecp.SearchQuery{Text: "conformance"})
		require.NoError(t, err)
		require.Empty(t, hits)
	})
}

func TestSearchRecords(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	conformance := mustRecord(t, &mecp.Record{
		ID:          "rec_conformance",
		Kind:        mecp.KindDecision,
		Subject:     "release conformance testing",
		Statement:   "The conformance suite runs against a controlled commit before a release.",
		Scope:       mecp.Scope{Repository: "https://github.com/lestrrat-go/helium"},
		Authority:   mecp.AuthorityUser,
		Sensitivity: mecp.SensitivityProject,
		Tags:        []string{"conformance"},
	})
	styles := mustRecord(t, &mecp.Record{
		ID:          "rec_stylesheet",
		Kind:        mecp.KindConstraint,
		Subject:     "untrusted stylesheets",
		Statement:   "Untrusted XSLT stylesheets must never be executed during parsing.",
		Scope:       mecp.Scope{Repository: "https://github.com/lestrrat-go/helium"},
		Authority:   mecp.AuthorityUser,
		Sensitivity: mecp.SensitivityProject,
	})
	personal := mustRecord(t, &mecp.Record{
		ID:          "rec_personal",
		Kind:        mecp.KindPreference,
		Subject:     "conformance reporting style",
		Statement:   "Personal note about conformance reporting.",
		Authority:   mecp.AuthorityUser,
		Sensitivity: mecp.SensitivityPersonal,
	})
	for _, rec := range []*mecp.Record{conformance, styles, personal} {
		require.NoError(t, store.PutRecord(ctx, rec))
	}

	t.Run("matches on statement text", func(t *testing.T) {
		hits, err := store.SearchRecords(ctx, mecp.SearchQuery{Text: "why is the conformance suite pinned?"})
		require.NoError(t, err)
		require.NotEmpty(t, hits)
		require.Equal(t, "rec_conformance", hits[0].Record.ID)
		require.Contains(t, hits[0].Terms, "conformance")
		require.InDelta(t, 1.0, hits[0].Relevance, 0.001)
	})

	t.Run("sensitivity ceiling is applied before matching", func(t *testing.T) {
		hits, err := store.SearchRecords(ctx, mecp.SearchQuery{
			RecordQuery: mecp.RecordQuery{MaxSensitivity: mecp.SensitivityProject},
			Text:        "conformance",
		})
		require.NoError(t, err)
		for _, hit := range hits {
			require.NotEqual(t, "rec_personal", hit.Record.ID)
		}
	})

	t.Run("repository restriction excludes global records when asked", func(t *testing.T) {
		hits, err := store.SearchRecords(ctx, mecp.SearchQuery{
			RecordQuery: mecp.RecordQuery{
				RestrictRepositories: true,
				Repositories:         []string{"https://github.com/lestrrat-go/helium"},
			},
			Text: "conformance",
		})
		require.NoError(t, err)
		require.Len(t, hits, 1)
		require.Equal(t, "rec_conformance", hits[0].Record.ID)
	})

	t.Run("query text that is only stop words matches nothing", func(t *testing.T) {
		hits, err := store.SearchRecords(ctx, mecp.SearchQuery{Text: "why is it that the"})
		require.NoError(t, err)
		require.Empty(t, hits)
	})

	t.Run("FTS operators in caller text are treated as literals", func(t *testing.T) {
		hits, err := store.SearchRecords(ctx, mecp.SearchQuery{Text: `conformance OR "*"`})
		require.NoError(t, err)
		require.NotEmpty(t, hits)
	})
}

func TestSupersession(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	old := mustRecord(t, &mecp.Record{
		ID: "rec_old", Kind: mecp.KindDecision, Subject: "test workflow",
		Statement: "The suite follows upstream automatically.",
		Authority: mecp.AuthorityUser, Sensitivity: mecp.SensitivityProject,
	})
	require.NoError(t, store.PutRecord(ctx, old))

	replacement := mustRecord(t, &mecp.Record{
		ID: "rec_new", Kind: mecp.KindDecision, Subject: "test workflow",
		Statement: "The suite runs against a controlled commit.",
		Authority: mecp.AuthorityUser, Sensitivity: mecp.SensitivityProject,
		Supersedes: []string{"rec_old"},
	})
	require.NoError(t, store.PutRecord(ctx, replacement))

	got, err := store.SupersededBy(ctx, []string{"rec_old", "rec_new"})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"rec_old": {"rec_new"}}, got)

	t.Run("deleting the newer record clears the dangling edge", func(t *testing.T) {
		require.NoError(t, store.DeleteRecord(ctx, "rec_new"))
		got, err := store.SupersededBy(ctx, []string{"rec_old"})
		require.NoError(t, err)
		require.Empty(t, got)
	})
}

func TestProposalIdempotency(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	first := &mecp.Proposal{
		ID: "prop_1", Key: "session-123:decision:controlled-commit", Status: mecp.ProposalPending,
		PrincipalID: "local-user", ClientID: "claude-code", Kind: mecp.KindDecision,
		Subject: "release conformance testing", Statement: "Run the suite against a controlled commit.",
		CreatedAt: now,
		Evidence:  []mecp.Source{{ID: "src_1", Type: mecp.SourceConversation, Locator: "turn://42"}},
	}
	stored, created, err := store.PutProposal(ctx, first)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "prop_1", stored.ID)

	second := *first
	second.ID = "prop_2"
	again, created, err := store.PutProposal(ctx, &second)
	require.NoError(t, err)
	require.False(t, created, "a repeated proposal key must not create a second proposal")
	require.Equal(t, "prop_1", again.ID)
	require.Len(t, again.Evidence, 1)

	pending, err := store.QueryProposals(ctx, mecp.ProposalQuery{Statuses: []mecp.ProposalStatus{mecp.ProposalPending}})
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestContentVersionChangesWithWrites(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()

	before, err := store.ContentVersion(ctx)
	require.NoError(t, err)

	require.NoError(t, store.PutRecord(ctx, mustRecord(t, &mecp.Record{
		ID: "rec_v", Kind: mecp.KindObservation, Subject: "x", Statement: "y",
		Authority: mecp.AuthorityInferred, Sensitivity: mecp.SensitivityProject,
	})))

	after, err := store.ContentVersion(ctx)
	require.NoError(t, err)
	require.NotEqual(t, before, after)
}
