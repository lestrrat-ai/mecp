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

func namedReturnRule() mecp.ExtractedRule {
	return mecp.ExtractedRule{
		Kind:      mecp.KindConstraint,
		Subject:   "style",
		Statement: "Do not use named return values in Go.",
		Quote:     "Do not use named return values.",
	}
}

func TestExtractRulesActivates(t *testing.T) {
	svc, docPath := extractService(t)

	req := mecp.ExtractRulesRequest{
		Caller:       proposingCaller(),
		DocumentPath: docPath,
		Scope:        mecp.Scope{PathPatterns: []string{"*.go"}},
		Rules:        []mecp.ExtractedRule{namedReturnRule()},
	}

	res, err := svc.ExtractRules(t.Context(), req)
	require.NoError(t, err)

	t.Run("a clean rule becomes a record without anyone reviewing it", func(t *testing.T) {
		require.Len(t, res.Accepted, 1)
		require.Equal(t, 1, res.ActivatedCount)
		require.Zero(t, res.ReviewCount)
		require.NotEmpty(t, res.Accepted[0].RecordID)
		require.Empty(t, res.Accepted[0].ProposalID)
		require.Empty(t, res.Accepted[0].NeedsReview)
	})

	t.Run("the record is live in a context pack", func(t *testing.T) {
		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:    agentCaller(),
			Task:      "Write a function in helium",
			Workspace: mecp.Workspace{Repository: heliumRepo, RelevantPaths: []string{"parser.go"}},
		})
		require.NoError(t, err)
		require.Contains(t, itemIDs(pack.Items), res.Accepted[0].RecordID)
	})

	t.Run("it claims the authority configured for documents, not one a model chose", func(t *testing.T) {
		store := extractStore(t, svc)
		rec, err := store.GetRecord(t.Context(), res.Accepted[0].RecordID)
		require.NoError(t, err)
		require.Equal(t, mecp.AuthorityUser, rec.Authority)
	})

	t.Run("it is revalidated against the document it came from", func(t *testing.T) {
		store := extractStore(t, svc)
		rec, err := store.GetRecord(t.Context(), res.Accepted[0].RecordID)
		require.NoError(t, err)
		require.Equal(t, mecp.ValidateFileAndHash, rec.ValidationPolicy)
		require.Equal(t, mecp.HashContent(rulesDoc), rec.Sources[0].ContentHash)
	})

	t.Run("re-running the same extraction changes nothing", func(t *testing.T) {
		again, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, res.Accepted[0].RecordID, again.Accepted[0].RecordID)
		require.False(t, again.Accepted[0].Created)
		require.Zero(t, again.ActivatedCount)

		store := extractStore(t, svc)
		all, err := store.QueryRecords(t.Context(), mecp.RecordQuery{})
		require.NoError(t, err)
		require.Len(t, all, 1, "a second run must not leave a duplicate behind")
	})

	t.Run("nothing sits in the review queue", func(t *testing.T) {
		store := extractStore(t, svc)
		pending, err := store.QueryProposals(t.Context(), mecp.ProposalQuery{
			Statuses: []mecp.ProposalStatus{mecp.ProposalPending},
		})
		require.NoError(t, err)
		require.Empty(t, pending)
	})
}

func TestExtractRulesHoldsWhatNeedsAPerson(t *testing.T) {
	t.Run("a statement that shares almost nothing with its quote", func(t *testing.T) {
		svc, docPath := extractService(t)

		res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller:       proposingCaller(),
			DocumentPath: docPath,
			Rules: []mecp.ExtractedRule{{
				Kind:      mecp.KindConstraint,
				Subject:   "style",
				Statement: "Deployment happens every Friday afternoon without exception.",
				Quote:     "Do not use named return values.",
			}},
		})
		require.NoError(t, err)
		require.Equal(t, 1, res.ReviewCount)
		require.Zero(t, res.ActivatedCount)
		require.NotEmpty(t, res.Accepted[0].ProposalID)
		require.Empty(t, res.Accepted[0].RecordID)
		require.Equal(t, mecp.ReviewDrifted, res.Accepted[0].NeedsReview[0].Reason)
	})

	t.Run("several rules under one heading are siblings, not conflicts", func(t *testing.T) {
		svc, docPath := extractService(t)

		// A document says many things about one subject. The first activates,
		// and the rest must not be held merely for sharing its heading.
		res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller: proposingCaller(), DocumentPath: docPath,
			Rules: []mecp.ExtractedRule{
				namedReturnRule(),
				{
					Kind:      mecp.KindPreference,
					Subject:   "style",
					Statement: "Prefer early returns from functions and early continue from loops.",
					Quote:     "Prefer early returns from functions, and early continue from loops.",
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, 2, res.ActivatedCount)
		require.Zero(t, res.ReviewCount)
	})

	t.Run("a rule that contradicts a record from somewhere else", func(t *testing.T) {
		svc, docPath := extractService(t)
		store := extractStore(t, svc)

		existing := &mecp.Record{
			ID: "rec_existing", Kind: mecp.KindPreference, Subject: "style",
			Statement: "Named return values are fine when they document the result.",
			Scope:     mecp.Scope{User: "local-user"},
			Authority: mecp.AuthorityUser,
		}
		existing.Normalize(testNow)
		require.NoError(t, store.PutRecord(t.Context(), existing))

		res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller: proposingCaller(), DocumentPath: docPath,
			Rules: []mecp.ExtractedRule{namedReturnRule()},
		})
		require.NoError(t, err)
		require.Equal(t, 1, res.ReviewCount)
		require.Equal(t, mecp.ReviewConflicts, res.Accepted[0].NeedsReview[0].Reason)
		require.Contains(t, res.Accepted[0].NeedsReview[0].Related, "rec_existing")
	})

	t.Run("a rule an active record already covers", func(t *testing.T) {
		svc, docPath := extractService(t)
		store := extractStore(t, svc)

		existing := &mecp.Record{
			ID: "rec_dup", Kind: mecp.KindConstraint, Subject: "style",
			Statement: "Do not use named return values in Go.",
			Scope:     mecp.Scope{User: "local-user"},
			Authority: mecp.AuthorityUser,
		}
		existing.Normalize(testNow)
		require.NoError(t, store.PutRecord(t.Context(), existing))

		res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller: proposingCaller(), DocumentPath: docPath,
			Rules: []mecp.ExtractedRule{namedReturnRule()},
		})
		require.NoError(t, err)
		require.Equal(t, mecp.ReviewDuplicates, res.Accepted[0].NeedsReview[0].Reason)
	})

	t.Run("a held rule is not live until someone approves it", func(t *testing.T) {
		svc, docPath := extractService(t)

		_, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
			Caller: proposingCaller(), DocumentPath: docPath,
			Rules: []mecp.ExtractedRule{{
				Kind:      mecp.KindConstraint,
				Statement: "Deployment happens every Friday afternoon without exception.",
				Quote:     "Do not use named return values.",
			}},
		})
		require.NoError(t, err)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Deploy something", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.Empty(t, pack.Items)
	})
}

