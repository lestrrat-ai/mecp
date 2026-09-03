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

func initCommand() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "write a default configuration and create the database",
		Description: `Creates config.yaml and an empty database with owner-only permissions, and
prints the MCP server entry to add to an agent host.`,
		Flags: append(globalFlags(),
			&cli.StringFlag{Name: "principal", Usage: "who this store belongs to", Value: "local-user"},
			&cli.BoolFlag{Name: "force", Usage: "overwrite an existing configuration"},
		),
		Action: runInit,
	}
}

func runInit(ctx context.Context, cmd *cli.Command) error {
	path := cmd.String("config")
	if path == "" {
		path = config.DefaultConfigPath()
	}

	if _, err := os.Stat(path); err == nil && !cmd.Bool("force") {
		return fmt.Errorf(`%s already exists; pass --force to overwrite it`, path)
	}

	cfg := config.Default()
	cfg.Principal = cmd.String("principal")
	if db := cmd.String("database"); db != "" {
		cfg.Database = db
	}
	if err := cfg.Save(path); err != nil {
		return err
	}

	store, err := sqlite.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}

	fmt.Printf("configuration  %s\n", path)
	fmt.Printf("database       %s\n", cfg.Database)
	fmt.Printf("audit log      %s\n", cfg.AuditLog)
	fmt.Printf(`
Add this to an MCP host's server configuration:

  {
    "mcpServers": {
      "mecp": {
        "command": %q,
        "args": ["mcp", "--client", "default"]
      }
    }
  }

Then tell the agent to call context_prepare_task before nontrivial tasks.
`, executablePath())
	return nil
}

func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "mecp"
	}
	return exe
}

func validateCommand() *cli.Command {
	return &cli.Command{
		Name:  "validate",
		Usage: "re-check every record's freshness policy",
		Description: `Runs each record's validation policy and reports what no longer holds. With
--apply, records that fail are marked stale so that they stop acting as
guidance until you re-verify them with "mecp record verify".

Git and content-hash policies need validation.git enabled in the configuration
and a workspace to check against.`,
		Flags: append(append(globalFlags(), workspaceFlags()...),
			&cli.BoolFlag{Name: "apply", Usage: "mark failing records stale"},
			&cli.BoolFlag{Name: "all", Usage: "also check records that are not currently active"},
		),
		Action: runValidate,
	}
}

func runValidate(ctx context.Context, cmd *cli.Command) error {
	rt, err := openRuntime(ctx, cmd, !cmd.Bool("apply"))
	if err != nil {
		return err
	}
	defer rt.Close()

	q := mecp.RecordQuery{}
	if !cmd.Bool("all") {
		q.Statuses = []mecp.RecordStatus{mecp.StatusActive}
	}
	recs, err := rt.store.QueryRecords(ctx, q)
	if err != nil {
		return err
	}

	var resolver mecp.SourceResolver
	if rt.cfg.Validation.Git {
		resolver = source.NewGitResolver()
	}
	validator := mecp.NewValidator(resolver)
	ws := workspaceFrom(cmd)
	now := time.Now().UTC()

	var failing int
	for _, rec := range recs {
		status := validator.Validate(ctx, rec, ws, now)
		if status.State == mecp.ValidationValid {
			continue
		}
		failing++
		fmt.Printf("%s  %-12s %s\n", rec.ID, status.State, rec.Subject)
		if status.Reason != "" {
			fmt.Printf("    %s\n", status.Reason)
		}

		if !cmd.Bool("apply") || status.State == mecp.ValidationUnverified {
			continue
		}
		rec.Status = mecp.StatusStale
		rec.UpdatedAt = now
		if err := rt.store.PutRecord(ctx, rec); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "%d of %d record(s) need attention\n", failing, len(recs))
	return nil
}

func auditCommand() *cli.Command {
	return &cli.Command{
		Name:  "audit",
		Usage: "show recent audit events from the configured sink",
		Description: `Reads whichever sink "audit" names in the configuration: the SQLite audit_events
table, or the JSONL file named by audit_log. Events are shown newest first.`,
		Flags: append(globalFlags(),
			&cli.IntFlag{Name: "limit", Usage: "how many events to show", Value: 20},
			&cli.BoolFlag{Name: "json", Usage: "print the raw events"},
			&cli.StringFlag{
				Name:  "since",
				Usage: `drop events older than this: RFC3339, YYYY-MM-DD, or an age such as "24h"`,
			},
		),
		Action: runAudit,
	}
}

