package sqlite_test

import (
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/stretchr/testify/require"
)

func TestDeleteProposal(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	p := &mecp.Proposal{
		ID: "prop_1", Key: "k1", Status: mecp.ProposalRejected, PrincipalID: "lestrrat",
		ClientID: "claude-code", Kind: mecp.KindDecision, Subject: "s",
		Statement: "A rule.", CreatedAt: now,
		Evidence: []mecp.Source{{ID: "src_1", Type: mecp.SourceFile, Locator: "file:///doc.md"}},
	}
	_, _, err := store.PutProposal(ctx, p)
	require.NoError(t, err)

	require.NoError(t, store.DeleteProposal(ctx, "prop_1"))

	t.Run("the proposal is gone", func(t *testing.T) {
		_, err := store.GetProposal(ctx, "prop_1")
		require.ErrorIs(t, err, mecp.ErrNotFound)
	})

	t.Run("its key is free again", func(t *testing.T) {
		replacement := &mecp.Proposal{
			ID: "prop_2", Key: "k1", Status: mecp.ProposalPending, PrincipalID: "lestrrat",
			ClientID: "claude-code", Kind: mecp.KindDecision, Subject: "s",
			Statement: "The same rule, scoped properly.", CreatedAt: now,
		}
		stored, created, err := store.PutProposal(ctx, replacement)
		require.NoError(t, err)
		require.True(t, created, "a deleted proposal must not keep blocking its key")
		require.Equal(t, "prop_2", stored.ID)
	})

	t.Run("deleting one that does not exist says so", func(t *testing.T) {
		require.ErrorIs(t, store.DeleteProposal(ctx, "prop_absent"), mecp.ErrNotFound)
	})
}
