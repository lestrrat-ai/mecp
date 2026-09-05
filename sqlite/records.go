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
	"github.com/lestrrat-go/rasql/query"
)

// recordProjections lists the columns a record is read from, in the order
// scanRecord expects them. The two lists are positional, so a column added
// here needs a destination added there.
func recordProjections() []query.Projection {
	return []query.Projection{
		recordID,
		recordsTable.Column("kind"),
		recordsTable.Column("subject"),
		recordsTable.Column("statement"),
		recordsTable.Column("rationale"),
		recordsTable.Column("authority"),
		recordsTable.Column("status"),
		recordsTable.Column("confidence"),
		recordsTable.Column("valid_from"),
		recordsTable.Column("valid_until"),
		recordsTable.Column("review_after"),
		recordsTable.Column("last_verified_at"),
		recordsTable.Column("validation_policy"),
		recordsTable.Column("superseded_by"),
		recordsTable.Column("conflict_group"),
		recordsTable.Column("created_at"),
		recordUpdatedAt,
		recordScopesTable.Column("principal"),
		recordScopesTable.Column("org"),
		recordScopesTable.Column("repository"),
		recordScopesTable.Column("branch_patterns"),
		recordScopesTable.Column("path_patterns"),
		recordScopesTable.Column("task_kinds"),
		recordScopesTable.Column("conditions"),
	}
}

// scopeJoin attaches the scope row every record read needs, both for the
// projected scope columns and for the authorization predicates.
func scopeJoin() query.Join {
	return query.InnerJoin(recordScopesTable, query.Equal(scopeRecordID, recordID))
}

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

		insert, err := query.NewInsert(recordsTable,
			query.Set(recordID, rec.ID),
			query.Set(recordsTable.Column("kind"), string(rec.Kind)),
			query.Set(recordsTable.Column("subject"), rec.Subject),
			query.Set(recordsTable.Column("normalized_subject"), rec.NormalizedSubject()),
			query.Set(recordsTable.Column("statement"), rec.Statement),
			query.Set(recordsTable.Column("rationale"), rec.Rationale),
			query.Set(recordsTable.Column("authority"), string(rec.Authority)),
			query.Set(recordsTable.Column("status"), string(rec.Status)),
			query.Set(recordsTable.Column("confidence"), rec.Confidence),
			query.Set(recordsTable.Column("valid_from"), formatTime(rec.ValidFrom)),
			query.Set(recordsTable.Column("valid_until"), formatTimePtr(rec.ValidUntil)),
			query.Set(recordsTable.Column("review_after"), formatTimePtr(rec.ReviewAfter)),
			query.Set(recordsTable.Column("last_verified_at"), formatTimePtr(rec.LastVerifiedAt)),
			query.Set(recordsTable.Column("validation_policy"), string(rec.ValidationPolicy)),
			query.Set(recordsTable.Column("superseded_by"), rec.SupersededBy),
			query.Set(recordsTable.Column("conflict_group"), rec.ConflictGroup),
			query.Set(recordsTable.Column("created_at"), formatTime(rec.CreatedAt)),
			query.Set(recordUpdatedAt, formatTime(rec.UpdatedAt)),
		)
		if err != nil {
			return fmt.Errorf(`failed to build the record insert: %w`, err)
		}
		if _, err := execWrite(ctx, tx, insert); err != nil {
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

	insert, err := query.NewInsert(recordScopesTable,
		query.Set(scopeRecordID, rec.ID),
		query.Set(recordScopesTable.Column("principal"), rec.Scope.User),
		query.Set(recordScopesTable.Column("org"), rec.Scope.Org),
		query.Set(recordScopesTable.Column("repository"), rec.Scope.Repository),
		query.Set(recordScopesTable.Column("branch_patterns"), string(branches)),
		query.Set(recordScopesTable.Column("path_patterns"), string(paths)),
		query.Set(recordScopesTable.Column("task_kinds"), string(kinds)),
		query.Set(recordScopesTable.Column("conditions"), string(conds)),
	)
	if err != nil {
		return fmt.Errorf(`failed to build the scope insert: %w`, err)
	}
	if _, err := execWrite(ctx, tx, insert); err != nil {
		return fmt.Errorf(`failed to insert scope for record %s: %w`, rec.ID, err)
	}
	return nil
}

