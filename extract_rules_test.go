package mecp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/source"
	"github.com/lestrrat-ai/mecp/sqlite"
	"github.com/stretchr/testify/require"
)

const rulesDoc = `# Go

## Style

- Do not use named return values.
- Prefer early returns from functions, and early continue from loops.

## Testing

- Only use github.com/stretchr/testify/require and not assert.
`

// extractStores keeps the store each service was built over, so a test can
// reach past the service to review what it filed.
var extractStores = map[mecp.Service]*sqlite.Store{}

// extractService wires a service with a document reader rooted at a directory
// holding one instruction file.
func extractService(t *testing.T) (mecp.Service, string) {
	t.Helper()

	root := t.TempDir()
	docPath := filepath.Join(root, "go.md")
	require.NoError(t, os.WriteFile(docPath, []byte(rulesDoc), 0o600))

	store := newExtractStore(t)
	svc, err := mecp.New(store,
		mecp.WithClock(mecp.FixedClock{Time: testNow}),
		mecp.WithDocumentReader(source.NewDocumentReader([]string{root})),
	)
	require.NoError(t, err)

	extractStores[svc] = store
	t.Cleanup(func() { delete(extractStores, svc) })
	return svc, docPath
}

func newExtractStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(t.Context()))
	return store
}

func extractStore(t *testing.T, svc mecp.Service) *sqlite.Store {
	t.Helper()
	store, ok := extractStores[svc]
	require.True(t, ok)
	return store
}

func TestExtractRules(t *testing.T) {
	svc, docPath := extractService(t)

	req := mecp.ExtractRulesRequest{
		Caller:       proposingCaller(),
		DocumentPath: docPath,
		Scope:        mecp.Scope{PathPatterns: []string{"*.go"}},
		Rules: []mecp.ExtractedRule{
			{
				Kind:      mecp.KindConstraint,
				Subject:   "style",
				Statement: "Do not use named return values in Go.",
				Quote:     "Do not use named return values.",
			},
			{
				Kind:      mecp.KindPreference,
				Subject:   "style",
				Statement: "Prefer early returns from functions.",
				Quote:     "- Prefer early returns from functions, and early continue from loops.",
			},
		},
	}

	t.Run("a quoted rule becomes a pending proposal", func(t *testing.T) {
		res, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Len(t, res.Accepted, 2)
		require.Empty(t, res.Rejected)
		require.Equal(t, 2, res.CreatedCount)
	})

	t.Run("the line the quote came from is reported", func(t *testing.T) {
		res, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, 5, res.Accepted[0].Line)
		require.Equal(t, 6, res.Accepted[1].Line)
	})

	t.Run("a bullet marker on the quote is tolerated", func(t *testing.T) {
		// The second rule quotes the line including its "- ", which a model
		// copying from the document will often do.
		res, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Empty(t, res.Rejected)
	})

	t.Run("re-running over an unchanged document queues nothing twice", func(t *testing.T) {
		res, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Zero(t, res.CreatedCount)
		require.Equal(t, 2, res.PendingCount)
	})

	t.Run("the document hash is recorded so a record can go stale", func(t *testing.T) {
		res, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, mecp.HashContent(rulesDoc), res.ContentHash)
	})
}

func TestExtractRulesRefusesUnquotableRules(t *testing.T) {
	svc, docPath := extractService(t)

	res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
		Caller:       proposingCaller(),
		DocumentPath: docPath,
		Rules: []mecp.ExtractedRule{
			{
				Kind:      mecp.KindConstraint,
				Statement: "Always deploy straight to production on Fridays.",
				Quote:     "Always deploy straight to production on Fridays.",
			},
			{
				Kind:      mecp.KindConstraint,
				Statement: "Do not use named return values in Go.",
				Quote:     "Do not use named return values.",
			},
		},
	})
	require.NoError(t, err)

	t.Run("a rule the document does not contain is refused", func(t *testing.T) {
		require.Len(t, res.Rejected, 1)
		require.Contains(t, res.Rejected[0].Reason, "does not appear")
		require.Contains(t, res.Rejected[0].Statement, "Fridays")
	})

	t.Run("the rules that do check out still go through", func(t *testing.T) {
		require.Len(t, res.Accepted, 1)
	})

	t.Run("the refusal is reported rather than silent", func(t *testing.T) {
		require.NotEmpty(t, res.Warnings)
	})
}

