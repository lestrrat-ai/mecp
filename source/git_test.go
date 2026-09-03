package source_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/source"
	"github.com/stretchr/testify/require"
)

// initRepo builds a two-commit repository so that ancestry can be tested in
// both directions.
func initRepo(t *testing.T) (dir, first, second string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")
	writeFile(t, dir, "docs/adr.md", "The suite is pinned to a controlled commit.\n")
	run("add", ".")
	run("commit", "-m", "first")
	first = run("rev-parse", "HEAD")

	writeFile(t, dir, "docs/second.md", "More documentation.\n")
	run("add", ".")
	run("commit", "-m", "second")
	second = run("rev-parse", "HEAD")
	return dir, first, second
}

func TestGitResolver(t *testing.T) {
	dir, first, second := initRepo(t)
	resolver := source.NewGitResolver()
	ws := mecp.Workspace{RootURI: "file://" + dir, Revision: second}

	t.Run("finds a file that is still present", func(t *testing.T) {
		ok, err := resolver.Exists(t.Context(), mecp.Source{
			Type: mecp.SourceADR, Locator: "docs/adr.md",
		}, ws)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("reports a file that is gone", func(t *testing.T) {
		ok, err := resolver.Exists(t.Context(), mecp.Source{
			Type: mecp.SourceFile, Locator: "docs/removed.md",
		}, ws)
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("refuses a locator that escapes the workspace", func(t *testing.T) {
		_, err := resolver.Exists(t.Context(), mecp.Source{
			Type: mecp.SourceFile, Locator: "../../etc/passwd",
		}, ws)
		require.Error(t, err)
		require.Contains(t, err.Error(), "escapes")
	})

	t.Run("cannot verify a conversation, and says so", func(t *testing.T) {
		_, err := resolver.Exists(t.Context(), mecp.Source{
			Type: mecp.SourceConversation, Locator: "turn://42",
		}, ws)
		require.ErrorIs(t, err, source.ErrUnverifiable)
	})

	t.Run("hashes a file's current content", func(t *testing.T) {
		got, err := resolver.ContentHash(t.Context(), mecp.Source{
			Type: mecp.SourceADR, Locator: "docs/adr.md",
		}, ws)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(got, "sha256:"))
	})

	t.Run("an earlier commit applies to a later one", func(t *testing.T) {
		ok, err := resolver.RevisionApplies(t.Context(), mecp.Source{
			Type: mecp.SourceCommit, Revision: first,
		}, ws)
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("a later commit does not apply to an earlier one", func(t *testing.T) {
		ok, err := resolver.RevisionApplies(t.Context(), mecp.Source{
			Type: mecp.SourceCommit, Revision: second,
		}, mecp.Workspace{RootURI: "file://" + dir, Revision: first})
		require.NoError(t, err)
		require.False(t, ok)
	})
}

func TestValidationAgainstGit(t *testing.T) {
	dir, first, second := initRepo(t)
	validator := mecp.NewValidator(source.NewGitResolver())
	ws := mecp.Workspace{RootURI: "file://" + dir, Revision: second}
	now := testTime()

	t.Run("content hash validation passes while the file is unchanged", func(t *testing.T) {
		hash, err := source.HashFile(dir + "/docs/adr.md")
		require.NoError(t, err)

		rec := &mecp.Record{
			ID: "rec_hash", ValidationPolicy: mecp.ValidateContentHash,
			Sources: []mecp.Source{{ID: "src", Type: mecp.SourceADR, Locator: "docs/adr.md", ContentHash: hash}},
		}
		require.Equal(t, mecp.ValidationValid, validator.Validate(t.Context(), rec, ws, now).State)
	})

	t.Run("content hash validation goes stale once the file changes", func(t *testing.T) {
		rec := &mecp.Record{
			ID: "rec_hash_stale", ValidationPolicy: mecp.ValidateContentHash,
			Sources: []mecp.Source{{
				ID: "src", Type: mecp.SourceADR, Locator: "docs/adr.md",
				ContentHash: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}},
		}
		status := validator.Validate(t.Context(), rec, ws, now)
		require.Equal(t, mecp.ValidationStale, status.State)
		require.Contains(t, status.Reason, "content changed")
	})

	t.Run("a record from an unrelated revision is stale", func(t *testing.T) {
		rec := &mecp.Record{
			ID: "rec_rev", ValidationPolicy: mecp.ValidateGitAncestor,
			Sources: []mecp.Source{{ID: "src", Type: mecp.SourceCommit, Revision: second}},
		}
		status := validator.Validate(t.Context(), rec, mecp.Workspace{RootURI: "file://" + dir, Revision: first}, now)
		require.Equal(t, mecp.ValidationStale, status.State)
	})

	t.Run("without a resolver the same policy is unverified, not failed", func(t *testing.T) {
		rec := &mecp.Record{
			ID: "rec_rev2", ValidationPolicy: mecp.ValidateGitAncestor,
			Sources: []mecp.Source{{ID: "src", Type: mecp.SourceCommit, Revision: first}},
		}
		status := mecp.NewValidator(nil).Validate(t.Context(), rec, ws, now)
		require.Equal(t, mecp.ValidationUnverified, status.State)
	})
}
