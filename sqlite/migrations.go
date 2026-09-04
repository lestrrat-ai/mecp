package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// migration is one ordered, forward-only schema step. Migrations are applied
// inside a transaction so that a failure leaves the database at its previous
// version rather than half-upgraded.
type migration struct {
	version int
	name    string
	stmts   []string
}

var migrations = []migration{
	{
		version: 1,
		name:    "initial schema",
		stmts: []string{
			`CREATE TABLE records (
				id                 TEXT PRIMARY KEY,
				kind               TEXT NOT NULL,
				subject            TEXT NOT NULL,
				normalized_subject TEXT NOT NULL,
				statement          TEXT NOT NULL,
				rationale          TEXT NOT NULL DEFAULT '',
				authority          TEXT NOT NULL,
				status             TEXT NOT NULL,
				confidence         REAL NOT NULL DEFAULT 1.0,
				valid_from         TEXT NOT NULL,
				valid_until        TEXT,
				review_after       TEXT,
				last_verified_at   TEXT,
				validation_policy  TEXT NOT NULL,
				superseded_by      TEXT NOT NULL DEFAULT '',
				conflict_group     TEXT NOT NULL DEFAULT '',
				created_at         TEXT NOT NULL,
				updated_at         TEXT NOT NULL
			)`,
			`CREATE INDEX idx_records_status ON records(status)`,
			`CREATE INDEX idx_records_kind ON records(kind)`,
			`CREATE INDEX idx_records_subject ON records(normalized_subject)`,

			`CREATE TABLE record_scopes (
				record_id       TEXT PRIMARY KEY REFERENCES records(id) ON DELETE CASCADE,
				principal       TEXT NOT NULL DEFAULT '',
				org             TEXT NOT NULL DEFAULT '',
				repository      TEXT NOT NULL DEFAULT '',
				branch_patterns TEXT NOT NULL DEFAULT '[]',
				path_patterns   TEXT NOT NULL DEFAULT '[]',
				task_kinds      TEXT NOT NULL DEFAULT '[]',
				conditions      TEXT NOT NULL DEFAULT '{}'
			)`,
			`CREATE INDEX idx_record_scopes_repository ON record_scopes(repository)`,
			`CREATE INDEX idx_record_scopes_principal ON record_scopes(principal)`,

			`CREATE TABLE sources (
				id                TEXT PRIMARY KEY,
				type              TEXT NOT NULL,
				locator           TEXT NOT NULL,
				revision          TEXT NOT NULL DEFAULT '',
				content_hash      TEXT NOT NULL DEFAULT '',
				exact_excerpt     TEXT NOT NULL DEFAULT '',
				captured_at       TEXT NOT NULL,
				validation_policy TEXT NOT NULL DEFAULT ''
			)`,

			`CREATE TABLE record_sources (
				record_id TEXT NOT NULL REFERENCES records(id) ON DELETE CASCADE,
				source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
				position  INTEGER NOT NULL,
				PRIMARY KEY (record_id, source_id)
			)`,
			`CREATE INDEX idx_record_sources_source ON record_sources(source_id)`,

			`CREATE TABLE record_relationships (
				from_record_id TEXT NOT NULL,
				to_record_id   TEXT NOT NULL,
				kind           TEXT NOT NULL,
				PRIMARY KEY (from_record_id, to_record_id, kind)
			)`,
			`CREATE INDEX idx_relationships_to ON record_relationships(to_record_id, kind)`,

			`CREATE TABLE record_tags (
				record_id TEXT NOT NULL REFERENCES records(id) ON DELETE CASCADE,
				tag       TEXT NOT NULL,
				PRIMARY KEY (record_id, tag)
			)`,
			`CREATE INDEX idx_record_tags_tag ON record_tags(tag)`,

			`CREATE TABLE proposals (
				id                    TEXT PRIMARY KEY,
				proposal_key          TEXT NOT NULL UNIQUE,
				status                TEXT NOT NULL,
				principal_id          TEXT NOT NULL,
				client_id             TEXT NOT NULL,
				kind                  TEXT NOT NULL,
				subject               TEXT NOT NULL,
				statement             TEXT NOT NULL,
				rationale             TEXT NOT NULL DEFAULT '',
				scope                 TEXT NOT NULL DEFAULT '{}',
				tags                  TEXT NOT NULL DEFAULT '[]',
				supersedes_record_ids TEXT NOT NULL DEFAULT '[]',
				created_at            TEXT NOT NULL,
				decided_at            TEXT,
				decided_by            TEXT NOT NULL DEFAULT '',
				decision_note         TEXT NOT NULL DEFAULT '',
				result_record_id      TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE INDEX idx_proposals_status ON proposals(status, created_at)`,

			`CREATE TABLE proposal_sources (
				proposal_id TEXT NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
				position    INTEGER NOT NULL,
				payload     TEXT NOT NULL,
				PRIMARY KEY (proposal_id, position)
			)`,

			`CREATE TABLE audit_events (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				at           TEXT NOT NULL,
				principal_id TEXT NOT NULL,
				client_id    TEXT NOT NULL,
				operation    TEXT NOT NULL,
				payload      TEXT NOT NULL
			)`,
			`CREATE INDEX idx_audit_events_at ON audit_events(at)`,

			// records_fts indexes only the columns the current policy allows to
			// be searched. record_id is stored but not indexed so that an
			// identifier can never satisfy a text match.
			`CREATE VIRTUAL TABLE records_fts USING fts5(
				record_id UNINDEXED,
				subject,
				statement,
				rationale,
				tags,
				evidence,
				tokenize = 'unicode61 remove_diacritics 2'
			)`,
		},
	},
}

// Migrate applies every migration this build knows about that the database has
// not yet seen.
func (s *Store) Migrate(ctx context.Context) error {
	if s.readOnly {
		return s.checkSchema(ctx)
	}

	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf(`failed to create migration table: %w`, err)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return fmt.Errorf(`failed to apply migration %d (%s): %w`, m.version, m.name, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, m migration) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range m.stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf(`statement failed: %w`, err)
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, formatTime(time.Now()))
		return err
	})
}

func (s *Store) appliedVersions(ctx context.Context) (map[int]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf(`failed to read applied migrations: %w`, err)
	}
	defer rows.Close()

	out := make(map[int]struct{})
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = struct{}{}
	}
	return out, rows.Err()
}

// checkSchema verifies a read-only database is already at the expected version
// rather than silently serving queries against a schema this build predates.
func (s *Store) checkSchema(ctx context.Context) error {
	var current int
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf(`database is not initialized; run "mecp init" with write access: %w`, err)
	}
	want := migrations[len(migrations)-1].version
	if current < want {
		return fmt.Errorf(`database schema is at version %d but this build expects %d; run "mecp migrate" with write access`, current, want)
	}
	return nil
}