func TestExtractRulesAuthorization(t *testing.T) {
	svc, docPath := extractService(t)

	rules := []mecp.ExtractedRule{{
		Kind: mecp.KindConstraint, Statement: "x.", Quote: "Do not use named return values.",
	}}

	t.Run("the propose capability is required", func(t *testing.T) {
		_, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller: agentCaller(), DocumentPath: docPath, Rules: rules,
		})
		require.Equal(t, mecp.CodeProposalDisabled, mecp.CodeOf(err))
	})

	t.Run("a document outside the roots is refused", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "elsewhere.md")
		require.NoError(t, os.WriteFile(other, []byte("- a rule\n"), 0o600))

		_, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller: proposingCaller(), DocumentPath: other, Rules: rules,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "outside every configured document root")
	})

	t.Run("path traversal out of a root is refused", func(t *testing.T) {
		_, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller:       proposingCaller(),
			DocumentPath: filepath.Join(filepath.Dir(docPath), "..", "..", "etc", "passwd"),
			Rules:        rules,
		})
		require.Error(t, err)
	})

	t.Run("without a document store the tool refuses rather than trusting", func(t *testing.T) {
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
		require.NoError(t, err)
		t.Cleanup(func() { store.Close() })
		require.NoError(t, store.Migrate(t.Context()))

		bare, err := mecp.New(store, mecp.WithClock(mecp.FixedClock{Time: testNow}))
		require.NoError(t, err)

		_, err = bare.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller: proposingCaller(), DocumentPath: docPath, Rules: rules,
		})
		require.Equal(t, mecp.CodeSourceUnavailable, mecp.CodeOf(err))
	})

	t.Run("too many rules at once is refused", func(t *testing.T) {
		many := make([]mecp.ExtractedRule, 201)
		for i := range many {
			many[i] = rules[0]
		}
		_, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller: proposingCaller(), DocumentPath: docPath, Rules: many,
		})
		require.Equal(t, mecp.CodeInvalidRecord, mecp.CodeOf(err))
	})
}

func TestExtractedProposalsStayInactive(t *testing.T) {
	svc, docPath := extractService(t)

	_, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
		Caller:       proposingCaller(),
		DocumentPath: docPath,
		Rules: []mecp.ExtractedRule{{
			Kind:      mecp.KindConstraint,
			Statement: "Do not use named return values in Go.",
			Quote:     "Do not use named return values.",
		}},
	})
	require.NoError(t, err)

	pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
		Caller: agentCaller(), Task: "Write a Go function", Workspace: heliumWorkspace(),
	})
	require.NoError(t, err)
	require.Empty(t, pack.Items, "an extracted rule must not act as context before review")
}

func TestExtractRulesRefusesDeadScopes(t *testing.T) {
	svc, docPath := extractService(t)

	t.Run("a condition scope is refused, because nothing supplies conditions", func(t *testing.T) {
		res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller:       proposingCaller(),
			DocumentPath: docPath,
			Rules: []mecp.ExtractedRule{{
				Kind:      mecp.KindConstraint,
				Statement: "Do not use named return values in Go.",
				Quote:     "Do not use named return values.",
				Scope:     &mecp.Scope{Conditions: map[string]string{"tool": "bash"}},
			}},
		})
		require.NoError(t, err)
		require.Empty(t, res.Accepted)
		require.Len(t, res.Rejected, 1)
		require.Contains(t, res.Rejected[0].Reason, "condition")
		require.Contains(t, res.Rejected[0].Reason, "repository, path, or task kind")
	})

	t.Run("a document-wide condition scope is refused too", func(t *testing.T) {
		res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller:       proposingCaller(),
			DocumentPath: docPath,
			Scope:        mecp.Scope{Conditions: map[string]string{"language": "go"}},
			Rules: []mecp.ExtractedRule{{
				Kind:      mecp.KindConstraint,
				Statement: "Do not use named return values in Go.",
				Quote:     "Do not use named return values.",
			}},
		})
		require.NoError(t, err)
		require.Len(t, res.Rejected, 1)
	})

	t.Run("the scopes that do work still go through", func(t *testing.T) {
		res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller:       proposingCaller(),
			DocumentPath: docPath,
			Scope:        mecp.Scope{PathPatterns: []string{"*.go"}},
			Rules: []mecp.ExtractedRule{{
				Kind:      mecp.KindConstraint,
				Statement: "Do not use named return values in Go.",
				Quote:     "Do not use named return values.",
			}},
		})
		require.NoError(t, err)
		require.Len(t, res.Accepted, 1)
		require.Empty(t, res.Rejected)
	})
}

