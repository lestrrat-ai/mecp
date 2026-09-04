package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lestrrat-ai/mecp"
)

const recordColumns = `
	r.id, r.kind, r.subject, r.statement, r.rationale, r.authority, r.status, r.confidence,
	r.valid_from, r.valid_until, r.review_after, r.last_verified_at,
	r.validation_policy, r.superseded_by, r.conflict_group, r.created_at, r.updated_at,
	sc.principal, sc.org, sc.repository, sc.branch_patterns, sc.path_patterns, sc.task_kinds, sc.conditions`

// PutRecord inserts or replaces a record together with its scope, tags,
// sources, relationships, and search index entry, in one transaction. A record
// is never partially written.
func (s *Store) PutRecord(ctx context.Context, rec *mecp.Record) error {
	if err := rec.Validate(); err != nil {
		return err
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := deleteRecordRows(ctx, tx, rec.ID); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO records (
				id, kind, subject, normalized_subject, statement, rationale, authority, status,
				confidence, valid_from, valid_until, review_after,
				last_verified_at, validation_policy, superseded_by, conflict_group, created_at, updated_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			rec.ID, string(rec.Kind), rec.Subject, rec.NormalizedSubject(), rec.Statement, rec.Rationale,
			string(rec.Authority), string(rec.Status), rec.Confidence,
			formatTime(rec.ValidFrom), formatTimePtr(rec.ValidUntil),
			formatTimePtr(rec.ReviewAfter), formatTimePtr(rec.LastVerifiedAt), string(rec.ValidationPolicy),
			rec.SupersededBy, rec.ConflictGroup, formatTime(rec.CreatedAt), formatTime(rec.UpdatedAt),
		); err != nil {
			return fmt.Errorf(`failed to insert record %s: %w`, rec.ID, err)
		}

		if err := putScope(ctx, tx, rec); err != nil {
			return err
		}
		if err := putTags(ctx, tx, rec); err != nil {
			return err
		}
		if err := putSources(ctx, tx, rec); err != nil {
			return err
		}
		if err := putRelationships(ctx, tx, rec); err != nil {
			return err
		}
		return putFTS(ctx, tx, rec)
	})
}

func putScope(ctx context.Context, tx *sql.Tx, rec *mecp.Record) error {
	branches, err := json.Marshal(orEmpty(rec.Scope.BranchPatterns))
	if err != nil {
		return err
	}
	paths, err := json.Marshal(orEmpty(rec.Scope.PathPatterns))
	if err != nil {
		return err
	}
	kinds, err := json.Marshal(orEmptyKinds(rec.Scope.TaskKinds))
	if err != nil {
		return err
	}
	conds, err := json.Marshal(orEmptyMap(rec.Scope.Conditions))
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO record_scopes (record_id, principal, org, repository, branch_patterns, path_patterns, task_kinds, conditions)
		VALUES (?,?,?,?,?,?,?,?)`,
		rec.ID, rec.Scope.User, rec.Scope.Org, rec.Scope.Repository,
		string(branches), string(paths), string(kinds), string(conds))
	if err != nil {
		return fmt.Errorf(`failed to insert scope for record %s: %w`, rec.ID, err)
	}
	return nil
}

func putTags(ctx context.Context, tx *sql.Tx, rec *mecp.Record) error {
	for _, tag := range rec.Tags {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO record_tags (record_id, tag) VALUES (?, ?)`, rec.ID, tag); err != nil {
			return fmt.Errorf(`failed to insert tag %q for record %s: %w`, tag, rec.ID, err)
		}
	}
	return nil
}

func putSources(ctx context.Context, tx *sql.Tx, rec *mecp.Record) error {
	for i, src := range rec.Sources {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sources (id, type, locator, revision, content_hash, exact_excerpt, captured_at, validation_policy)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET
				type = excluded.type, locator = excluded.locator, revision = excluded.revision,
				content_hash = excluded.content_hash, exact_excerpt = excluded.exact_excerpt,
				captured_at = excluded.captured_at,
				validation_policy = excluded.validation_policy`,
			src.ID, string(src.Type), src.Locator, src.Revision, src.ContentHash, src.ExactExcerpt,
			formatTime(src.CapturedAt), string(src.ValidationPolicy),
		); err != nil {
			return fmt.Errorf(`failed to insert source %s: %w`, src.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO record_sources (record_id, source_id, position) VALUES (?,?,?)`,
			rec.ID, src.ID, i); err != nil {
			return fmt.Errorf(`failed to link source %s to record %s: %w`, src.ID, rec.ID, err)
		}
	}
	return nil
}