func runAudit(ctx context.Context, cmd *cli.Command) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	since, err := parseSinceFlag(cmd, "since")
	if err != nil {
		return err
	}

	reader, closer, err := auditReader(ctx, cfg)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer()
	}
	if reader == nil {
		fmt.Fprintf(os.Stderr, "auditing is off; set audit to \"jsonl\" or \"sqlite\" in %s\n", cfg.Path())
		return nil
	}

	q := mecp.AuditQuery{Limit: cmd.Int("limit")}
	if since != nil {
		q.Since = *since
	}
	events, err := reader.AuditEvents(ctx, q)
	if err != nil {
		return err
	}
	noteSplitAuditTrail(cfg)

	if cmd.Bool("json") {
		return printJSON(events)
	}
	if len(events) == 0 {
		fmt.Printf("No matching audit events in %s.\n", auditLocation(cfg))
		return nil
	}
	// The origin sits next to the client profile because the two together say
	// who asked: the same profile appears on an agent's call over MCP and on a
	// "mecp prepare --client ..." run that reproduces it. An event written
	// before origins were recorded prints as "unknown".
	for _, ev := range events {
		fmt.Printf("%s  %-16s %-16s %-8s %d record(s) %dms %s\n",
			ev.At.Format(time.RFC3339), ev.Operation, ev.ClientID, ev.Origin,
			ev.ResultCount, ev.LatencyMS, ev.ErrorCode)
	}
	return nil
}

// auditReader opens a reader over whichever sink the configuration selects. A
// nil reader means auditing is off. The returned function releases whatever was
// opened, and is nil when nothing was.
func auditReader(ctx context.Context, cfg *config.Config) (mecp.AuditReader, func() error, error) {
	switch cfg.Audit {
	case "", "none":
		return nil, nil, nil
	case "jsonl":
		return mecp.NewJSONLAuditReader(auditLogPath(cfg)), nil, nil
	case "sqlite":
		store, err := sqlite.Open(cfg.Database, sqlite.WithReadOnly(true))
		if err != nil {
			return nil, nil, err
		}
		if err := store.Migrate(ctx); err != nil {
			store.Close()
			return nil, nil, err
		}
		return store, store.Close, nil
	default:
		return nil, nil, fmt.Errorf(`unknown audit sink %q`, cfg.Audit)
	}
}

// auditLocation names where the events were read from, so that an empty result
// says which file or database was actually looked at.
func auditLocation(cfg *config.Config) string {
	if cfg.Audit == "sqlite" {
		return cfg.Database
	}
	return auditLogPath(cfg)
}

// noteSplitAuditTrail warns when the SQLite sink is configured but the JSONL log
// also holds events. A read-only process falls back to the JSONL log, so the
// trail can be split across both and this command only reads one of them.
func noteSplitAuditTrail(cfg *config.Config) {
	if cfg.Audit != "sqlite" {
		return
	}
	info, err := os.Stat(auditLogPath(cfg))
	if err != nil || info.Size() == 0 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"mecp: %s also holds events, written whenever a read-only process could not use the SQLite sink\n",
		auditLogPath(cfg))
}

// parseSinceFlag reads a lower time bound written either as an instant or as an
// age. An age is the natural way to ask for recent activity, and the audit trail
// is the one place a relative bound is what the user means.
func parseSinceFlag(cmd *cli.Command, name string) (*time.Time, error) {
	raw := cmd.String(name)
	if raw == "" {
		return nil, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d < 0 {
			d = -d
		}
		cutoff := time.Now().UTC().Add(-d)
		return &cutoff, nil
	}
	t, err := parseTimeFlag(cmd, name)
	if err != nil {
		return nil, fmt.Errorf(`--%s must be RFC3339, YYYY-MM-DD, or a duration such as "24h", got %q`, name, raw)
	}
	return t, nil
}