func TestExtractRulesRefusesUnquotableRules(t *testing.T) {
	svc, docPath := extractService(t)

	res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
		Caller:       proposingCaller(),
		DocumentPath: docPath,
		Rules: []mecp.ExtractedRule{
			{Kind: mecp.KindConstraint, Statement: "Always deploy on Fridays.",
				Quote: "Always deploy on Fridays."},
			namedReturnRule(),
		},
	})
	require.NoError(t, err)

	t.Run("a rule the document does not contain is refused", func(t *testing.T) {
		require.Len(t, res.Rejected, 1)
		require.Contains(t, res.Rejected[0].Reason, "does not appear")
	})

	t.Run("the rules that do check out still go through", func(t *testing.T) {
		require.Len(t, res.Accepted, 1)
		require.Equal(t, 1, res.ActivatedCount)
	})

	t.Run("the refusal is reported rather than silent", func(t *testing.T) {
		require.NotEmpty(t, res.Warnings)
	})
}

func TestExtractRulesRefusesDeadScopes(t *testing.T) {
	svc, docPath := extractService(t)

	rule := namedReturnRule()
	rule.Scope = &mecp.Scope{Conditions: map[string]string{"tool": "bash"}}

	res, err := svc.ExtractRules(t.Context(), mecp.ExtractRulesRequest{
		Caller: proposingCaller(), DocumentPath: docPath,
		Rules: []mecp.ExtractedRule{rule},
	})
	require.NoError(t, err)
	require.Empty(t, res.Accepted)
	require.Len(t, res.Rejected, 1)
	require.Contains(t, res.Rejected[0].Reason, "condition")
}

func TestExtractRulesReportsDecidedCollisions(t *testing.T) {
	svc, docPath := extractService(t)

	// A drifted statement is held for review, which is the only path that still
	// creates a proposal, and therefore the only one a decision can block.
	req := mecp.ExtractRulesRequest{
		Caller:       proposingCaller(),
		DocumentPath: docPath,
		Rules: []mecp.ExtractedRule{{
			Kind:      mecp.KindConstraint,
			Statement: "Deployment happens every Friday afternoon without exception.",
			Quote:     "Do not use named return values.",
		}},
	}

	first, err := svc.ExtractRules(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 1, first.ReviewCount)

	store := extractStore(t, svc)
	pending, err := store.QueryProposals(t.Context(), mecp.ProposalQuery{
		Statuses: []mecp.ProposalStatus{mecp.ProposalPending},
	})
	require.NoError(t, err)
	require.NoError(t, mecp.RejectProposal(t.Context(), store, pending[0], "lestrrat", "no", testNow))

	blocked, err := svc.ExtractRules(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, blocked.Accepted, "nothing was stored, so nothing may be reported as accepted")
	require.Len(t, blocked.Blocked, 1)
	require.Equal(t, mecp.ProposalRejected, blocked.Blocked[0].Status)

	t.Run("reopening lets it be filed again", func(t *testing.T) {
		rejected, err := store.QueryProposals(t.Context(), mecp.ProposalQuery{
			Statuses: []mecp.ProposalStatus{mecp.ProposalRejected},
		})
		require.NoError(t, err)
		require.NoError(t, mecp.ReopenProposal(t.Context(), store, rejected[0], "reconsidered", testNow))

		again, err := svc.ExtractRules(t.Context(), req)
		require.NoError(t, err)
		require.Len(t, again.Accepted, 1)
		require.Empty(t, again.Blocked)
	})
}

func TestExtractRulesAuthorization(t *testing.T) {
	svc, docPath := extractService(t)
	rules := []mecp.ExtractedRule{namedReturnRule()}

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

	t.Run("without a document reader the tool refuses rather than trusting", func(t *testing.T) {
		store := newExtractStore(t)
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
