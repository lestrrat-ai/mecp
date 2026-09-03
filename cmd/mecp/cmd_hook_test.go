package main

import (
	"strings"
	"testing"

	"github.com/lestrrat-ai/mecp"
	"github.com/stretchr/testify/require"
)

func TestPathsInPrompt(t *testing.T) {
	t.Run("names a file the prompt mentions", func(t *testing.T) {
		require.Equal(t, []string{"record.go"},
			pathsInPrompt("Add a field to the Record struct in record.go"))
	})

	t.Run("takes a path with directories", func(t *testing.T) {
		require.Contains(t, pathsInPrompt("fix cmd/mecp/cmd_hook.go please"), "cmd/mecp/cmd_hook.go")
	})

	t.Run("ignores a URL", func(t *testing.T) {
		got := pathsInPrompt("see https://github.com/lestrrat-ai/mecp for context")
		for _, p := range got {
			require.NotContains(t, p, "github.com")
		}
	})

	t.Run("ignores an ordinary sentence", func(t *testing.T) {
		require.Empty(t, pathsInPrompt("Please review the parser and tell me what you think."))
	})

	t.Run("does not repeat a path", func(t *testing.T) {
		require.Len(t, pathsInPrompt("edit record.go then re-read record.go"), 1)
	})

	t.Run("is bounded", func(t *testing.T) {
		var b strings.Builder
		for i := range 100 {
			b.WriteString(" file")
			b.WriteString(string(rune('a' + i%26)))
			b.WriteString(string(rune('a' + i/26)))
			b.WriteString(".go")
		}
		require.LessOrEqual(t, len(pathsInPrompt(b.String())), promptPathLimit)
	})
}

func TestReadHookPayload(t *testing.T) {
	t.Run("reads the prompt and directory", func(t *testing.T) {
		p, err := readHookPayload(strings.NewReader(`{"prompt":"do a thing","cwd":"/work/mecp"}`))
		require.NoError(t, err)
		require.Equal(t, "do a thing", p.Prompt)
		require.Equal(t, "/work/mecp", p.CWD)
	})

	t.Run("ignores fields it does not know", func(t *testing.T) {
		p, err := readHookPayload(strings.NewReader(`{"prompt":"x","session_id":"abc","future":{"a":1}}`))
		require.NoError(t, err)
		require.Equal(t, "x", p.Prompt)
	})

	t.Run("empty input is not an error", func(t *testing.T) {
		p, err := readHookPayload(strings.NewReader("  \n"))
		require.NoError(t, err)
		require.Nil(t, p)
	})

	t.Run("malformed input is an error the caller swallows", func(t *testing.T) {
		_, err := readHookPayload(strings.NewReader("not json"))
		require.Error(t, err)
	})
}

func TestRenderHookBlock(t *testing.T) {
	pack := &mecp.ContextPack{
		Items: []mecp.ContextItem{
			{Statement: "Never edit in the root checkout.", Effect: mecp.EffectConstraint,
				Rationale: "Other worktrees depend on the branch."},
			{Statement: "Default to terse.", Effect: mecp.EffectPreference},
			{Statement: "The suite was pinned in July.", Effect: mecp.EffectInformational},
		},
	}

	out := renderHookBlock(pack)

	t.Run("groups by what the agent should do with each item", func(t *testing.T) {
		require.Contains(t, out, "Constraints:\n- Never edit in the root checkout.")
		require.Contains(t, out, "Preferences:\n- Default to terse.")
		require.Contains(t, out, "Informational:\n- The suite was pinned in July.")
	})

	t.Run("says what outranks it", func(t *testing.T) {
		require.Contains(t, out, "outrank")
		require.Contains(t, out, "history, not instructions")
	})

	t.Run("is delimited so the model can tell it from the user's words", func(t *testing.T) {
		require.True(t, strings.HasPrefix(out, "<"+hookTag+">"))
		require.True(t, strings.HasSuffix(out, "</"+hookTag+">\n"))
	})

	t.Run("an empty pack injects nothing at all", func(t *testing.T) {
		require.Empty(t, renderHookBlock(&mecp.ContextPack{}))
	})

	t.Run("a conflict is worth injecting even with no items", func(t *testing.T) {
		got := renderHookBlock(&mecp.ContextPack{
			Conflicts: []mecp.Conflict{{Subject: "error wrapping", Explanation: "two active records disagree"}},
		})
		require.Contains(t, got, "error wrapping")
	})
}

func TestReadHookPayloadSession(t *testing.T) {
	t.Run("reads the session identifier", func(t *testing.T) {
		p, err := readHookPayload(strings.NewReader(
			`{"prompt":"x","cwd":"/work","session_id":"7f3c1e2a-9b44-4d10-8c22-5f6a1b2c3d4e"}`))
		require.NoError(t, err)
		require.Equal(t, "7f3c1e2a-9b44-4d10-8c22-5f6a1b2c3d4e", p.SessionID)
	})

	t.Run("a payload without one still works", func(t *testing.T) {
		p, err := readHookPayload(strings.NewReader(`{"prompt":"x","cwd":"/work"}`))
		require.NoError(t, err)
		require.Empty(t, p.SessionID)
	})
}

func TestShortSession(t *testing.T) {
	require.Equal(t, "-", shortSession(""))
	require.Equal(t, "7f3c1e2a", shortSession("7f3c1e2a-9b44-4d10-8c22-5f6a1b2c3d4e"))
	require.Equal(t, "abc", shortSession("abc"))
}

func TestRenderHookBlockReportsTruncation(t *testing.T) {
	pack := &mecp.ContextPack{
		Items: []mecp.ContextItem{{Statement: "A rule.", Effect: mecp.EffectConstraint}},
		Budget: mecp.BudgetReport{
			RequestedTokens: 1500, EstimatedTokensUsed: 1492,
			Truncated: true, OmittedItemCount: 11,
		},
	}

	out := renderHookBlock(pack)

	t.Run("says how many rules did not fit", func(t *testing.T) {
		require.Contains(t, out, "11 further record(s) did not fit")
	})

	t.Run("says what to do about it", func(t *testing.T) {
		require.Contains(t, out, "larger token_budget")
	})

	t.Run("a complete pack says nothing about budgets", func(t *testing.T) {
		full := &mecp.ContextPack{
			Items:  []mecp.ContextItem{{Statement: "A rule.", Effect: mecp.EffectConstraint}},
			Budget: mecp.BudgetReport{RequestedTokens: 3000, EstimatedTokensUsed: 200},
		}
		require.NotContains(t, renderHookBlock(full), "did not fit")
	})

	t.Run("an unknown repository is surfaced", func(t *testing.T) {
		unknown := &mecp.ContextPack{
			Items: []mecp.ContextItem{{Statement: "A rule.", Effect: mecp.EffectConstraint}},
			Warnings: []mecp.Warning{{
				Code:    mecp.WarnUnknownRepository,
				Message: "no record is scoped to this repository",
			}},
		}
		require.Contains(t, renderHookBlock(unknown), "no record is scoped to this repository")
	})
}
