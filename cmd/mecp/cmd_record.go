package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/urfave/cli/v3"
)

func recordCommand() *cli.Command {
	return &cli.Command{
		Name:        "record",
		Usage:       "create, inspect, and retire records",
		Description: `The write path. Agents can only propose; records become active here.`,
		Commands: []*cli.Command{
			recordAddCommand(),
			recordListCommand(),
			recordShowCommand(),
			recordSupersedeCommand(),
			recordStatusCommand(),
			recordVerifyCommand(),
			recordRemoveCommand(),
		},
	}
}

func recordAddCommand() *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "add a record",
		ArgsUsage: "<statement>",
		Flags: append(globalFlags(),
			&cli.StringFlag{Name: "kind", Usage: "one of " + joinRecordKinds(), Value: string(mecp.KindDecision)},
			&cli.StringFlag{Name: "subject", Usage: "short name for what this is about; derived from the statement when omitted"},
			&cli.StringFlag{Name: "rationale", Usage: "why this is the case"},
			&cli.StringFlag{Name: "authority", Usage: "one of " + joinAuthorities(), Value: string(mecp.AuthorityUser)},
			&cli.StringFlag{Name: "repository", Aliases: []string{"r"}, Usage: "scope to a repository; discovered from git when --here is given"},
			&cli.BoolFlag{Name: "here", Usage: "scope to the repository in the current directory"},
			&cli.StringSliceFlag{Name: "branch", Usage: "scope to a branch pattern (repeatable)"},
			&cli.StringSliceFlag{Name: "path", Usage: "scope to a path pattern (repeatable)"},
			&cli.StringSliceFlag{Name: "task-kind", Usage: "scope to a task kind (repeatable)"},
			&cli.StringSliceFlag{Name: "condition", Usage: "scope to a key=value condition (repeatable)"},
			&cli.StringSliceFlag{Name: "tag", Usage: "tag (repeatable)"},
			&cli.StringFlag{Name: "validation", Usage: "freshness policy: " + joinValidationPolicies(), Value: string(mecp.ValidateNone)},
			&cli.StringFlag{Name: "review-after", Usage: "mark stale after this date (RFC3339 or YYYY-MM-DD)"},
			&cli.StringFlag{Name: "valid-until", Usage: "stop applying after this date"},
			&cli.StringSliceFlag{Name: "source", Usage: "evidence as type:locator (repeatable)"},
			&cli.StringFlag{Name: "excerpt", Usage: "verbatim excerpt supporting the first source"},
			&cli.StringSliceFlag{Name: "supersedes", Usage: "record ID this replaces (repeatable)"},
		),
		Action: runRecordAdd,
	}
}

func runRecordAdd(ctx context.Context, cmd *cli.Command) error {
	statement := strings.Join(cmd.Args().Slice(), " ")
	if statement == "" {
		return fmt.Errorf(`a statement is required`)
	}

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	repository := cmd.String("repository")
	if repository == "" && cmd.Bool("here") {
		repository = discoverRemote(mustGetwd())
		if repository == "" {
			return fmt.Errorf(`--here was given but the current directory has no git remote`)
		}
	}

	conditions, err := parseConditions(cmd.StringSlice("condition"))
	if err != nil {
		return err
	}

	taskKinds := make([]mecp.TaskKind, 0, len(cmd.StringSlice("task-kind")))
	for _, k := range cmd.StringSlice("task-kind") {
		kind := mecp.TaskKind(k)
		if !kind.Valid() {
			return fmt.Errorf(`unknown task kind %q`, k)
		}
		taskKinds = append(taskKinds, kind)
	}

	reviewAfter, err := parseTimeFlag(cmd, "review-after")
	if err != nil {
		return err
	}
	validUntil, err := parseTimeFlag(cmd, "valid-until")
	if err != nil {
		return err
	}

	sources, err := parseSources(cmd.StringSlice("source"), cmd.String("excerpt"))
	if err != nil {
		return err
	}

	rec := &mecp.Record{
		ID:        mecp.NewID("rec"),
		Kind:      mecp.RecordKind(cmd.String("kind")),
		Subject:   cmd.String("subject"),
		Statement: statement,
		Rationale: cmd.String("rationale"),
		Scope: mecp.Scope{
			User:           rt.cfg.Principal,
			Repository:     repository,
			BranchPatterns: cmd.StringSlice("branch"),
			PathPatterns:   cmd.StringSlice("path"),
			TaskKinds:      taskKinds,
			Conditions:     conditions,
		},
		Authority:        mecp.Authority(cmd.String("authority")),
		ValidationPolicy: mecp.ValidationPolicy(cmd.String("validation")),
		ReviewAfter:      reviewAfter,
		ValidUntil:       validUntil,
		Tags:             cmd.StringSlice("tag"),
		Sources:          sources,
		Supersedes:       cmd.StringSlice("supersedes"),
	}
	if rec.Subject == "" {
		rec.Subject = firstClause(statement)
	}

	now := time.Now().UTC()
	rec.Normalize(now)
	if err := rec.Validate(); err != nil {
		return err
	}
	if err := rt.store.PutRecord(ctx, rec); err != nil {
		return err
	}
	if err := markSuperseded(ctx, rt, rec, now); err != nil {
		return err
	}

	fmt.Println(rec.ID)
	return nil
}

