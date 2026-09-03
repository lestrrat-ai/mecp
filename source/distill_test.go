package source_test

import (
	"path/filepath"
	"testing"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/source"
	"github.com/stretchr/testify/require"
)

// sampleDoc is shaped like a real instruction file: a checklist, a routing
// table, a fenced example, a numbered procedure, and prose between them.
const sampleDoc = "# Before ANY Task\n" +
	"\n" +
	"Verify BEFORE your first edit:\n" +
	"\n" +
	"- [ ] NEVER edit in the root checkout\n" +
	"- [ ] Read the applicable docs\n" +
	"\n" +
	"# Pre-Read Rules\n" +
	"\n" +
	"| Area | Trigger | Doc |\n" +
	"|------|---------|-----|\n" +
	"| Go | Writing Go code | `docs/go.md` |\n" +
	"| Shell | Using Bash | `docs/shell.md` |\n" +
	"\n" +
	"## Style\n" +
	"\n" +
	"This paragraph explains the reasoning and is not itself a rule.\n" +
	"It continues onto a second line.\n" +
	"\n" +
	"- Do not use named return values.\n" +
	"- Prefer early returns from functions.\n" +
	"  - This continuation belongs to the bullet above.\n" +
	"\n" +
	"```go\n" +
	"- this bullet is inside a fence and must be ignored\n" +
	"```\n" +
	"\n" +
	"## Procedure\n" +
	"\n" +
	"1. Stash the changes.\n" +
	"2. Create the worktree.\n"

func writeDoc(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	return writeFile(t, filepath.Dir(path), "CLAUDE.md", body)
}

func TestDistill(t *testing.T) {
	path := writeDoc(t, sampleDoc)
	d := source.NewDistiller("lestrrat")
	d.Now = testTime()

	res, err := d.Distill(path)
	require.NoError(t, err)

	statements := make([]string, 0, len(res.Records))
	for _, rec := range res.Records {
		statements = append(statements, rec.Statement)
	}

	t.Run("a bullet becomes a rule and a checkbox is stripped", func(t *testing.T) {
		require.Contains(t, statements, "NEVER edit in the root checkout.")
		require.Contains(t, statements, "Do not use named return values.")
	})

	t.Run("a numbered step is a rule too", func(t *testing.T) {
		require.Contains(t, statements, "Stash the changes.")
		require.Contains(t, statements, "Create the worktree.")
	})

	t.Run("a table row becomes a rule keyed by its first column", func(t *testing.T) {
		require.Contains(t, statements, "Go: Writing Go code — `docs/go.md`.")
	})

	t.Run("the table header is not a rule", func(t *testing.T) {
		for _, s := range statements {
			require.NotContains(t, s, "Trigger — Doc")
		}
	})

	t.Run("a fenced block is ignored", func(t *testing.T) {
		for _, s := range statements {
			require.NotContains(t, s, "inside a fence")
		}
	})

	t.Run("an indented bullet joins the one above it", func(t *testing.T) {
		var found bool
		for _, s := range statements {
			if s == "Prefer early returns from functions. This continuation belongs to the bullet above." {
				found = true
			}
		}
		require.True(t, found, "got %v", statements)
	})

	t.Run("prose is counted and skipped rather than guessed at", func(t *testing.T) {
		require.Positive(t, res.SkippedParagraphs)
		require.Len(t, res.SkippedLines, res.SkippedParagraphs)
		for _, s := range statements {
			require.NotContains(t, s, "explains the reasoning")
		}
	})

	t.Run("a multi-line paragraph counts once", func(t *testing.T) {
		// The sample holds two prose blocks: the checklist preamble and the
		// two-line style paragraph.
		require.Equal(t, 2, res.SkippedParagraphs)
	})

	t.Run("the nearest heading becomes the subject", func(t *testing.T) {
		subjects := map[string]string{}
		for _, rec := range res.Records {
			subjects[rec.Statement] = rec.Subject
		}
		require.Equal(t, "style", subjects["Do not use named return values."])
		require.Equal(t, "procedure", subjects["Stash the changes."])
		require.Equal(t, "before any task", subjects["NEVER edit in the root checkout."])
	})
}

