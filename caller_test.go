package mecp_test

import (
	"testing"

	"github.com/lestrrat-ai/mecp"
	"github.com/stretchr/testify/require"
)

func TestOrigin(t *testing.T) {
	t.Run("a known origin names itself", func(t *testing.T) {
		require.True(t, mecp.OriginMCP.Valid())
		require.True(t, mecp.OriginCLI.Valid())
		require.Equal(t, "mcp", mecp.OriginMCP.String())
		require.Equal(t, "cli", mecp.OriginCLI.String())
	})

	t.Run("an absent origin reads as unknown rather than as an interface", func(t *testing.T) {
		var origin mecp.Origin
		require.False(t, origin.Valid())
		require.Equal(t, "unknown", origin.String())
	})
}

func TestCallerOrigin(t *testing.T) {
	t.Run("WithOrigin leaves the original caller alone", func(t *testing.T) {
		caller := agentCaller()
		stamped := caller.WithOrigin(mecp.OriginMCP)

		require.Equal(t, mecp.OriginMCP, stamped.Origin)
		require.Empty(t, caller.Origin)
		require.Equal(t, caller.ClientID, stamped.ClientID)
	})

	t.Run("an unset origin is accepted, because an embedder may predate it", func(t *testing.T) {
		require.NoError(t, agentCaller().Validate())
	})

	t.Run("a misspelled origin is refused", func(t *testing.T) {
		err := agentCaller().WithOrigin(mecp.Origin("mpc")).Validate()
		require.Error(t, err)
		require.Equal(t, mecp.CodeUnauthorizedScope, mecp.CodeOf(err))
		require.Contains(t, err.Error(), `"mpc"`)
	})
}