// markSuperseded retires the records a new record replaces. History is kept:
// the old record stays readable, it just stops being guidance.
func markSuperseded(ctx context.Context, rt *runtime, rec *mecp.Record, now time.Time) error {
	for _, id := range rec.Supersedes {
		old, err := rt.store.GetRecord(ctx, id)
		if err != nil {
			return fmt.Errorf(`cannot supersede %s: %w`, id, err)
		}
		old.SupersededBy = rec.ID
		old.Status = mecp.StatusSuperseded
		old.UpdatedAt = now
		if err := rt.store.PutRecord(ctx, old); err != nil {
			return err
		}
	}
	return nil
}

func recordListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list records",
		Flags: append(globalFlags(),
			&cli.StringSliceFlag{Name: "kind", Usage: "restrict to a record kind (repeatable)"},
			&cli.StringSliceFlag{Name: "status", Usage: "restrict to a lifecycle status (repeatable)"},
			&cli.StringFlag{Name: "repository", Aliases: []string{"r"}, Usage: "restrict to a repository"},
			&cli.StringSliceFlag{Name: "tag", Usage: "restrict to a tag (repeatable)"},
			&cli.IntFlag{Name: "limit", Value: 50},
			&cli.BoolFlag{Name: "json"},
		),
		Action: runRecordList,
	}
}

func runRecordList(ctx context.Context, cmd *cli.Command) error {
	rt, err := openRuntime(ctx, cmd, true)
	if err != nil {
		return err
	}
	defer rt.Close()

	q := mecp.RecordQuery{Limit: cmd.Int("limit"), Tags: cmd.StringSlice("tag")}
	for _, k := range cmd.StringSlice("kind") {
		q.Kinds = append(q.Kinds, mecp.RecordKind(k))
	}
	for _, s := range cmd.StringSlice("status") {
		q.Statuses = append(q.Statuses, mecp.RecordStatus(s))
	}
	if repo := cmd.String("repository"); repo != "" {
		q.RestrictRepositories = true
		q.Repositories = []string{mecp.CanonicalRepository(repo)}
	}

	recs, err := rt.store.QueryRecords(ctx, q)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		return printJSON(recs)
	}

	if len(recs) == 0 {
		fmt.Println("No records.")
		return nil
	}
	for _, rec := range recs {
		fmt.Printf("%s  %-22s %-10s %-24s %s\n", rec.ID, rec.Kind, rec.Status, rec.Scope.SpecificityLabel(), rec.Subject)
	}
	return nil
}

func recordShowCommand() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Usage:     "show one record in full, including evidence",
		ArgsUsage: "<record-id>...",
		Flags:     append(globalFlags(), &cli.BoolFlag{Name: "json"}),
		Action:    runRecordShow,
	}
}