func putTags(ctx context.Context, tx *sql.Tx, rec *mecp.Record) error {
	for _, tag := range rec.Tags {
		insert, err := query.NewInsert(recordTagsTable,
			query.Set(tagRecordID, rec.ID),
			query.Set(tagName, tag),
		)
		if err != nil {
			return fmt.Errorf(`failed to build the tag insert: %w`, err)
		}
		// An upsert with no assignments is ON CONFLICT DO NOTHING, which is
		// what the tag write has always wanted: re-adding a tag a record
		// already carries is not an error.
		ignore, err := query.NewUpsert(insert, []query.ColumnRef{tagRecordID, tagName}, nil)
		if err != nil {
			return fmt.Errorf(`failed to build the tag upsert: %w`, err)
		}
		if _, err := execWrite(ctx, tx, ignore); err != nil {
			return fmt.Errorf(`failed to insert tag %q for record %s: %w`, tag, rec.ID, err)
		}
	}
	return nil
}

func putSources(ctx context.Context, tx *sql.Tx, rec *mecp.Record) error {
	for i, src := range rec.Sources {
		insert, err := query.NewInsert(sourcesTable,
			query.Set(sourceID, src.ID),
			query.Set(sourcesTable.Column("type"), string(src.Type)),
			query.Set(sourcesTable.Column("locator"), src.Locator),
			query.Set(sourcesTable.Column("revision"), src.Revision),
			query.Set(sourcesTable.Column("content_hash"), src.ContentHash),
			query.Set(sourcesTable.Column("exact_excerpt"), src.ExactExcerpt),
			query.Set(sourcesTable.Column("captured_at"), formatTime(src.CapturedAt)),
			query.Set(sourcesTable.Column("validation_policy"), string(src.ValidationPolicy)),
		)
		if err != nil {
			return fmt.Errorf(`failed to build the source insert: %w`, err)
		}
		upsert, err := query.NewUpsert(insert, []query.ColumnRef{sourceID}, sourceUpsertAssignments())
		if err != nil {
			return fmt.Errorf(`failed to build the source upsert: %w`, err)
		}
		if _, err := execWrite(ctx, tx, upsert); err != nil {
			return fmt.Errorf(`failed to insert source %s: %w`, src.ID, err)
		}

		position := recordSourcesTable.Column("position")
		link, err := query.NewInsert(recordSourcesTable,
			query.Set(recordSourceRecID, rec.ID),
			query.Set(recordSourceSrcID, src.ID),
			query.Set(position, i),
		)
		if err != nil {
			return fmt.Errorf(`failed to build the source link insert: %w`, err)
		}
		relink, err := query.NewUpsert(link,
			[]query.ColumnRef{recordSourceRecID, recordSourceSrcID},
			[]query.Assignment{query.Set(position, query.Excluded(position))},
		)
		if err != nil {
			return fmt.Errorf(`failed to build the source link upsert: %w`, err)
		}
		if _, err := execWrite(ctx, tx, relink); err != nil {
			return fmt.Errorf(`failed to link source %s to record %s: %w`, src.ID, rec.ID, err)
		}
	}
	return nil
}

// sourceUpsertAssignments refreshes every mutable column of a source from the
// row being written, so a source captured again replaces what was stored.
func sourceUpsertAssignments() []query.Assignment {
	names := []string{"type", "locator", "revision", "content_hash", "exact_excerpt", "captured_at", "validation_policy"}
	out := make([]query.Assignment, 0, len(names))
	for _, name := range names {
		column := sourcesTable.Column(name)
		out = append(out, query.Set(column, query.Excluded(column)))
	}
	return out
}