func TestExtractRulesReportsDecidedCollisions(t *testing.T) {
	svc, docPath := extractService(t)

	req := mecp.ExtractRulesRequest{
		Caller:       proposingCaller(),
		DocumentPath: docPath,
		Rules: []mecp.ExtractedRule{{
			Kind:      mecp.KindConstraint,
			Statement: "Do not use named return values in Go.",
			Quote:     "Do not use named return values.",
		}},
	}

	first, err := svc.ExtractRules(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, first.Accepted, 1)
	require.Equal(t, 1, first.CreatedCount)

	t.Run("a second run while it is still pending is an acceptance", func(t *testing.T) {
		again, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Len(t, again.Accepted, 1)
		require.Zero(t, again.CreatedCount)
		require.Equal(t, 1, again.PendingCount)
		require.Empty(t, again.Blocked)
	})

	t.Run("once rejected, refiling is reported as blocked rather than accepted", func(t *testing.T) {
		store := extractStore(t, svc)
		pending, err := store.QueryProposals(t.Context(), mecp.ProposalQuery{
			Statuses: []mecp.ProposalStatus{mecp.ProposalPending},
		})
		require.NoError(t, err)
		require.NotEmpty(t, pending)
		require.NoError(t, mecp.RejectProposal(t.Context(), store, pending[0], "lestrrat", "wrong scope", testNow))

		blockedRun, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Empty(t, blockedRun.Accepted, "nothing was stored, so nothing may be reported as accepted")
		require.Len(t, blockedRun.Blocked, 1)
		require.Equal(t, mecp.ProposalRejected, blockedRun.Blocked[0].Status)
		require.Contains(t, blockedRun.Blocked[0].Reason, "reopen or delete")
		require.NotEmpty(t, blockedRun.Warnings)
	})

	t.Run("reopening lets the same rule be filed again", func(t *testing.T) {
		store := extractStore(t, svc)
		rejected, err := store.QueryProposals(t.Context(), mecp.ProposalQuery{
			Statuses: []mecp.ProposalStatus{mecp.ProposalRejected},
		})
		require.NoError(t, err)
		require.NotEmpty(t, rejected)

		require.NoError(t, mecp.ReopenProposal(t.Context(), store, rejected[0], "scope guard added", testNow))

		reopened, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Len(t, reopened.Accepted, 1)
		require.Empty(t, reopened.Blocked)
	})
}

func TestReopenProposal(t *testing.T) {
	store := newExtractStore(t)
	ctx := t.Context()

	p := &mecp.Proposal{
		ID: "prop_x", Key: "k", Status: mecp.ProposalPending, PrincipalID: "lestrrat",
		ClientID: "claude-code", Kind: mecp.KindDecision, Subject: "s",
		Statement: "A rule.", CreatedAt: testNow,
	}
	_, _, err := store.PutProposal(ctx, p)
	require.NoError(t, err)

	t.Run("a pending proposal cannot be reopened", func(t *testing.T) {
		require.Error(t, mecp.ReopenProposal(ctx, store, p, "", testNow))
	})

	t.Run("a rejection reopens and keeps both notes", func(t *testing.T) {
		require.NoError(t, mecp.RejectProposal(ctx, store, p, "lestrrat", "wrong scope", testNow))
		require.NoError(t, mecp.ReopenProposal(ctx, store, p, "scope guard added", testNow))

		got, err := store.GetProposal(ctx, "prop_x")
		require.NoError(t, err)
		require.Equal(t, mecp.ProposalPending, got.Status)
		require.Nil(t, got.DecidedAt)
		require.Contains(t, got.DecisionNote, "wrong scope")
		require.Contains(t, got.DecisionNote, "scope guard added")
	})

	t.Run("an approved proposal points at its record instead", func(t *testing.T) {
		approved := &mecp.Proposal{
			ID: "prop_y", Key: "k2", Status: mecp.ProposalApproved, PrincipalID: "lestrrat",
			ClientID: "claude-code", Kind: mecp.KindDecision, Subject: "s",
			Statement: "Another rule.", CreatedAt: testNow, ResultRecordID: "rec_abc",
		}
		_, _, err := store.PutProposal(ctx, approved)
		require.NoError(t, err)

		err = mecp.ReopenProposal(ctx, store, approved, "", testNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "rec_abc")
	})
}
