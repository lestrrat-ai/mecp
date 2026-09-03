package source_test

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/source"
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

func TestJSONLRoundTrip(t *testing.T) {
	ctx := t.Context()
	origin := newStore(t)
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	records := []*mecp.Record{
		{
			ID: "rec_b", Kind: mecp.KindDecision, Subject: "conformance suite",
			Statement:   "The suite runs against a controlled commit.",
			Rationale:   "Reproducibility comes from choosing the commit at release time.",
			Scope:       mecp.Scope{Repository: "https://github.com/lestrrat-go/helium", TaskKinds: []mecp.TaskKind{mecp.TaskRelease}},
			Authority:   mecp.AuthorityUser,
			Sensitivity: mecp.SensitivityProject,
			Tags:        []string{"conformance"},
			Sources: []mecp.Source{{
				ID: "src_1", Type: mecp.SourceConversation, Locator: "turn://42",
				ExactExcerpt: "We pin the suite before each release.",
			}},
		},
		{
			ID: "rec_a", Kind: mecp.KindConstraint, Subject: "untrusted stylesheets",
			Statement:   "Untrusted XSLT stylesheets must never be executed.",
			Authority:   mecp.AuthorityRepository,
			Sensitivity: mecp.SensitivityProject,
		},
	}
	for _, rec := range records {
		rec.Normalize(now)
		require.NoError(t, origin.PutRecord(ctx, rec))
	}

	var buf bytes.Buffer
	n, err := source.ExportJSONL(ctx, origin, &buf, false)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	t.Run("export is ordered so two runs are byte-identical", func(t *testing.T) {
		var again bytes.Buffer
		_, err := source.ExportJSONL(ctx, origin, &again, false)
		require.NoError(t, err)
		require.Equal(t, buf.String(), again.String())
	})

	t.Run("restore reproduces every record", func(t *testing.T) {
		restored := newStore(t)
		n, err := source.ImportJSONL(ctx, restored, bytes.NewReader(buf.Bytes()))
		require.NoError(t, err)
		require.Equal(t, 2, n)

		got, err := restored.GetRecord(ctx, "rec_b")
		require.NoError(t, err)
		require.Equal(t, "The suite runs against a controlled commit.", got.Statement)
		require.Equal(t, []mecp.TaskKind{mecp.TaskRelease}, got.Scope.TaskKinds)
		require.Len(t, got.Sources, 1)
		require.Equal(t, "We pin the suite before each release.", got.Sources[0].ExactExcerpt)
	})

	t.Run("restored records are searchable, so the index was rebuilt", func(t *testing.T) {
		restored := newStore(t)
		_, err := source.ImportJSONL(ctx, restored, bytes.NewReader(buf.Bytes()))
		require.NoError(t, err)

		hits, err := restored.SearchRecords(ctx, mecp.SearchQuery{Text: "conformance suite commit"})
		require.NoError(t, err)
		require.NotEmpty(t, hits)
	})

	t.Run("proposals are included on request", func(t *testing.T) {
		_, _, err := origin.PutProposal(ctx, &mecp.Proposal{
			ID: "prop_1", Key: "k1", Status: mecp.ProposalPending, PrincipalID: "local-user",
			ClientID: "claude-code", Kind: mecp.KindDecision, Subject: "s",
			Statement: "A suggestion.", CreatedAt: now,
		})
		require.NoError(t, err)

		var withProposals bytes.Buffer
		n, err := source.ExportJSONL(ctx, origin, &withProposals, true)
		require.NoError(t, err)
		require.Equal(t, 3, n)

		restored := newStore(t)
		_, err = source.ImportJSONL(ctx, restored, bytes.NewReader(withProposals.Bytes()))
		require.NoError(t, err)

		p, err := restored.GetProposal(ctx, "prop_1")
		require.NoError(t, err)
		require.Equal(t, mecp.ProposalPending, p.Status)
	})

	t.Run("a malformed line is reported with its number", func(t *testing.T) {
		restored := newStore(t)
		_, err := source.ImportJSONL(ctx, restored, bytes.NewReader([]byte("{\"format\":\"mecp-export\",\"type\":\"record\"}\n")))
		require.Error(t, err)
		require.Contains(t, err.Error(), "line 1")
	})

	t.Run("a foreign format is refused", func(t *testing.T) {
		restored := newStore(t)
		_, err := source.ImportJSONL(ctx, restored, bytes.NewReader([]byte("{\"format\":\"something-else\",\"type\":\"record\"}\n")))
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown format")
	})
}