func putRelationships(ctx context.Context, tx *sql.Tx, rec *mecp.Record) error {
	for _, target := range rec.Supersedes {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO record_relationships (from_record_id, to_record_id, kind) VALUES (?,?, 'supersedes')`,
			rec.ID, target); err != nil {
			return fmt.Errorf(`failed to record supersession %s -> %s: %w`, rec.ID, target, err)
		}
	}
	return nil
}

func putFTS(ctx context.Context, tx *sql.Tx, rec *mecp.Record) error {
	var evidence strings.Builder
	for _, src := range rec.Sources {
		evidence.WriteString(src.ExactExcerpt)
		evidence.WriteByte('\n')
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO records_fts (record_id, subject, statement, rationale, tags, evidence)
		VALUES (?,?,?,?,?,?)`,
		rec.ID, rec.Subject, rec.Statement, rec.Rationale, strings.Join(rec.Tags, " "), evidence.String())
	if err != nil {
		return fmt.Errorf(`failed to index record %s: %w`, rec.ID, err)
	}
	return nil
}

// deleteRecordRows removes every trace of a record, including its search index
// entry and any source rows no other record still references. "Delete" has to
// mean the record stops being retrievable, not merely hidden.
func deleteRecordRows(ctx context.Context, tx *sql.Tx, id string) error {
	stmts := []string{
		`DELETE FROM records_fts WHERE record_id = ?`,
		`DELETE FROM record_sources WHERE record_id = ?`,
		`DELETE FROM record_tags WHERE record_id = ?`,
		`DELETE FROM record_relationships WHERE from_record_id = ?`,
		`DELETE FROM record_scopes WHERE record_id = ?`,
		`DELETE FROM records WHERE id = ?`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf(`failed to clear record %s: %w`, id, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sources WHERE id NOT IN (SELECT source_id FROM record_sources)`); err != nil {
		return fmt.Errorf(`failed to remove orphaned sources: %w`, err)
	}
	return nil
}

// DeleteRecord removes a record permanently.
func (s *Store) DeleteRecord(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM records WHERE id = ?`, id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return mecp.ErrNotFound
		}
		if err := deleteRecordRows(ctx, tx, id); err != nil {
			return err
		}
		// A dangling supersession edge would make a live record look superseded
		// by a record that no longer exists.
		_, err := tx.ExecContext(ctx, `DELETE FROM record_relationships WHERE to_record_id = ?`, id)
		return err
	})
}

// GetRecord returns one record without applying any scope filter. Callers that
// serve agents must authorize separately.
func (s *Store) GetRecord(ctx context.Context, id string) (*mecp.Record, error) {
	recs, err := s.QueryRecords(ctx, mecp.RecordQuery{IDs: []string{id}, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, mecp.ErrNotFound
	}
	return recs[0], nil
}

// QueryRecords applies a structured filter. Every security-relevant dimension
// is applied here in SQL, so an unauthorized row never reaches the caller,
// a result count, or the ranking stage.
func (s *Store) QueryRecords(ctx context.Context, q mecp.RecordQuery) ([]*mecp.Record, error) {
	where, args, ok := buildWhere(q)
	if !ok {
		return nil, nil
	}

	query := `SELECT ` + recordColumns + `
		FROM records r JOIN record_scopes sc ON sc.record_id = r.id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY r.id`
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
		return nil, fmt.Errorf(`failed to query records: %w`, err)
	}
	defer rows.Close()

	recs, err := scanRecords(rows)
	if err != nil {
		return nil, err
	}
	return s.hydrate(ctx, recs)
}

// buildWhere renders the structured filter. The boolean reports whether the
// filter can match anything at all; a filter that cannot is answered without
// touching the database.
func buildWhere(q mecp.RecordQuery) ([]string, []any, bool) {
	where := []string{"1=1"}
	var args []any

	if q.PrincipalID != "" {
		where = append(where, `(sc.principal = '' OR sc.principal = ?)`)
		args = append(args, q.PrincipalID)
	}
	if q.RestrictRepositories {
		switch {
		case len(q.Repositories) > 0 && q.AllowGlobal:
			where = append(where, `(sc.repository = '' OR sc.repository IN (`+placeholders(len(q.Repositories))+`))`)
			args = append(args, toAny(q.Repositories)...)
		case len(q.Repositories) > 0:
			where = append(where, `sc.repository IN (`+placeholders(len(q.Repositories))+`)`)
			args = append(args, toAny(q.Repositories)...)
		case q.AllowGlobal:
			where = append(where, `sc.repository = ''`)
		default:
			return nil, nil, false
		}
	}
	if len(q.Kinds) > 0 {
		where = append(where, `r.kind IN (`+placeholders(len(q.Kinds))+`)`)
		for _, k := range q.Kinds {
			args = append(args, string(k))
		}
	}
	if len(q.Statuses) > 0 {
		where = append(where, `r.status IN (`+placeholders(len(q.Statuses))+`)`)
		for _, st := range q.Statuses {
			args = append(args, string(st))
		}
	}
	if len(q.IDs) > 0 {
		where = append(where, `r.id IN (`+placeholders(len(q.IDs))+`)`)
		args = append(args, toAny(q.IDs)...)
	}
	if q.Subject != "" {
		where = append(where, `r.normalized_subject = ?`)
		args = append(args, strings.ToLower(strings.Join(strings.Fields(q.Subject), " ")))
	}
	if len(q.Tags) > 0 {
		where = append(where,
			`EXISTS (SELECT 1 FROM record_tags t WHERE t.record_id = r.id AND t.tag IN (`+placeholders(len(q.Tags))+`))`)
		args = append(args, toAny(q.Tags)...)
	}
	if !q.At.IsZero() {
		at := formatTime(q.At)
		where = append(where, `r.valid_from <= ?`, `(r.valid_until IS NULL OR r.valid_until > ?)`)
		args = append(args, at, at)
	}
	return where, args, true
}

