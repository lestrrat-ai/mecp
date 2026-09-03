package mecp_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/stretchr/testify/require"
)

// writeAuditLog appends one event per hour, oldest first, and returns the path.
func writeAuditLog(t *testing.T, base time.Time, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit", "audit.jsonl")
	sink, err := mecp.NewJSONLAudit(path)
	require.NoError(t, err)
	for i := range n {
		require.NoError(t, sink.Write(t.Context(), mecp.AuditEvent{
			At:          base.Add(time.Duration(i) * time.Hour),
			PrincipalID: "local-user",
			ClientID:    "default",
			Operation:   fmt.Sprintf("op-%d", i),
			ResultCount: i,
		}))
	}
	return path
}

func TestJSONLAuditReader(t *testing.T) {
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	t.Run("newest first, bounded by limit", func(t *testing.T) {
		reader := mecp.NewJSONLAuditReader(writeAuditLog(t, base, 5))

		events, err := reader.AuditEvents(t.Context(), mecp.AuditQuery{Limit: 2})
		require.NoError(t, err)
		require.Len(t, events, 2)
		require.Equal(t, "op-4", events[0].Operation)
		require.Equal(t, "op-3", events[1].Operation)
		require.True(t, events[0].At.Equal(base.Add(4*time.Hour)))
	})

	t.Run("limit larger than the log returns everything", func(t *testing.T) {
		reader := mecp.NewJSONLAuditReader(writeAuditLog(t, base, 3))

		events, err := reader.AuditEvents(t.Context(), mecp.AuditQuery{Limit: 10})
		require.NoError(t, err)
		require.Len(t, events, 3)
		require.Equal(t, "op-2", events[0].Operation)
		require.Equal(t, "op-0", events[2].Operation)
	})

	t.Run("since drops older events and is inclusive", func(t *testing.T) {
		reader := mecp.NewJSONLAuditReader(writeAuditLog(t, base, 5))

		events, err := reader.AuditEvents(t.Context(), mecp.AuditQuery{Since: base.Add(3 * time.Hour)})
		require.NoError(t, err)
		require.Len(t, events, 2)
		require.Equal(t, "op-4", events[0].Operation)
		require.Equal(t, "op-3", events[1].Operation)
	})

	t.Run("since applies before limit", func(t *testing.T) {
		reader := mecp.NewJSONLAuditReader(writeAuditLog(t, base, 6))

		events, err := reader.AuditEvents(t.Context(), mecp.AuditQuery{Limit: 2, Since: base.Add(time.Hour)})
		require.NoError(t, err)
		require.Len(t, events, 2)
		require.Equal(t, "op-5", events[0].Operation)
		require.Equal(t, "op-4", events[1].Operation)
	})

	t.Run("a log that does not exist holds no events", func(t *testing.T) {
		reader := mecp.NewJSONLAuditReader(filepath.Join(t.TempDir(), "missing.jsonl"))

		events, err := reader.AuditEvents(t.Context(), mecp.AuditQuery{})
		require.NoError(t, err)
		require.Empty(t, events)
	})

	t.Run("reading does not create the log or its directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		path := filepath.Join(dir, "audit.jsonl")

		_, err := mecp.NewJSONLAuditReader(path).AuditEvents(t.Context(), mecp.AuditQuery{})
		require.NoError(t, err)
		_, err = os.Stat(dir)
		require.True(t, os.IsNotExist(err))
	})

	t.Run("blank lines are skipped", func(t *testing.T) {
		path := writeAuditLog(t, base, 2)
		require.NoError(t, appendLine(path, "\n   \n"))

		events, err := mecp.NewJSONLAuditReader(path).AuditEvents(t.Context(), mecp.AuditQuery{})
		require.NoError(t, err)
		require.Len(t, events, 2)
	})

	t.Run("a line that does not decode is an error naming the line", func(t *testing.T) {
		path := writeAuditLog(t, base, 2)
		require.NoError(t, appendLine(path, "{\"at\": truncated\n"))

		_, err := mecp.NewJSONLAuditReader(path).AuditEvents(t.Context(), mecp.AuditQuery{})
		require.Error(t, err)
		require.Equal(t, mecp.CodeStorage, mecp.CodeOf(err))
		require.Contains(t, err.Error(), "line 3")
	})

	t.Run("a cancelled context stops the read", func(t *testing.T) {
		reader := mecp.NewJSONLAuditReader(writeAuditLog(t, base, 3))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := reader.AuditEvents(ctx, mecp.AuditQuery{})
		require.ErrorIs(t, err, context.Canceled)
	})
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}
