package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/config"
	"github.com/lestrrat-ai/mecp/source"
	"github.com/lestrrat-ai/mecp/sqlite"
	"github.com/urfave/cli/v3"
)

// runtime is the wiring shared by every command: configuration, the store, and
// a service built over it.
type runtime struct {
	cfg   *config.Config
	store *sqlite.Store
	svc   mecp.Service
}

// loadConfig reads configuration without touching the database. Commands that
// only need to know the principal, such as a dry-run import, use this so that
// they work before "mecp init" has been run.
func loadConfig(cmd *cli.Command) (*config.Config, error) {
	cfg, err := config.Load(cmd.String("config"))
	if err != nil {
		return nil, err
	}
	if db := cmd.String("database"); db != "" {
		cfg.Database = db
	}
	return cfg, nil
}

// openRuntime loads configuration, opens the database, and builds a service.
//
// readOnly opens the database without write access. Agent-facing processes use
// it so that a bug in the gateway cannot corrupt the store; the administrative
// commands and the proposal path need write access.
func openRuntime(ctx context.Context, cmd *cli.Command, readOnly bool) (*runtime, error) {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return nil, err
	}

	store, err := sqlite.Open(cfg.Database, sqlite.WithReadOnly(readOnly))
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		store.Close()
		return nil, err
	}

	options := []mecp.ServiceOption{
		mecp.WithValidationTTL(cfg.Validation.TTL.Duration()),
		mecp.WithContextTTL(cfg.Defaults.ContextTTL.Duration()),
	}
	if cfg.Defaults.MaxCandidates > 0 {
		options = append(options, mecp.WithMaxCandidates(cfg.Defaults.MaxCandidates))
	}
	if len(cfg.RepositoryAliases) > 0 {
		options = append(options, mecp.WithRepositoryAliases(cfg.RepositoryAliases))
	}
	if cfg.Validation.Git {
		options = append(options, mecp.WithSourceResolver(source.NewGitResolver()))
	}

	sink, err := auditSink(cfg, store, readOnly)
	if err != nil {
		store.Close()
		return nil, err
	}
	options = append(options, mecp.WithAuditSink(sink))

	svc, err := mecp.New(store, options...)
	if err != nil {
		store.Close()
		return nil, err
	}
	return &runtime{cfg: cfg, store: store, svc: svc}, nil
}

func (r *runtime) Close() error {
	if r == nil || r.store == nil {
		return nil
	}
	return r.store.Close()
}

// auditSink selects where audit events go. A read-only store cannot accept the
// SQLite sink, so that configuration falls back to the JSONL file rather than
// silently dropping the audit trail.
func auditSink(cfg *config.Config, store *sqlite.Store, readOnly bool) (mecp.AuditSink, error) {
	kind := cfg.Audit
	if kind == "sqlite" && readOnly {
		fmt.Fprintln(os.Stderr, "mecp: the SQLite audit sink needs write access; falling back to the JSONL log")
		kind = "jsonl"
	}

	switch kind {
	case "", "none":
		return mecp.NopAudit{}, nil
	case "sqlite":
		return sqlite.NewAuditSink(store), nil
	case "jsonl":
		path := cfg.AuditLog
		if path == "" {
			path = config.DefaultAuditPath()
		}
		return mecp.NewJSONLAudit(path)
	default:
		return nil, fmt.Errorf(`unknown audit sink %q`, cfg.Audit)
	}
}

// globalFlags are accepted by every command.
func globalFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "config",
			Usage:   "path to config.yaml (default: the platform configuration directory)",
			Sources: cli.EnvVars("MECP_CONFIG"),
		},
		&cli.StringFlag{
			Name:    "database",
			Usage:   "override the configured database path",
			Sources: cli.EnvVars("MECP_DATABASE"),
		},
	}
}

func parseTimeFlag(cmd *cli.Command, name string) (*time.Time, error) {
	raw := cmd.String(name)
	if raw == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			utc := t.UTC()
			return &utc, nil
		}
	}
	return nil, fmt.Errorf(`--%s must be RFC3339 or YYYY-MM-DD, got %q`, name, raw)
}
