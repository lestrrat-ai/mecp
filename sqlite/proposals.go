package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-go/rasql/query"
)

// proposalProjections lists the columns a proposal is read from, in the order
// scanProposal expects them. The two lists are positional, so a column added
// here needs a destination added there.
func proposalProjections() []query.Projection {
	return []query.Projection{
		proposalID,
		proposalsTable.Column("proposal_key"),
		proposalsTable.Column("status"),
		proposalsTable.Column("principal_id"),
		proposalsTable.Column("client_id"),
		proposalsTable.Column("kind"),
		proposalsTable.Column("subject"),
		proposalsTable.Column("statement"),
		proposalsTable.Column("rationale"),
		proposalsTable.Column("scope"),
		proposalsTable.Column("tags"),
		proposalsTable.Column("supersedes_record_ids"),
		proposalsTable.Column("created_at"),
		proposalsTable.Column("decided_at"),
		proposalsTable.Column("decided_by"),
		proposalsTable.Column("decision_note"),
		proposalsTable.Column("result_record_id"),
	}
}

// oneProposal reads the single proposal matching where, reporting
// sql.ErrNoRows when there is none. Callers turn that into whichever answer
// suits them, so the two lookups below keep their differing behavior.
func (s *Store) oneProposal(ctx context.Context, where query.Expression) (*mecp.Proposal, error) {
	p, err := s.firstProposal(ctx, where)
	if err != nil {
		return nil, err
	}
	// Hydration runs only once firstProposal's rows are closed. A writable
	// store is capped at one connection, so a second query issued while the
	// first result set is still open waits for a connection that result set
	// holds, and neither ever finishes.
	if err := s.hydrateProposals(ctx, []*mecp.Proposal{p}); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) firstProposal(ctx context.Context, where query.Expression) (*mecp.Proposal, error) {
	statement, err := selectWhere(proposalsTable, where, proposalProjections()...)
	if err != nil {
		return nil, fmt.Errorf(`failed to build the proposal query: %w`, err)
	}
	rows, err := querySelect(ctx, s.db, statement)
	if err != nil {
		return nil, fmt.Errorf(`failed to query proposal: %w`, err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	p, err := scanProposal(rows)
	if err != nil {
		return nil, err
	}
	return p, rows.Err()
}

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
		insert, err := query.NewInsert(proposalsTable,
			query.Set(proposalID, p.ID),
			query.Set(proposalsTable.Column("proposal_key"), p.Key),
			query.Set(proposalsTable.Column("status"), string(p.Status)),
			query.Set(proposalsTable.Column("principal_id"), p.PrincipalID),
			query.Set(proposalsTable.Column("client_id"), p.ClientID),
			query.Set(proposalsTable.Column("kind"), string(p.Kind)),
			query.Set(proposalsTable.Column("subject"), p.Subject),
			query.Set(proposalsTable.Column("statement"), p.Statement),
			query.Set(proposalsTable.Column("rationale"), p.Rationale),
			query.Set(proposalsTable.Column("scope"), string(scope)),
			query.Set(proposalsTable.Column("tags"), string(tags)),
			query.Set(proposalsTable.Column("supersedes_record_ids"), string(supersedes)),
			query.Set(proposalsTable.Column("created_at"), formatTime(p.CreatedAt)),
			query.Set(proposalsTable.Column("decided_at"), formatTimePtr(p.DecidedAt)),
			query.Set(proposalsTable.Column("decided_by"), p.DecidedBy),
			query.Set(proposalsTable.Column("decision_note"), p.DecisionNote),
			query.Set(proposalsTable.Column("result_record_id"), p.ResultRecordID),
		)
		if err != nil {
			return fmt.Errorf(`failed to build the proposal insert: %w`, err)
		}
		if _, err := execWrite(ctx, tx, insert); err != nil {
			return fmt.Errorf(`failed to insert proposal: %w`, err)
		}
		for i, ev := range p.Evidence {
			payload, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			evidence, err := query.NewInsert(proposalSourcesTable,
				query.Set(proposalEvidenceID, p.ID),
				query.Set(proposalSourcesTable.Column("position"), i),
				query.Set(proposalSourcesTable.Column("payload"), string(payload)),
			)
			if err != nil {
				return fmt.Errorf(`failed to build the proposal evidence insert: %w`, err)
			}
			if _, err := execWrite(ctx, tx, evidence); err != nil {
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
	p, err := s.oneProposal(ctx, query.Equal(proposalsTable.Column("proposal_key"), key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// GetProposal returns one proposal by ID.
func (s *Store) GetProposal(ctx context.Context, id string) (*mecp.Proposal, error) {
	p, err := s.oneProposal(ctx, query.Equal(proposalID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, mecp.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// UpdateProposal writes back a reviewed proposal. Evidence is immutable once
// filed, so only the review fields are updated.
func (s *Store) UpdateProposal(ctx context.Context, p *mecp.Proposal) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		update, err := query.NewUpdate(proposalsTable,
			query.Set(proposalsTable.Column("status"), string(p.Status)),
			query.Set(proposalsTable.Column("decided_at"), formatTimePtr(p.DecidedAt)),
			query.Set(proposalsTable.Column("decided_by"), p.DecidedBy),
			query.Set(proposalsTable.Column("decision_note"), p.DecisionNote),
			query.Set(proposalsTable.Column("result_record_id"), p.ResultRecordID),
		)
		if err != nil {
			return fmt.Errorf(`failed to build the proposal update: %w`, err)
		}
		if update, err = update.WithWhere(query.Equal(proposalID, p.ID)); err != nil {
			return fmt.Errorf(`failed to filter the proposal update: %w`, err)
		}
		res, err := execWrite(ctx, tx, update)
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
	var where []query.Expression
	if q.PrincipalID != "" {
		where = append(where, query.Equal(proposalsTable.Column("principal_id"), q.PrincipalID))
	}
	if len(q.Statuses) > 0 {
		where = append(where, query.In(proposalsTable.Column("status"), toAny(q.Statuses)...))
	}

	var filter query.Expression
	switch len(where) {
	case 0:
	case 1:
		filter = where[0]
	default:
		filter = query.And(where...)
	}

	statement, err := selectWhere(proposalsTable, filter, proposalProjections()...)
	if err != nil {
		return nil, fmt.Errorf(`failed to build the proposal query: %w`, err)
	}
	createdAt := proposalsTable.Column("created_at")
	if statement, err = statement.WithOrder(query.Desc(createdAt), query.Desc(proposalID)); err != nil {
		return nil, fmt.Errorf(`failed to order the proposal query: %w`, err)
	}
	if statement, err = withPaging(statement, q.Limit, q.Offset); err != nil {
		return nil, fmt.Errorf(`failed to page the proposal query: %w`, err)
	}

	rows, err := querySelect(ctx, s.db, statement)
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

	position := proposalSourcesTable.Column("position")
	statement, err := selectWhere(proposalSourcesTable, query.In(proposalEvidenceID, ids...),
		proposalEvidenceID, proposalSourcesTable.Column("payload"))
	if err != nil {
		return fmt.Errorf(`failed to build the proposal evidence query: %w`, err)
	}
	if statement, err = statement.WithOrder(query.Asc(proposalEvidenceID), query.Asc(position)); err != nil {
		return fmt.Errorf(`failed to order the proposal evidence query: %w`, err)
	}
	rows, err := querySelect(ctx, s.db, statement)
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

// DeleteProposal removes a proposal and its evidence permanently.
func (s *Store) DeleteProposal(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		statement, err := deleteWhere(proposalsTable, query.Equal(proposalID, id))
		if err != nil {
			return fmt.Errorf(`failed to build the proposal delete: %w`, err)
		}
		res, err := execWrite(ctx, tx, statement)
		if err != nil {
			return fmt.Errorf(`failed to delete proposal %s: %w`, id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return mecp.ErrNotFound
		}
		// proposal_sources cascades, but the cascade only fires with foreign
		// keys on, so the evidence is removed explicitly too.
		evidence, err := deleteWhere(proposalSourcesTable, query.Equal(proposalEvidenceID, id))
		if err != nil {
			return fmt.Errorf(`failed to build the proposal evidence delete: %w`, err)
		}
		_, err = execWrite(ctx, tx, evidence)
		return err
	})
}