func scanRecords(rows *sql.Rows) ([]*mecp.Record, error) {
	var out []*mecp.Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// rowScanner is the part of *sql.Rows and *sql.Row that scanRecord needs.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRecord reads the recordColumns list. Extra destinations are appended, so
// a query that selects additional trailing columns (a relevance score, for
// example) can reuse the same scanner.
func scanRecord(rows rowScanner, extra ...any) (*mecp.Record, error) {
	var (
		rec                                 mecp.Record
		kind, authority, status             string
		policy                              string
		validFrom                           string
		createdAt, updatedAt                string
		validUntil, reviewAfter, lastVerify sql.NullString
		branches, paths, taskKinds, conds   string
	)

	dest := []any{
		&rec.ID, &kind, &rec.Subject, &rec.Statement, &rec.Rationale, &authority, &status, &rec.Confidence,
		&validFrom, &validUntil, &reviewAfter, &lastVerify,
		&policy, &rec.SupersededBy, &rec.ConflictGroup, &createdAt, &updatedAt,
		&rec.Scope.User, &rec.Scope.Org, &rec.Scope.Repository, &branches, &paths, &taskKinds, &conds,
	}
	dest = append(dest, extra...)

	if err := rows.Scan(dest...); err != nil {
		return nil, fmt.Errorf(`failed to scan record: %w`, err)
	}

	rec.Kind = mecp.RecordKind(kind)
	rec.Authority = mecp.Authority(authority)
	rec.Status = mecp.RecordStatus(status)
	rec.ValidationPolicy = mecp.ValidationPolicy(policy)

	var err error
	if rec.ValidFrom, err = parseTime(validFrom); err != nil {
		return nil, fmt.Errorf(`record %s has an unreadable valid_from: %w`, rec.ID, err)
	}
	if rec.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf(`record %s has an unreadable created_at: %w`, rec.ID, err)
	}
	if rec.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf(`record %s has an unreadable updated_at: %w`, rec.ID, err)
	}
	if rec.ValidUntil, err = parseTimePtr(validUntil); err != nil {
		return nil, fmt.Errorf(`record %s has an unreadable valid_until: %w`, rec.ID, err)
	}
	if rec.ReviewAfter, err = parseTimePtr(reviewAfter); err != nil {
		return nil, fmt.Errorf(`record %s has an unreadable review_after: %w`, rec.ID, err)
	}
	if rec.LastVerifiedAt, err = parseTimePtr(lastVerify); err != nil {
		return nil, fmt.Errorf(`record %s has an unreadable last_verified_at: %w`, rec.ID, err)
	}

	if err := json.Unmarshal([]byte(branches), &rec.Scope.BranchPatterns); err != nil {
		return nil, fmt.Errorf(`record %s has unreadable branch patterns: %w`, rec.ID, err)
	}
	if err := json.Unmarshal([]byte(paths), &rec.Scope.PathPatterns); err != nil {
		return nil, fmt.Errorf(`record %s has unreadable path patterns: %w`, rec.ID, err)
	}
	if err := json.Unmarshal([]byte(taskKinds), &rec.Scope.TaskKinds); err != nil {
		return nil, fmt.Errorf(`record %s has unreadable task kinds: %w`, rec.ID, err)
	}
	if err := json.Unmarshal([]byte(conds), &rec.Scope.Conditions); err != nil {
		return nil, fmt.Errorf(`record %s has unreadable conditions: %w`, rec.ID, err)
	}
	if len(rec.Scope.Conditions) == 0 {
		rec.Scope.Conditions = nil
	}
	return &rec, nil
}

