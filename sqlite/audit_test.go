package sqlite_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/sqlite"
	"github.com/stretchr/testify/require"
)

func TestAuditEvents(t *testing.T) {
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	newAuditStore := func(t *testing.T, n int) *sqlite.Store {
		t.Helper()
		store := newStore(t)
		sink := sqlite.NewAuditSink(store)
		for i := range n {
			require.NoError(t, sink.Write(t.Context(), mecp.AuditEvent{
				At:          base.Add(time.Duration(i) * time.Hour),
				PrincipalID: "local-user",
				ClientID:    "default",
				Operation:   fmt.Sprintf("op-%d", i),
				ResultCount: i,
			}))
		}
		return store
	}

	t.Run("newest first, bounded by limit", func(t *testing.T) {
		store := newAuditStore(t, 5)

		events, err := store.AuditEvents(t.Context(), mecp.AuditQuery{Limit: 2})
		require.NoError(t, err)
		require.Len(t, events, 2)
		require.Equal(t, "op-4", events[0].Operation)
		require.Equal(t, "op-3", events[1].Operation)
	})

	t.Run("since drops older events and is inclusive", func(t *testing.T) {
		store := newAuditStore(t, 5)

		events, err := store.AuditEvents(t.Context(), mecp.AuditQuery{Since: base.Add(3 * time.Hour)})
		require.NoError(t, err)
		require.Len(t, events, 2)
		require.Equal(t, "op-4", events[0].Operation)
		require.Equal(t, "op-3", events[1].Operation)
	})

	t.Run("an empty table holds no events", func(t *testing.T) {
		events, err := newStore(t).AuditEvents(t.Context(), mecp.AuditQuery{})
		require.NoError(t, err)
		require.Empty(t, events)
	})
}
