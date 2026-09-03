package mecp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/source"
	"github.com/stretchr/testify/require"
)

func TestQuotePresentValidation(t *testing.T) {
	const doc = "# Shell\n\n- Never use /tmp.\n- Prefer early returns.\n"

	root := t.TempDir()
	path := filepath.Join(root, "shell.md")
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	validator := mecp.NewValidator(source.NewGitResolver(source.WithAllowedRoots([]string{root})))
	rec := &mecp.Record{
		ID: "rec_x", ValidationPolicy: mecp.ValidateQuotePresent,
		Sources: []mecp.Source{{
			ID: "src", Type: mecp.SourceFile, Locator: "file://" + path,
			ExactExcerpt: "line 3: - Never use /tmp.",
		}},
	}

	t.Run("fresh while its own line is there", func(t *testing.T) {
		st := validator.Validate(t.Context(), rec, mecp.Workspace{}, testNow)
		require.Equal(t, mecp.ValidationValid, st.State)
	})

	t.Run("editing a different rule leaves it alone", func(t *testing.T) {
		edited := "# Shell\n\n- Never use /tmp.\n- Prefer early returns from every function.\n"
		require.NoError(t, os.WriteFile(path, []byte(edited), 0o600))

		st := mecp.NewValidator(source.NewGitResolver(source.WithAllowedRoots([]string{root}))).
			Validate(t.Context(), rec, mecp.Workspace{}, testNow)
		require.Equal(t, mecp.ValidationValid, st.State,
			"one rule changing must not demote every other rule in the same document")
	})

	t.Run("re-indenting or re-marking the line does not break it", func(t *testing.T) {
		edited := "# Shell\n\n  * Never use /tmp.\n- Prefer early returns.\n"
		require.NoError(t, os.WriteFile(path, []byte(edited), 0o600))

		st := mecp.NewValidator(source.NewGitResolver(source.WithAllowedRoots([]string{root}))).
			Validate(t.Context(), rec, mecp.Workspace{}, testNow)
		require.Equal(t, mecp.ValidationValid, st.State)
	})

	t.Run("stale once its own line is gone", func(t *testing.T) {
		edited := "# Shell\n\n- Prefer early returns.\n"
		require.NoError(t, os.WriteFile(path, []byte(edited), 0o600))

		st := mecp.NewValidator(source.NewGitResolver(source.WithAllowedRoots([]string{root}))).
			Validate(t.Context(), rec, mecp.Workspace{}, testNow)
		require.Equal(t, mecp.ValidationStale, st.State)
	})

	t.Run("stale once the file is gone", func(t *testing.T) {
		require.NoError(t, os.Remove(path))

		st := mecp.NewValidator(source.NewGitResolver(source.WithAllowedRoots([]string{root}))).
			Validate(t.Context(), rec, mecp.Workspace{}, testNow)
		require.Equal(t, mecp.ValidationStale, st.State)
	})
}
