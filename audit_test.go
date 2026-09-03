package mecp_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/sqlite"
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

	t.Run("an origin survives the round trip", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "audit.jsonl")
		sink, err := mecp.NewJSONLAudit(path)
		require.NoError(t, err)
		require.NoError(t, sink.Write(t.Context(), mecp.AuditEvent{
			At: base, PrincipalID: "local-user", ClientID: "claude-code",
			Origin: mecp.OriginMCP, Operation: "prepare_task",
		}))

		events, err := mecp.NewJSONLAuditReader(path).AuditEvents(t.Context(), mecp.AuditQuery{})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, mecp.OriginMCP, events[0].Origin)
	})

	t.Run("a line written before origins were recorded still reads back", func(t *testing.T) {
		path := writeAuditLog(t, base, 1)
		require.NoError(t, appendLine(path,
			`{"at":"2026-09-03T09:00:00Z","principal_id":"local-user","client_id":"claude-code",`+
				`"operation":"prepare_task","result_count":1}`+"\n"))

		events, err := mecp.NewJSONLAuditReader(path).AuditEvents(t.Context(), mecp.AuditQuery{})
		require.NoError(t, err)
		require.Len(t, events, 2)
		require.Equal(t, "prepare_task", events[0].Operation)
		require.Empty(t, events[0].Origin)
		require.Equal(t, "unknown", events[0].Origin.String())
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

// recordingAudit keeps every event a service wrote, so a test can assert what
// the trail says about a call.
type recordingAudit struct {
	mu     sync.Mutex
	events []mecp.AuditEvent
}

func (a *recordingAudit) Write(_ context.Context, ev mecp.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

func (a *recordingAudit) all() []mecp.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]mecp.AuditEvent(nil), a.events...)
}

// auditedService builds a service whose audit trail the test reads back.
func auditedService(t *testing.T, records ...*mecp.Record) (mecp.Service, *recordingAudit) {
	t.Helper()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(t.Context()))

	for _, rec := range records {
		rec.Normalize(testNow.Add(-24 * time.Hour))
		require.NoError(t, rec.Validate())
		require.NoError(t, store.PutRecord(t.Context(), rec))
	}

	sink := &recordingAudit{}
	svc, err := mecp.New(store,
		mecp.WithClock(mecp.FixedClock{Time: testNow}), mecp.WithAuditSink(sink))
	require.NoError(t, err)
	return svc, sink
}

func TestAuditOrigin(t *testing.T) {
	prepare := func(t *testing.T, svc mecp.Service, caller mecp.Caller) error {
		t.Helper()
		_, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:    caller,
			Task:      "Review the parser",
			TaskKind:  mecp.TaskCodeReview,
			Workspace: heliumWorkspace(),
		})
		return err
	}

	t.Run("a diagnostic CLI run is distinguishable from the agent's own call", func(t *testing.T) {
		svc, sink := auditedService(t, stylesheetConstraint())

		require.NoError(t, prepare(t, svc, agentCaller().WithOrigin(mecp.OriginMCP)))
		require.NoError(t, prepare(t, svc, agentCaller().WithOrigin(mecp.OriginCLI)))

		events := sink.all()
		require.Len(t, events, 2)
		// The client profile is identical, which is the whole problem the
		// origin solves.
		require.Equal(t, "claude-code", events[0].ClientID)
		require.Equal(t, "claude-code", events[1].ClientID)
		require.Equal(t, mecp.OriginMCP, events[0].Origin)
		require.Equal(t, mecp.OriginCLI, events[1].Origin)
	})

	t.Run("a search records the origin", func(t *testing.T) {
		svc, sink := auditedService(t, stylesheetConstraint())

		_, err := svc.Search(t.Context(), mecp.SearchRequest{
			Caller:    agentCaller().WithOrigin(mecp.OriginCLI),
			Query:     "stylesheets",
			Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)

		events := sink.all()
		require.Len(t, events, 1)
		require.Equal(t, mecp.OriginCLI, events[0].Origin)
	})

	t.Run("a refused call records the origin too", func(t *testing.T) {
		svc, sink := auditedService(t, stylesheetConstraint())

		caller := agentCaller().WithOrigin(mecp.OriginMCP)
		caller.AllowedRepositories = []string{"https://github.com/example/billing"}
		err := prepare(t, svc, caller)
		require.Error(t, err)
		require.Equal(t, mecp.CodeUnauthorizedScope, mecp.CodeOf(err))

		events := sink.all()
		require.Len(t, events, 1)
		require.Equal(t, mecp.OriginMCP, events[0].Origin)
		require.Equal(t, mecp.CodeUnauthorizedScope, events[0].ErrorCode)
	})

	t.Run("a caller built without an origin audits as unknown", func(t *testing.T) {
		svc, sink := auditedService(t, stylesheetConstraint())

		require.NoError(t, prepare(t, svc, agentCaller()))

		events := sink.all()
		require.Len(t, events, 1)
		require.Empty(t, events[0].Origin)
		require.Equal(t, "unknown", events[0].Origin.String())
	})
}