func runRecordShow(ctx context.Context, cmd *cli.Command) error {
	ids := cmd.Args().Slice()
	if len(ids) == 0 {
		return fmt.Errorf(`at least one record ID is required`)
	}

	rt, err := openRuntime(ctx, cmd, true)
	if err != nil {
		return err
	}
	defer rt.Close()

	res, err := rt.svc.GetRecords(ctx, mecp.GetRecordsRequest{
		Caller:          rt.cfg.AdminCaller().WithOrigin(mecp.OriginCLI),
		RecordIDs:       ids,
		IncludeEvidence: true,
	})
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		return printJSON(res)
	}

	for _, rec := range res.Records {
		fmt.Printf("%s\n", rec.RecordID)
		fmt.Printf("  kind        %s (%s)\n", rec.Kind, rec.Effect)
		fmt.Printf("  subject     %s\n", rec.Subject)
		fmt.Printf("  statement   %s\n", rec.Statement)
		if rec.Rationale != "" {
			fmt.Printf("  rationale   %s\n", rec.Rationale)
		}
		fmt.Printf("  authority   %s\n", rec.Authority)
		fmt.Printf("  status      %s\n", rec.Status)
		fmt.Printf("  scope       %s\n", describeScope(rec.Scope))
		fmt.Printf("  validation  %s (%s)\n", rec.Validation.State, rec.ValidationPolicy)
		if rec.Validation.Reason != "" {
			fmt.Printf("              %s\n", rec.Validation.Reason)
		}
		if len(rec.Supersedes) > 0 {
			fmt.Printf("  supersedes  %s\n", strings.Join(rec.Supersedes, ", "))
		}
		if len(rec.SupersededBy) > 0 {
			fmt.Printf("  replaced by %s\n", strings.Join(rec.SupersededBy, ", "))
		}
		for _, src := range rec.Sources {
			fmt.Printf("  source      %s %s\n", src.Type, src.Locator)
			if src.Excerpt != "" {
				fmt.Printf("              %q\n", src.Excerpt)
			}
		}
		fmt.Println()
	}
	printWarnings(res.Warnings)
	return nil
}

func recordSupersedeCommand() *cli.Command {
	return &cli.Command{
		Name:      "supersede",
		Usage:     "replace a record with a new one, preserving the history",
		ArgsUsage: "<old-record-id> <new statement>",
		Flags: append(globalFlags(),
			&cli.StringFlag{Name: "rationale"},
			&cli.StringFlag{Name: "kind", Usage: "defaults to the superseded record's kind"},
		),
		Action: runRecordSupersede,
	}
}

func runRecordSupersede(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) < 2 {
		return fmt.Errorf(`a record ID and a replacement statement are required`)
	}
	oldID, statement := args[0], strings.Join(args[1:], " ")

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	old, err := rt.store.GetRecord(ctx, oldID)
	if err != nil {
		return err
	}

	kind := old.Kind
	if k := cmd.String("kind"); k != "" {
		kind = mecp.RecordKind(k)
	}

	now := time.Now().UTC()
	replacement := &mecp.Record{
		ID:               mecp.NewID("rec"),
		Kind:             kind,
		Subject:          old.Subject,
		Statement:        statement,
		Rationale:        cmd.String("rationale"),
		Scope:            old.Scope.Clone(),
		Authority:        old.Authority,
		ValidationPolicy: old.ValidationPolicy,
		Tags:             old.Tags,
		Supersedes:       []string{oldID},
	}
	replacement.Normalize(now)
	if err := replacement.Validate(); err != nil {
		return err
	}
	if err := rt.store.PutRecord(ctx, replacement); err != nil {
		return err
	}
	if err := markSuperseded(ctx, rt, replacement, now); err != nil {
		return err
	}

	fmt.Println(replacement.ID)
	return nil
}

func recordStatusCommand() *cli.Command {
	return &cli.Command{
		Name:      "status",
		Usage:     "change a record's lifecycle status",
		ArgsUsage: "<record-id> <status>",
		Description: `Valid statuses are ` + joinStatuses() + `.

Marking a record disputed says that credible sources disagree and no resolution
has been recorded; the record stops acting as guidance but stays visible.`,
		Flags:  globalFlags(),
		Action: runRecordStatus,
	}
}

func runRecordStatus(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) != 2 {
		return fmt.Errorf(`a record ID and a status are required`)
	}
	status := mecp.RecordStatus(args[1])
	if !status.Valid() {
		return fmt.Errorf(`unknown status %q; valid statuses are %s`, args[1], joinStatuses())
	}

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	rec, err := rt.store.GetRecord(ctx, args[0])
	if err != nil {
		return err
	}
	rec.Status = status
	rec.UpdatedAt = time.Now().UTC()
	if err := rt.store.PutRecord(ctx, rec); err != nil {
		return err
	}

	fmt.Printf("%s is now %s\n", rec.ID, status)
	return nil
}

func recordVerifyCommand() *cli.Command {
	return &cli.Command{
		Name:      "verify",
		Usage:     "record that you have re-checked a record and it is still true",
		ArgsUsage: "<record-id>...",
		Description: `Sets last_verified_at to now and clears a passed review date by pushing it
forward, which is what takes a record out of the stale state.`,
		Flags: append(globalFlags(),
			&cli.StringFlag{Name: "review-again", Usage: "set the next review date (RFC3339 or YYYY-MM-DD)"},
		),
		Action: runRecordVerify,
	}
}