// hydrate loads tags, sources, and supersession edges for a batch of records
// in three queries rather than three per record.
func (s *Store) hydrate(ctx context.Context, recs []*mecp.Record) ([]*mecp.Record, error) {
	if len(recs) == 0 {
		return recs, nil
	}
	byID := make(map[string]*mecp.Record, len(recs))
	ids := make([]any, 0, len(recs))
	for _, rec := range recs {
		byID[rec.ID] = rec
		ids = append(ids, rec.ID)
	}

	tagRows, err := s.db.QueryContext(ctx,
		`SELECT record_id, tag FROM record_tags WHERE record_id IN (`+placeholders(len(ids))+`) ORDER BY record_id, tag`, ids...)
	if err != nil {
		return nil, fmt.Errorf(`failed to load tags: %w`, err)
	}
	if err := scanInto(tagRows, func(rows *sql.Rows) error {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return err
		}
		byID[id].Tags = append(byID[id].Tags, tag)
		return nil
	}); err != nil {
		return nil, err
	}

	srcRows, err := s.db.QueryContext(ctx, `
		SELECT rs.record_id, s.id, s.type, s.locator, s.revision, s.content_hash, s.exact_excerpt,
		       s.captured_at, s.validation_policy
		FROM record_sources rs JOIN sources s ON s.id = rs.source_id
		WHERE rs.record_id IN (`+placeholders(len(ids))+`)
		ORDER BY rs.record_id, rs.position`, ids...)
	if err != nil {
		return nil, fmt.Errorf(`failed to load sources: %w`, err)
	}
	if err := scanInto(srcRows, func(rows *sql.Rows) error {
		var (
			recordID, srcType, capturedAt, policy string
			src                                   mecp.Source
		)
		if err := rows.Scan(&recordID, &src.ID, &srcType, &src.Locator, &src.Revision,
			&src.ContentHash, &src.ExactExcerpt, &capturedAt, &policy); err != nil {
			return err
		}
		src.Type = mecp.SourceType(srcType)
		src.ValidationPolicy = mecp.ValidationPolicy(policy)
		t, err := parseTime(capturedAt)
		if err != nil {
			return fmt.Errorf(`source %s has an unreadable captured_at: %w`, src.ID, err)
		}
		src.CapturedAt = t
		byID[recordID].Sources = append(byID[recordID].Sources, src)
		return nil
	}); err != nil {
		return nil, err
	}

	relRows, err := s.db.QueryContext(ctx, `
		SELECT from_record_id, to_record_id FROM record_relationships
		WHERE kind = 'supersedes' AND from_record_id IN (`+placeholders(len(ids))+`)
		ORDER BY from_record_id, to_record_id`, ids...)
	if err != nil {
		return nil, fmt.Errorf(`failed to load relationships: %w`, err)
	}
	if err := scanInto(relRows, func(rows *sql.Rows) error {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return err
		}
		byID[from].Supersedes = append(byID[from].Supersedes, to)
		return nil
	}); err != nil {
		return nil, err
	}

	return recs, nil
}

// SupersededBy returns, for each requested record, the records that supersede it.
func (s *Store) SupersededBy(ctx context.Context, ids []string) (map[string][]string, error) {
	if len(ids) == 0 {
		return map[string][]string{}, nil
	}
	args := toAny(ids)
	rows, err := s.db.QueryContext(ctx, `
		SELECT to_record_id, from_record_id FROM record_relationships
		WHERE kind = 'supersedes' AND to_record_id IN (`+placeholders(len(args))+`)
		ORDER BY to_record_id, from_record_id`, args...)
	if err != nil {
		return nil, fmt.Errorf(`failed to load supersession: %w`, err)
	}

	out := make(map[string][]string)
	if err := scanInto(rows, func(rows *sql.Rows) error {
		var to, from string
		if err := rows.Scan(&to, &from); err != nil {
			return err
		}
		out[to] = append(out[to], from)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func scanInto(rows *sql.Rows, fn func(*sql.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func toAny[T ~string](in []T) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return out
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return slices.Clone(in)
}

func orEmptyKinds(in []mecp.TaskKind) []mecp.TaskKind {
	if in == nil {
		return []mecp.TaskKind{}
	}
	return slices.Clone(in)
}

func orEmptyMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

// IsNotFound reports whether err means the addressed row does not exist.
func IsNotFound(err error) bool { return errors.Is(err, mecp.ErrNotFound) }

// KnownRepositories returns every canonical repository some record is scoped
// to, sorted and without duplicates.
func (s *Store) KnownRepositories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT repository FROM record_scopes WHERE repository != '' ORDER BY repository`)
	if err != nil {
		return nil, fmt.Errorf(`failed to list repositories: %w`, err)
	}

	var out []string
	if err := scanInto(rows, func(rows *sql.Rows) error {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return err
		}
		out = append(out, repo)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}
