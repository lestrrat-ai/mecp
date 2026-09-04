package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/source"
	"github.com/urfave/cli/v3"
)

func importCommand() *cli.Command {
	return &cli.Command{
		Name:      "import",
		Usage:     "import records from files or a previous export",
		ArgsUsage: "<path>",
		Description: `A .jsonl path is treated as a previous "mecp export" and restored verbatim.
Any other file or directory is read as record source material: YAML files
holding one record, a list, or a "records:" key, and Markdown files with YAML
front matter.

Imported records get sourced_import authority by default. An importer never
assigns explicit_user authority on its own; use --authority when you are
importing material you actually authored.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "authority", Usage: "authority to assign to imported records", Value: string(mecp.AuthorityImport)},
			&cli.BoolFlag{Name: "mine", Usage: `shorthand for --authority explicit_user, for material you wrote yourself`},
			&cli.BoolFlag{Name: "dry-run", Usage: "report what would be imported without writing"},
		},
		Action: runImport,
	}
}

func runImport(ctx context.Context, cmd *cli.Command) error {
	path := cmd.Args().First()
	if path == "" {
		return fmt.Errorf(`a path is required`)
	}

	// A dry run over record files reads nothing from the store, so it works
	// before the database exists.
	dryRun := cmd.Bool("dry-run")
	isJSONL := strings.EqualFold(filepath.Ext(path), ".jsonl")

	if dryRun && !isJSONL {
		cfg, err := loadConfig(cmd)
		if err != nil {
			return err
		}
		return importFiles(cmd, cfg.Principal, path, nil)
	}

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	if isJSONL {
		return importJSONL(ctx, rt, path, dryRun)
	}
	return importFiles(cmd, rt.cfg.Principal, path, func(rec *mecp.Record) error {
		return rt.store.PutRecord(ctx, rec)
	})
}

// importFiles reads record files and hands each record to store, which is nil
// on a dry run.
func importFiles(cmd *cli.Command, principal, path string, store func(*mecp.Record) error) error {
	authority := mecp.Authority(cmd.String("authority"))
	if cmd.Bool("mine") {
		if cmd.IsSet("authority") {
			return fmt.Errorf(`--mine and --authority contradict each other; use one`)
		}
		authority = mecp.AuthorityUser
	}
	if !authority.Valid() {
		return fmt.Errorf(`unknown authority %q`, cmd.String("authority"))
	}
	imp := source.NewFileImporter(principal)
	imp.DefaultAuthority = authority
	imp.Now = time.Now().UTC()

	recs, err := imp.ImportPath(path)
	if err != nil {
		return err
	}

	for _, rec := range recs {
		if store == nil {
			fmt.Printf("would import %-22s %s\n", rec.Kind, rec.Subject)
			continue
		}
		if err := store(rec); err != nil {
			return fmt.Errorf(`failed to import %s: %w`, rec.ID, err)
		}
		fmt.Printf("%s  %s\n", rec.ID, rec.Subject)
	}

	fmt.Fprintf(os.Stderr, "%d record(s)\n", len(recs))
	reportNonDirective(recs)
	return nil
}

// reportNonDirective says when imported records will not act as rules. Import
// authority is deliberately weak, but a silent import leaves the user to work
// out for themselves why their own rules come back as background information.
func reportNonDirective(recs []*mecp.Record) {
	var weak int
	for _, rec := range recs {
		if !rec.Authority.Directive() {
			weak++
		}
	}
	if weak == 0 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"mecp: %d of them carry %s authority, so agents will read them as background information rather than rules.\n"+
			"      Re-import with --mine if you wrote this material, or set authority: in the file itself.\n",
		weak, recs[0].Authority)
}

func importJSONL(ctx context.Context, rt *runtime, path string, dryRun bool) error {
	if dryRun {
		return fmt.Errorf(`--dry-run is not supported for a .jsonl restore`)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf(`failed to open %s: %w`, path, err)
	}
	defer f.Close()

	n, err := source.ImportJSONL(ctx, rt.store, f)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d entr(ies) restored\n", n)
	return nil
}

func exportCommand() *cli.Command {
	return &cli.Command{
		Name:  "export",
		Usage: "export every record as portable JSONL",
		Description: `The export is ordered by ID and carries no index or storage detail, so two
exports of the same data are byte-identical and a restore reproduces the store.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "write to a file instead of stdout"},
			&cli.BoolFlag{Name: "proposals", Usage: "include proposals and their review outcomes"},
		},
		Action: runExport,
	}
}

func runExport(ctx context.Context, cmd *cli.Command) error {
	rt, err := openRuntime(ctx, cmd, true)
	if err != nil {
		return err
	}
	defer rt.Close()

	out := os.Stdout
	if path := cmd.String("out"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf(`failed to create %s: %w`, path, err)
		}
		defer f.Close()
		out = f
	}

	n, err := source.ExportJSONL(ctx, rt.store, out, cmd.Bool("proposals"))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d entr(ies) exported\n", n)
	return nil
}