func putRelationships(ctx context.Context, tx *sql.Tx, rec *mecp.Record) error {
	for _, target := range rec.Supersedes {
		insert, err := query.NewInsert(recordRelationshipsTable,
			query.Set(relationshipFrom, rec.ID),
			query.Set(relationshipTo, target),
			query.Set(relationshipKind, supersedesKind),
		)
		if err != nil {
			return fmt.Errorf(`failed to build the supersession insert: %w`, err)
		}
		ignore, err := query.NewUpsert(insert,
			[]query.ColumnRef{relationshipFrom, relationshipTo, relationshipKind}, nil)
		if err != nil {
			return fmt.Errorf(`failed to build the supersession upsert: %w`, err)
		}
		if _, err := execWrite(ctx, tx, ignore); err != nil {
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
	insert, err := query.NewInsert(recordsFTSTable,
		query.Set(ftsRecordID, rec.ID),
		query.Set(recordsFTSTable.Column("subject"), rec.Subject),
		query.Set(recordsFTSTable.Column("statement"), rec.Statement),
		query.Set(recordsFTSTable.Column("rationale"), rec.Rationale),
		query.Set(recordsFTSTable.Column("tags"), strings.Join(rec.Tags, " ")),
		query.Set(recordsFTSTable.Column("evidence"), evidence.String()),
	)
	if err != nil {
		return fmt.Errorf(`failed to build the index insert: %w`, err)
	}
	if _, err := execWrite(ctx, tx, insert); err != nil {
		return fmt.Errorf(`failed to index record %s: %w`, rec.ID, err)
	}
	return nil
}

// deleteRecordRows removes every trace of a record, including its search index
// entry and any source rows no other record still references. "Delete" has to
// mean the record stops being retrievable, not merely hidden.
func deleteRecordRows(ctx context.Context, tx *sql.Tx, id string) error {
	owned := []struct {
		table  query.TableRef
		column query.ColumnRef
	}{
		{recordsFTSTable, ftsRecordID},
		{recordSourcesTable, recordSourceRecID},
		{recordTagsTable, tagRecordID},
		{recordRelationshipsTable, relationshipFrom},
		{recordScopesTable, scopeRecordID},
		{recordsTable, recordID},
	}
	for _, target := range owned {
		statement, err := deleteWhere(target.table, query.Equal(target.column, id))
		if err != nil {
			return fmt.Errorf(`failed to build the delete for record %s: %w`, id, err)
		}
		if _, err := execWrite(ctx, tx, statement); err != nil {
			return fmt.Errorf(`failed to clear record %s: %w`, id, err)
		}
	}

	// A source row survives only while some record still links to it, so the
	// sources nothing links to any more go with the record that cited them.
	linked, err := query.NewSelect(recordSourcesTable, recordSourceSrcID)
	if err != nil {
		return fmt.Errorf(`failed to build the orphan subquery: %w`, err)
	}
	orphans, err := deleteWhere(sourcesTable, query.NotInSelect(sourceID, linked))
	if err != nil {
		return fmt.Errorf(`failed to build the orphan delete: %w`, err)
	}
	if _, err := execWrite(ctx, tx, orphans); err != nil {
		return fmt.Errorf(`failed to remove orphaned sources: %w`, err)
	}
	return nil
}

// DeleteRecord removes a record permanently.
func (s *Store) DeleteRecord(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		count, err := selectWhere(recordsTable, query.Equal(recordID, id), query.CountAll().As("n"))
		if err != nil {
			return fmt.Errorf(`failed to build the record count: %w`, err)
		}
		rows, err := querySelect(ctx, tx, count)
		if err != nil {
			return err
		}
		var exists int
		if err := scanOne(rows, &exists); err != nil {
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
		dangling, err := deleteWhere(recordRelationshipsTable, query.Equal(relationshipTo, id))
		if err != nil {
			return fmt.Errorf(`failed to build the supersession cleanup: %w`, err)
		}
		_, err = execWrite(ctx, tx, dangling)
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
	statement, ok, err := recordSelect(q)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if statement, err = statement.WithOrder(query.Asc(recordID)); err != nil {
		return nil, fmt.Errorf(`failed to order the record query: %w`, err)
	}
	if statement, err = withPaging(statement, q.Limit, q.Offset); err != nil {
		return nil, fmt.Errorf(`failed to page the record query: %w`, err)
	}

	rows, err := querySelect(ctx, s.db, statement)
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

// recordSelect builds the joined select every structured record read starts
// from, already carrying the filter. The boolean reports whether the filter can
// match anything at all; a filter that cannot is answered without touching the
// database.
func recordSelect(q mecp.RecordQuery) (query.Select, bool, error) {
	where, ok, err := buildPredicate(q)
	if err != nil {
		return query.Select{}, false, err
	}
	if !ok {
		return query.Select{}, false, nil
	}
	statement, err := query.NewJoinedSelect(recordsTable, []query.Join{scopeJoin()}, nil, recordProjections()...)
	if err != nil {
		return query.Select{}, false, fmt.Errorf(`failed to build the record query: %w`, err)
	}
	if where == nil {
		return statement, true, nil
	}
	statement, err = statement.WithWhere(where)
	if err != nil {
		return query.Select{}, false, fmt.Errorf(`failed to filter the record query: %w`, err)
	}
	return statement, true, nil
}

// buildPredicate turns the structured filter into one expression tree. It
// returns a nil expression when the filter constrains nothing, and false when
// the filter cannot match anything at all.
func buildPredicate(q mecp.RecordQuery) (query.Expression, bool, error) {
	var where []query.Expression

	principal := recordScopesTable.Column("principal")
	repository := recordScopesTable.Column("repository")

	if q.PrincipalID != "" {
		where = append(where, query.Or(
			query.Equal(principal, ""),
			query.Equal(principal, q.PrincipalID),
		))
	}
	if q.RestrictRepositories {
		switch {
		case len(q.Repositories) > 0 && q.AllowGlobal:
			where = append(where, query.Or(
				query.Equal(repository, ""),
				query.In(repository, toAny(q.Repositories)...),
			))
		case len(q.Repositories) > 0:
			where = append(where, query.In(repository, toAny(q.Repositories)...))
		case q.AllowGlobal:
			where = append(where, query.Equal(repository, ""))
		default:
			return nil, false, nil
		}
	}
	if len(q.Kinds) > 0 {
		where = append(where, query.In(recordsTable.Column("kind"), toAny(q.Kinds)...))
	}
	if len(q.Statuses) > 0 {
		where = append(where, query.In(recordsTable.Column("status"), toAny(q.Statuses)...))
	}
	if len(q.IDs) > 0 {
		where = append(where, query.In(recordID, toAny(q.IDs)...))
	}
	if q.Subject != "" {
		normalized := strings.ToLower(strings.Join(strings.Fields(q.Subject), " "))
		where = append(where, query.Equal(recordsTable.Column("normalized_subject"), normalized))
	}
	if len(q.Tags) > 0 {
		tagged, err := taggedWith(q.Tags)
		if err != nil {
			return nil, false, err
		}
		where = append(where, tagged)
	}
	if !q.At.IsZero() {
		at := formatTime(q.At)
		validUntil := recordsTable.Column("valid_until")
		where = append(where,
			query.LessThanOrEqual(recordsTable.Column("valid_from"), at),
			query.Or(query.IsNull(validUntil), query.GreaterThan(validUntil, at)),
		)
	}

	switch len(where) {
	case 0:
		return nil, true, nil
	case 1:
		return where[0], true, nil
	default:
		return query.And(where...), true, nil
	}
}

// taggedWith matches a record carrying any of the given tags. The subquery
// reads records.id, so it is correlated: WithCorrelation names that enclosing
// table, and has to come before the WithWhere that reads it.
func taggedWith(tags []string) (query.Expression, error) {
	inner, err := query.NewSelect(recordTagsTable, tagRecordID)
	if err != nil {
		return nil, fmt.Errorf(`failed to build the tag filter: %w`, err)
	}
	if inner, err = inner.WithCorrelation(recordsTable); err != nil {
		return nil, fmt.Errorf(`failed to correlate the tag filter: %w`, err)
	}
	inner, err = inner.WithWhere(query.And(
		query.Equal(tagRecordID, recordID),
		query.In(tagName, toAny(tags)...),
	))
	if err != nil {
		return nil, fmt.Errorf(`failed to filter the tag subquery: %w`, err)
	}
	return query.Exists(inner), nil
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

	tagQuery, err := selectWhere(recordTagsTable, query.In(tagRecordID, ids...), tagRecordID, tagName)
	if err != nil {
		return nil, fmt.Errorf(`failed to build the tag query: %w`, err)
	}
	if tagQuery, err = tagQuery.WithOrder(query.Asc(tagRecordID), query.Asc(tagName)); err != nil {
		return nil, fmt.Errorf(`failed to order the tag query: %w`, err)
	}
	tagRows, err := querySelect(ctx, s.db, tagQuery)
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

	position := recordSourcesTable.Column("position")
	srcQuery, err := query.NewJoinedSelect(recordSourcesTable,
		[]query.Join{query.InnerJoin(sourcesTable, query.Equal(sourceID, recordSourceSrcID))},
		nil,
		recordSourceRecID,
		sourceID,
		sourcesTable.Column("type"),
		sourcesTable.Column("locator"),
		sourcesTable.Column("revision"),
		sourcesTable.Column("content_hash"),
		sourcesTable.Column("exact_excerpt"),
		sourcesTable.Column("captured_at"),
		sourcesTable.Column("validation_policy"),
	)
	if err != nil {
		return nil, fmt.Errorf(`failed to build the source query: %w`, err)
	}
	if srcQuery, err = srcQuery.WithWhere(query.In(recordSourceRecID, ids...)); err != nil {
		return nil, fmt.Errorf(`failed to filter the source query: %w`, err)
	}
	if srcQuery, err = srcQuery.WithOrder(query.Asc(recordSourceRecID), query.Asc(position)); err != nil {
		return nil, fmt.Errorf(`failed to order the source query: %w`, err)
	}
	srcRows, err := querySelect(ctx, s.db, srcQuery)
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

	relQuery, err := selectWhere(recordRelationshipsTable,
		query.And(
			query.Equal(relationshipKind, supersedesKind),
			query.In(relationshipFrom, ids...),
		),
		relationshipFrom, relationshipTo)
	if err != nil {
		return nil, fmt.Errorf(`failed to build the supersession query: %w`, err)
	}
	if relQuery, err = relQuery.WithOrder(query.Asc(relationshipFrom), query.Asc(relationshipTo)); err != nil {
		return nil, fmt.Errorf(`failed to order the supersession query: %w`, err)
	}
	relRows, err := querySelect(ctx, s.db, relQuery)
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
	statement, err := selectWhere(recordRelationshipsTable,
		query.And(
			query.Equal(relationshipKind, supersedesKind),
			query.In(relationshipTo, toAny(ids)...),
		),
		relationshipTo, relationshipFrom)
	if err != nil {
		return nil, fmt.Errorf(`failed to build the supersession query: %w`, err)
	}
	if statement, err = statement.WithOrder(query.Asc(relationshipTo), query.Asc(relationshipFrom)); err != nil {
		return nil, fmt.Errorf(`failed to order the supersession query: %w`, err)
	}
	rows, err := querySelect(ctx, s.db, statement)
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
	repository := recordScopesTable.Column("repository")
	statement, err := selectWhere(recordScopesTable, query.NotEqual(repository, ""), repository)
	if err != nil {
		return nil, fmt.Errorf(`failed to build the repository query: %w`, err)
	}
	if statement, err = statement.WithDistinct(); err != nil {
		return nil, fmt.Errorf(`failed to build the repository query: %w`, err)
	}
	if statement, err = statement.WithOrder(query.Asc(repository)); err != nil {
		return nil, fmt.Errorf(`failed to order the repository query: %w`, err)
	}
	rows, err := querySelect(ctx, s.db, statement)
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
