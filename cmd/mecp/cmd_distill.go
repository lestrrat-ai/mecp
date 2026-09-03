package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/source"
	"github.com/urfave/cli/v3"
)

func distillCommand() *cli.Command {
	return &cli.Command{
		Name:      "distill",
		Usage:     "turn a Markdown instruction file into candidate records",
		ArgsUsage: "<path>...",
		Description: `Reads CLAUDE.md, AGENTS.md, or any Markdown rules document and writes candidate
records as YAML for you to edit, then import.

Nothing is written to the store. The parser takes the structure the document
already has: a bullet is a rule, a table row is a rule, and the nearest heading
is the subject. Prose paragraphs are counted and skipped rather than guessed at,
and the count is reported so you know what was left behind.

Scope is not inferred, because guessing it from prose produces records that
match the wrong work. One document normally covers one area, so set the scope
for the whole file with --path, --task-kind, --repository, or --condition, and
correct individual records in the YAML afterwards.

Every record keeps the original line verbatim as evidence, so a review can see
what the parser changed.`,
		Flags: append(globalFlags(),
			&cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "write to a file instead of stdout"},
			&cli.StringFlag{Name: "authority", Usage: "authority to claim: " + joinAuthorities(), Value: string(mecp.AuthorityImport)},
			&cli.BoolFlag{Name: "mine", Usage: "shorthand for --authority explicit_user, for documents you wrote"},
			&cli.StringFlag{Name: "repository", Aliases: []string{"r"}, Usage: "scope every record to a repository"},
			&cli.BoolFlag{Name: "here", Usage: "scope every record to the repository in the current directory"},
			&cli.StringSliceFlag{Name: "path", Usage: "scope every record to a path pattern (repeatable)"},
			&cli.StringSliceFlag{Name: "task-kind", Usage: "scope every record to a task kind (repeatable)"},
			&cli.StringSliceFlag{Name: "condition", Usage: "scope every record to a key=value condition (repeatable)"},
		),
		Action: runDistill,
	}
}

func runDistill(_ context.Context, cmd *cli.Command) error {
	paths := cmd.Args().Slice()
	if len(paths) == 0 {
		return fmt.Errorf(`at least one path is required`)
	}

	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}

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

	scope, err := distillScope(cmd)
	if err != nil {
		return err
	}

	d := source.NewDistiller(cfg.Principal)
	d.Authority = authority
	d.Scope = scope

	var (
		all     []*mecp.Record
		skipped int
	)
	for _, path := range paths {
		res, err := d.Distill(path)
		if err != nil {
			return err
		}
		all = append(all, res.Records...)
		skipped += res.SkippedParagraphs

		fmt.Fprintf(os.Stderr, "%s: %d rule(s) from %d section(s), %d prose paragraph(s) skipped\n",
			path, len(res.Records), res.Sections, res.SkippedParagraphs)
		if len(res.SkippedLines) > 0 {
			fmt.Fprintf(os.Stderr, "  skipped prose begins at line(s): %s\n", joinInts(res.SkippedLines))
		}
	}

	if err := writeDistilled(cmd.String("out"), all); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n%d candidate record(s). Edit the scope of each, then import with:\n", len(all))
	target := cmd.String("out")
	if target == "" {
		target = "<file>"
	}
	fmt.Fprintf(os.Stderr, "  mecp import %s\n", target)
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "%d prose paragraph(s) were not converted. Read them and add by hand what matters.\n", skipped)
	}
	return nil
}

func distillScope(cmd *cli.Command) (mecp.Scope, error) {
	repository := cmd.String("repository")
	if repository == "" && cmd.Bool("here") {
		repository = discoverRemote(mustGetwd())
		if repository == "" {
			return mecp.Scope{}, fmt.Errorf(`--here was given but the current directory has no git remote`)
		}
	}

	conditions, err := parseConditions(cmd.StringSlice("condition"))
	if err != nil {
		return mecp.Scope{}, err
	}

	taskKinds := make([]mecp.TaskKind, 0, len(cmd.StringSlice("task-kind")))
	for _, k := range cmd.StringSlice("task-kind") {
		kind := mecp.TaskKind(k)
		if !kind.Valid() {
			return mecp.Scope{}, fmt.Errorf(`unknown task kind %q`, k)
		}
		taskKinds = append(taskKinds, kind)
	}

	scope := mecp.Scope{
		Repository:   repository,
		PathPatterns: cmd.StringSlice("path"),
		TaskKinds:    taskKinds,
		Conditions:   conditions,
	}
	if err := scope.Validate(); err != nil {
		return mecp.Scope{}, err
	}
	return scope, nil
}

// writeDistilled emits the records in the envelope "mecp import" already reads,
// so the edited file goes straight back in.
func writeDistilled(path string, recs []*mecp.Record) error {
	buf, err := yaml.Marshal(map[string]any{"records": recs})
	if err != nil {
		return fmt.Errorf(`failed to encode records: %w`, err)
	}

	if path == "" {
		_, err := os.Stdout.Write(buf)
		return err
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return fmt.Errorf(`failed to write %s: %w`, path, err)
	}
	return nil
}

func joinInts(in []int) string {
	parts := make([]string, 0, len(in))
	for _, v := range in {
		parts = append(parts, fmt.Sprint(v))
	}
	if len(parts) > 12 {
		return strings.Join(parts[:12], ", ") + ", …"
	}
	return strings.Join(parts, ", ")
}
