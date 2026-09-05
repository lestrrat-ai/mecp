package sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-go/rasql/query"
)

// AuditSink writes audit events into the database. It is an alternative to
// mecp.JSONLAudit for deployments that would rather keep everything in one
// file; a read-only store rejects writes, so an agent-facing process that opens
// the database read-only must use the JSONL sink instead.
type AuditSink struct {
	store *Store
}

// NewAuditSink returns a sink backed by the audit_events table.
func NewAuditSink(store *Store) *AuditSink { return &AuditSink{store: store} }

func (a *AuditSink) Write(ctx context.Context, ev mecp.AuditEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf(`failed to encode audit event: %w`, err)
	}
	// id is left out so SQLite assigns the next autoincrement value.
	insert, err := query.NewInsert(auditEventsTable,
		query.Set(auditEventsTable.Column("at"), formatTime(ev.At)),
		query.Set(auditEventsTable.Column("principal_id"), ev.PrincipalID),
		query.Set(auditEventsTable.Column("client_id"), ev.ClientID),
		query.Set(auditEventsTable.Column("operation"), ev.Operation),
		query.Set(auditEventsTable.Column("payload"), string(payload)),
	)
	if err != nil {
		return fmt.Errorf(`failed to build the audit insert: %w`, err)
	}
	if _, err := execWrite(ctx, a.store.db, insert); err != nil {
		return fmt.Errorf(`failed to write audit event: %w`, err)
	}
	return nil
}

// AuditEvents returns the most recent matching audit events, newest first.
func (s *Store) AuditEvents(ctx context.Context, q mecp.AuditQuery) ([]mecp.AuditEvent, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = mecp.DefaultAuditLimit
	}

	var where query.Expression
	if !q.Since.IsZero() {
		where = query.GreaterThanOrEqual(auditEventsTable.Column("at"), formatTime(q.Since))
	}
	statement, err := selectWhere(auditEventsTable, where, auditEventsTable.Column("payload"))
	if err != nil {
		return nil, fmt.Errorf(`failed to build the audit query: %w`, err)
	}
	if statement, err = statement.WithOrder(query.Desc(auditEventsTable.Column("id"))); err != nil {
		return nil, fmt.Errorf(`failed to order the audit query: %w`, err)
	}
	if statement, err = statement.WithLimit(limit); err != nil {
		return nil, fmt.Errorf(`failed to limit the audit query: %w`, err)
	}

	rows, err := querySelect(ctx, s.db, statement)
	if err != nil {
		return nil, fmt.Errorf(`failed to read audit events: %w`, err)
	}
	defer rows.Close()

	var out []mecp.AuditEvent
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var ev mecp.AuditEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil, fmt.Errorf(`failed to decode audit event: %w`, err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

var (
	_ mecp.AuditSink   = (*AuditSink)(nil)
	_ mecp.AuditReader = (*Store)(nil)
)