func runRecordVerify(ctx context.Context, cmd *cli.Command) error {
	ids := cmd.Args().Slice()
	if len(ids) == 0 {
		return fmt.Errorf(`at least one record ID is required`)
	}

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	next, err := parseTimeFlag(cmd, "review-again")
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, id := range ids {
		rec, err := rt.store.GetRecord(ctx, id)
		if err != nil {
			return err
		}
		rec.LastVerifiedAt = &now
		rec.ReviewAfter = next
		if rec.Status == mecp.StatusStale {
			rec.Status = mecp.StatusActive
		}
		rec.UpdatedAt = now
		if err := rt.store.PutRecord(ctx, rec); err != nil {
			return err
		}
		fmt.Printf("%s verified\n", id)
	}
	return nil
}

func recordRemoveCommand() *cli.Command {
	return &cli.Command{
		Name:      "rm",
		Usage:     "delete a record permanently",
		ArgsUsage: "<record-id>...",
		Description: `Deletion removes the record from the search index and every relationship, not
just from listings. Prefer "record status archived" when you want to keep the
history.`,
		Flags:  append(globalFlags(), &cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"}),
		Action: runRecordRemove,
	}
}

func runRecordRemove(ctx context.Context, cmd *cli.Command) error {
	ids := cmd.Args().Slice()
	if len(ids) == 0 {
		return fmt.Errorf(`at least one record ID is required`)
	}
	if !cmd.Bool("yes") {
		fmt.Fprintf(os.Stderr, "This permanently deletes %d record(s). Re-run with --yes to confirm.\n", len(ids))
		return fmt.Errorf(`refusing to delete without --yes`)
	}

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	for _, id := range ids {
		if err := rt.store.DeleteRecord(ctx, id); err != nil {
			return err
		}
		fmt.Printf("%s deleted\n", id)
	}
	return nil
}

func parseSources(specs []string, excerpt string) ([]mecp.Source, error) {
	out := make([]mecp.Source, 0, len(specs))
	for i, spec := range specs {
		typ, locator, ok := strings.Cut(spec, ":")
		if !ok || locator == "" {
			return nil, fmt.Errorf(`--source expects type:locator, got %q`, spec)
		}
		sourceType := mecp.SourceType(typ)
		if !sourceType.Valid() {
			return nil, fmt.Errorf(`unknown source type %q in %q`, typ, spec)
		}
		src := mecp.Source{ID: mecp.NewID("src"), Type: sourceType, Locator: locator}
		if i == 0 {
			src.ExactExcerpt = excerpt
		}
		out = append(out, src)
	}
	return out, nil
}

func describeScope(s mecp.Scope) string {
	var parts []string
	if s.Repository != "" {
		parts = append(parts, "repository="+s.Repository)
	}
	if len(s.BranchPatterns) > 0 {
		parts = append(parts, "branches="+strings.Join(s.BranchPatterns, "|"))
	}
	if len(s.PathPatterns) > 0 {
		parts = append(parts, "paths="+strings.Join(s.PathPatterns, "|"))
	}
	if len(s.TaskKinds) > 0 {
		kinds := make([]string, 0, len(s.TaskKinds))
		for _, k := range s.TaskKinds {
			kinds = append(kinds, string(k))
		}
		parts = append(parts, "tasks="+strings.Join(kinds, "|"))
	}
	for k, v := range s.Conditions {
		parts = append(parts, k+"="+v)
	}
	if len(parts) == 0 {
		return "global"
	}
	return strings.Join(parts, " ")
}

func firstClause(statement string) string {
	statement = strings.TrimSpace(statement)
	if idx := strings.IndexAny(statement, ".;\n"); idx > 0 {
		statement = statement[:idx]
	}
	words := strings.Fields(statement)
	if len(words) > 12 {
		words = words[:12]
	}
	return strings.Join(words, " ")
}

func joinRecordKinds() string {
	out := make([]string, 0, len(mecp.AllRecordKinds))
	for _, k := range mecp.AllRecordKinds {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

func joinAuthorities() string {
	out := make([]string, 0, len(mecp.AllAuthorities))
	for _, a := range mecp.AllAuthorities {
		out = append(out, string(a))
	}
	return strings.Join(out, ", ")
}

func joinStatuses() string {
	out := make([]string, 0, len(mecp.AllRecordStatuses))
	for _, s := range mecp.AllRecordStatuses {
		out = append(out, string(s))
	}
	return strings.Join(out, ", ")
}

func joinValidationPolicies() string {
	out := make([]string, 0, len(mecp.AllValidationPolicies))
	for _, p := range mecp.AllValidationPolicies {
		out = append(out, string(p))
	}
	return strings.Join(out, ", ")
}
