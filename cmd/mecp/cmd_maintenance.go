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
		Usage: "show recent audit events from the SQLite sink",
		Description: `Only reads the SQLite audit table. With the JSONL sink, read the file named by
audit_log in the configuration directly.`,
		Flags: append(globalFlags(),
			&cli.IntFlag{Name: "limit", Value: 20},
			&cli.BoolFlag{Name: "json"},
		),
		Action: runAudit,
	}
}

func runAudit(ctx context.Context, cmd *cli.Command) error {
	rt, err := openRuntime(ctx, cmd, true)
	if err != nil {
		return err
	}
	defer rt.Close()

	events, err := rt.store.AuditEvents(ctx, cmd.Int("limit"))
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		return printJSON(events)
	}
	if len(events) == 0 {
		fmt.Printf("No audit events in the database. The configured sink is %q.\n", rt.cfg.Audit)
		if rt.cfg.Audit == "jsonl" {
			fmt.Printf("Read %s instead.\n", rt.cfg.AuditLog)
		}
		return nil
	}
	for _, ev := range events {
		fmt.Printf("%s  %-16s %-16s %d record(s) %dms %s\n",
			ev.At.Format(time.RFC3339), ev.Operation, ev.ClientID, ev.ResultCount, ev.LatencyMS, ev.ErrorCode)
	}
	return nil
}
