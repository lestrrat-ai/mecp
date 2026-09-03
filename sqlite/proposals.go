package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lestrrat-ai/mecp"
)

const proposalColumns = `
	id, proposal_key, status, principal_id, client_id, kind, subject, statement, rationale,
	scope, tags, supersedes_record_ids, created_at, decided_at, decided_by, decision_note, result_record_id`

// PutProposal stores a proposal. The proposal key provides idempotency: an
// agent that retries a tool call gets the existing proposal back instead of
// filling the review queue with duplicates.
func (s *Store) PutProposal(ctx context.Context, p *mecp.Proposal) (*mecp.Proposal, bool, error) {
	existing, err := s.proposalByKey(ctx, p.Key)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	scope, err := json.Marshal(p.Scope)
	if err != nil {
		return nil, false, err
	}
	tags, err := json.Marshal(orEmpty(p.Tags))
	if err != nil {
		return nil, false, err
	}
	supersedes, err := json.Marshal(orEmpty(p.SupersedesRecordIDs))
	if err != nil {
		return nil, false, err
	}

	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO proposals (
				id, proposal_key, status, principal_id, client_id, kind, subject, statement, rationale,
				scope, tags, supersedes_record_ids, created_at, decided_at, decided_by, decision_note, result_record_id
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, p.Key, string(p.Status), p.PrincipalID, p.ClientID, string(p.Kind), p.Subject,
			p.Statement, p.Rationale, string(scope), string(tags), string(supersedes),
			formatTime(p.CreatedAt), formatTimePtr(p.DecidedAt), p.DecidedBy, p.DecisionNote, p.ResultRecordID,
		); err != nil {
			return fmt.Errorf(`failed to insert proposal: %w`, err)
		}
		for i, ev := range p.Evidence {
			payload, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO proposal_sources (proposal_id, position, payload) VALUES (?,?,?)`,
				p.ID, i, string(payload)); err != nil {
				return fmt.Errorf(`failed to insert proposal evidence: %w`, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return p, true, nil
}

func (s *Store) proposalByKey(ctx context.Context, key string) (*mecp.Proposal, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE proposal_key = ?`, key)
	p, err := scanProposal(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if err := s.hydrateProposals(ctx, []*mecp.Proposal{p}); err != nil {
		return nil, err
	}
	return p, nil
}

// GetProposal returns one proposal by ID.
func (s *Store) GetProposal(ctx context.Context, id string) (*mecp.Proposal, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, id)
	p, err := scanProposal(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, mecp.ErrNotFound
		}
		return nil, err
	}
	if err := s.hydrateProposals(ctx, []*mecp.Proposal{p}); err != nil {
		return nil, err
	}
	return p, nil
}

// UpdateProposal writes back a reviewed proposal. Evidence is immutable once
// filed, so only the review fields are updated.
func (s *Store) UpdateProposal(ctx context.Context, p *mecp.Proposal) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE proposals SET status = ?, decided_at = ?, decided_by = ?, decision_note = ?, result_record_id = ?
			WHERE id = ?`,
			string(p.Status), formatTimePtr(p.DecidedAt), p.DecidedBy, p.DecisionNote, p.ResultRecordID, p.ID)
		if err != nil {
			return fmt.Errorf(`failed to update proposal %s: %w`, p.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return mecp.ErrNotFound
		}
		return nil
	})
}

// QueryProposals lists proposals matching a filter, newest first.
func (s *Store) QueryProposals(ctx context.Context, q mecp.ProposalQuery) ([]*mecp.Proposal, error) {
	where := []string{"1=1"}
	var args []any

	if q.PrincipalID != "" {
		where = append(where, `principal_id = ?`)
		args = append(args, q.PrincipalID)
	}
	if len(q.Statuses) > 0 {
		where = append(where, `status IN (`+placeholders(len(q.Statuses))+`)`)
		args = append(args, toAny(q.Statuses)...)
	}

	query := `SELECT ` + proposalColumns + ` FROM proposals WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at DESC, id DESC`
	if q.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, q.Limit)
		if q.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, q.Offset)
		}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(`failed to query proposals: %w`, err)
	}
	defer rows.Close()

	var out []*mecp.Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.hydrateProposals(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func scanProposal(row rowScanner) (*mecp.Proposal, error) {
	var (
		p                       mecp.Proposal
		status, kind            string
		scope, tags, supersedes string
		createdAt               string
		decidedAt               sql.NullString
	)
	if err := row.Scan(&p.ID, &p.Key, &status, &p.PrincipalID, &p.ClientID, &kind, &p.Subject,
		&p.Statement, &p.Rationale, &scope, &tags, &supersedes, &createdAt, &decidedAt,
		&p.DecidedBy, &p.DecisionNote, &p.ResultRecordID); err != nil {
		return nil, err
	}

	p.Status = mecp.ProposalStatus(status)
	p.Kind = mecp.RecordKind(kind)

	var err error
	if p.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf(`proposal %s has an unreadable created_at: %w`, p.ID, err)
	}
	if p.DecidedAt, err = parseTimePtr(decidedAt); err != nil {
		return nil, fmt.Errorf(`proposal %s has an unreadable decided_at: %w`, p.ID, err)
	}
	if err := json.Unmarshal([]byte(scope), &p.Scope); err != nil {
		return nil, fmt.Errorf(`proposal %s has an unreadable scope: %w`, p.ID, err)
	}
	if err := json.Unmarshal([]byte(tags), &p.Tags); err != nil {
		return nil, fmt.Errorf(`proposal %s has unreadable tags: %w`, p.ID, err)
	}
	if err := json.Unmarshal([]byte(supersedes), &p.SupersedesRecordIDs); err != nil {
		return nil, fmt.Errorf(`proposal %s has unreadable supersession IDs: %w`, p.ID, err)
	}
	return &p, nil
}

// hydrateProposals loads evidence for a batch of proposals in one query.
func (s *Store) hydrateProposals(ctx context.Context, ps []*mecp.Proposal) error {
	if len(ps) == 0 {
		return nil
	}
	byID := make(map[string]*mecp.Proposal, len(ps))
	ids := make([]any, 0, len(ps))
	for _, p := range ps {
		byID[p.ID] = p
		ids = append(ids, p.ID)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT proposal_id, payload FROM proposal_sources WHERE proposal_id IN (`+placeholders(len(ids))+`) ORDER BY proposal_id, position`,
		ids...)
	if err != nil {
		return fmt.Errorf(`failed to load proposal evidence: %w`, err)
	}
	if err := scanInto(rows, func(rows *sql.Rows) error {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return err
		}
		var src mecp.Source
		if err := json.Unmarshal([]byte(payload), &src); err != nil {
			return fmt.Errorf(`proposal %s has unreadable evidence: %w`, id, err)
		}
		byID[id].Evidence = append(byID[id].Evidence, src)
		return nil
	}); err != nil {
		return err
	}
	return nil
}