func TestDistillKindInference(t *testing.T) {
	path := writeDoc(t, "# Rules\n\n"+
		"- NEVER do this.\n"+
		"- Do not do that.\n"+
		"- Only use require, not assert.\n"+
		"- Avoid grab-bag filenames.\n"+
		"- Prefer interfaces over callbacks.\n"+
		"- The parser always runs before the linter.\n")

	d := source.NewDistiller("lestrrat")
	d.Now = testTime()
	res, err := d.Distill(path)
	require.NoError(t, err)

	kinds := map[string]mecp.RecordKind{}
	for _, rec := range res.Records {
		kinds[rec.Statement] = rec.Kind
	}

	require.Equal(t, mecp.KindConstraint, kinds["NEVER do this."])
	require.Equal(t, mecp.KindConstraint, kinds["Do not do that."])
	require.Equal(t, mecp.KindConstraint, kinds["Only use require, not assert."])
	require.Equal(t, mecp.KindConstraint, kinds["Avoid grab-bag filenames."])
	require.Equal(t, mecp.KindPreference, kinds["Prefer interfaces over callbacks."])

	// "always" in the middle of a sentence describes rather than instructs, so
	// lowercase matching must not promote it.
	require.Equal(t, mecp.KindPreference, kinds["The parser always runs before the linter."])
}

func TestDistillProvenance(t *testing.T) {
	path := writeDoc(t, "# Style\n\n- Do not use named return values.\n")

	d := source.NewDistiller("lestrrat")
	d.Now = testTime()
	d.Authority = mecp.AuthorityUser
	d.Scope = mecp.Scope{PathPatterns: []string{"*.go"}}

	res, err := d.Distill(path)
	require.NoError(t, err)
	require.Len(t, res.Records, 1)
	rec := res.Records[0]

	t.Run("the scope is applied to every record", func(t *testing.T) {
		require.Equal(t, []string{"*.go"}, rec.Scope.PathPatterns)
		require.Equal(t, "lestrrat", rec.Scope.User)
	})

	t.Run("the authority is what the caller claimed", func(t *testing.T) {
		require.Equal(t, mecp.AuthorityUser, rec.Authority)
	})

	t.Run("the original line is kept verbatim with its number", func(t *testing.T) {
		require.Len(t, rec.Sources, 1)
		require.Contains(t, rec.Sources[0].ExactExcerpt, "line 3:")
		require.Contains(t, rec.Sources[0].ExactExcerpt, "Do not use named return values.")
	})

	t.Run("the source carries a hash so the record can go stale", func(t *testing.T) {
		require.Equal(t, mecp.ValidateFileAndHash, rec.ValidationPolicy)
		want, err := source.HashFile(path)
		require.NoError(t, err)
		require.Equal(t, want, rec.Sources[0].ContentHash)
	})

	t.Run("every record is valid and importable", func(t *testing.T) {
		require.NoError(t, rec.Validate())
	})
}

func TestDistillRoundTripsThroughImport(t *testing.T) {
	// What distill writes has to be what import reads, or the workflow breaks
	// at the handover.
	path := writeDoc(t, "# Style\n\n- NEVER use named return values.\n- Prefer early returns.\n")

	d := source.NewDistiller("lestrrat")
	d.Now = testTime()
	res, err := d.Distill(path)
	require.NoError(t, err)
	require.Len(t, res.Records, 2)

	ctx := t.Context()
	store := newStore(t)
	for _, rec := range res.Records {
		require.NoError(t, store.PutRecord(ctx, rec))
	}

	stored, err := store.QueryRecords(ctx, mecp.RecordQuery{})
	require.NoError(t, err)
	require.Len(t, stored, 2)
}
