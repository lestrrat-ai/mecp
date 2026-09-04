// Package sqlite implements the mecp record store on SQLite with an FTS5
// index. SQLite was chosen because the workload is local, read-heavy,
// structured, and full of exact identifiers, and because the user can inspect
// and back up the result with ordinary tools.
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lestrrat-ai/mecp"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

// Store is a SQLite-backed mecp.Store. It is safe for concurrent use.
type Store struct {
	db           *sql.DB
	path         string
	readOnly     bool
	minScore     float64
	minRelevance float64
}

// Option configures Open.
type Option func(*openConfig)

type openConfig struct {
	readOnly     bool
	busyTimeout  time.Duration
	minScore     float64
	minRelevance float64
}

// WithReadOnly opens the database without write access. Agent-facing MCP
// processes use this unless the proposal tool is enabled, so that a bug in the
// gateway cannot corrupt the store.
func WithReadOnly(v bool) Option { return func(c *openConfig) { c.readOnly = v } }

// WithBusyTimeout sets how long a statement waits for a competing writer.
func WithBusyTimeout(d time.Duration) Option { return func(c *openConfig) { c.busyTimeout = d } }

// WithMinSearchScore overrides defaultMinScore, the absolute bm25 floor a
// search hit must clear to be returned at all. Zero disables the floor: every
// bm25 hit scores strictly positive, so nothing is ever excluded by it.
func WithMinSearchScore(v float64) Option { return func(c *openConfig) { c.minScore = v } }

// WithMinSearchRelevance overrides defaultMinRelevance, the fraction of the
// best hit in a result set that a weaker hit must still reach to survive.
// Zero disables the floor, since relevance is never negative.
func WithMinSearchRelevance(v float64) Option { return func(c *openConfig) { c.minRelevance = v } }

// Open connects to the database at path, creating the containing directory
// when the store is writable.
func Open(path string, options ...Option) (*Store, error) {
	cfg := openConfig{busyTimeout: 5 * time.Second, minScore: defaultMinScore, minRelevance: defaultMinRelevance}
	for _, opt := range options {
		opt(&cfg)
	}

	if path != ":memory:" {
		if cfg.readOnly {
			if err := checkStoreExists(path); err != nil {
				return nil, err
			}
		} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf(`failed to create database directory: %w`, err)
		}
	}

	db, err := sql.Open("sqlite", dsn(path, cfg))
	if err != nil {
		return nil, fmt.Errorf(`failed to open database %s: %w`, path, err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf(`failed to connect to database %s: %w`, path, err)
	}

	// A single writer avoids SQLITE_BUSY under concurrent MCP processes; the
	// WAL journal still allows readers to proceed in parallel.
	if !cfg.readOnly {
		db.SetMaxOpenConns(1)
	}

	return &Store{
		db:           db,
		path:         path,
		readOnly:     cfg.readOnly,
		minScore:     cfg.minScore,
		minRelevance: cfg.minRelevance,
	}, nil
}

// checkStoreExists fails fast, before the driver ever gets a handle, so that a
// missing store is reported as a missing store rather than as the SQLite
// driver's bare "unable to open database file" code. Read-only mode cannot
// create the file itself, so a typo in the path would otherwise open nothing,
// report zero records, and look indistinguishable from an empty store.
//
// A file that exists but cannot be read is a different, genuine failure — a
// permissions problem or a corrupted file — and keeps its own message rather
// than being folded into the "does not exist" case.
func checkStoreExists(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf(`database %s does not exist; run "mecp init" to create it`, path)
		}
		return fmt.Errorf(`failed to open database %s: %w`, path, err)
	}
	return f.Close()
}

func dsn(path string, cfg openConfig) string {
	if path == ":memory:" {
		path = ":memory:"
	}
	params := url.Values{}
	params.Add("_pragma", "busy_timeout("+fmt.Sprint(cfg.busyTimeout.Milliseconds())+")")
	params.Add("_pragma", "foreign_keys(1)")
	if cfg.readOnly {
		params.Add("mode", "ro")
	} else {
		params.Add("_pragma", "journal_mode(WAL)")
	}
	return "file:" + path + "?" + params.Encode()
}

// DB exposes the underlying handle for backup and diagnostics.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

func (s *Store) Close() error { return s.db.Close() }

// ContentVersion returns a token that changes whenever a record is written or
// removed. It participates in context-pack cache keys.
func (s *Store) ContentVersion(ctx context.Context) (string, error) {
	var (
		count   int64
		newest  sql.NullString
		version sql.NullInt64
	)
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(updated_at) FROM records`)
	if err := row.Scan(&count, &newest); err != nil {
		return "", fmt.Errorf(`failed to read content version: %w`, err)
	}
	row = s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`)
	if err := row.Scan(&version); err != nil {
		return "", fmt.Errorf(`failed to read schema version: %w`, err)
	}

	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d", count, newest.String, version.Int64)))
	return hex.EncodeToString(sum[:8]), nil
}

// withTx runs fn inside a transaction, rolling back on any error.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if s.readOnly {
		return fmt.Errorf(`database is open read-only`)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf(`failed to begin transaction: %w`, err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// formatTime renders a timestamp so that lexical ordering matches chronological
// ordering, which is what the SQL comparisons rely on.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func parseTimePtr(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// placeholders renders "?, ?, ?" for an IN clause of n values.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

var _ mecp.Store = (*Store)(nil)
