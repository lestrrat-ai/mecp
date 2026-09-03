package sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lestrrat-ai/mecp"
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
	_, err = a.store.db.ExecContext(ctx,
		`INSERT INTO audit_events (at, principal_id, client_id, operation, payload) VALUES (?,?,?,?,?)`,
		formatTime(ev.At), ev.PrincipalID, ev.ClientID, ev.Operation, string(payload))
	if err != nil {
		return fmt.Errorf(`failed to write audit event: %w`, err)
	}
	return nil
}

// AuditEvents returns the most recent audit events, newest first.
func (s *Store) AuditEvents(ctx context.Context, limit int) ([]mecp.AuditEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
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

var _ mecp.AuditSink = (*AuditSink)(nil)
