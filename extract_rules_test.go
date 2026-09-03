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

// extractService wires a service with a document store rooted at a directory
// holding one instruction file.
func extractService(t *testing.T) (mecp.Service, string) {
	t.Helper()

	root := t.TempDir()
	docPath := filepath.Join(root, "go.md")
	require.NoError(t, os.WriteFile(docPath, []byte(rulesDoc), 0o600))

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(t.Context()))

	svc, err := mecp.New(store,
		mecp.WithClock(mecp.FixedClock{Time: testNow}),
		mecp.WithDocumentReader(source.NewDocumentReader([]string{root})),
	)
	require.NoError(t, err)
	return svc, docPath
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
		require.Equal(t, 2, res.ExistingCount)
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
