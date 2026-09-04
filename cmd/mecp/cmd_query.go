package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/lestrrat-ai/mecp"
	"github.com/urfave/cli/v3"
)

// workspaceFlags describe the workspace to the query commands. They mirror the
// workspace object an agent sends over MCP, so a diagnostic run reproduces
// what the agent would have received.
func workspaceFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "repository", Aliases: []string{"r"}, Usage: "repository URL; discovered from git when omitted"},
		&cli.StringFlag{Name: "root", Usage: "workspace root directory (default: the current directory)"},
		&cli.StringFlag{Name: "revision", Usage: "current commit"},
		&cli.StringFlag{Name: "branch", Usage: "current branch"},
		&cli.StringSliceFlag{Name: "path", Usage: "repository-relative path the task touches (repeatable)"},
		&cli.StringFlag{Name: "task-kind", Usage: "one of " + joinTaskKinds() + "; omit to leave the kind unknown"},
		&cli.StringSliceFlag{Name: "condition", Usage: "extra key=value fact used to match conditional records (repeatable)"},
		&cli.StringFlag{Name: "client", Usage: "evaluate as this client profile instead of the administrator", Value: ""},
	}
}

func prepareCommand() *cli.Command {
	return &cli.Command{
		Name:      "prepare",
		Usage:     "show the context pack an agent would receive for a task",
		ArgsUsage: "<task description>",
		Description: `Runs the same code path as the MCP prepare_task tool and prints the result.

Use it to check what an agent will actually be told before wiring the server
into a host, and to debug why a record did or did not apply.`,
		Flags: append(workspaceFlags(),
			&cli.IntFlag{Name: "budget", Usage: "approximate token budget", Value: mecp.DefaultTokenBudget},
			&cli.BoolFlag{Name: "evidence", Usage: "include a summary of what backs each record"},
			&cli.BoolFlag{Name: "json", Usage: "print the raw context pack"},
		),
		Action: runPrepare,
	}
}

func runPrepare(ctx context.Context, cmd *cli.Command) error {
	task := strings.Join(cmd.Args().Slice(), " ")
	if task == "" {
		return fmt.Errorf(`a task description is required`)
	}

	rt, err := openRuntime(ctx, cmd, true)
	if err != nil {
		return err
	}
	defer rt.Close()

	conditions, err := parseConditions(cmd.StringSlice("condition"))
	if err != nil {
		return err
	}

	pack, err := rt.svc.PrepareTask(ctx, mecp.PrepareTaskRequest{
		Caller:                   callerFor(rt, cmd),
		Task:                     task,
		TaskKind:                 mecp.TaskKind(cmd.String("task-kind")),
		Workspace:                workspaceFrom(cmd),
		Conditions:               conditions,
		TokenBudget:              cmd.Int("budget"),
		IncludeEvidenceSummaries: cmd.Bool("evidence"),
	})
	if err != nil {
		return err
	}

	if cmd.Bool("json") {
		return printJSON(pack)
	}
	printPack(pack)
	return nil
}

func searchCommand() *cli.Command {
	return &cli.Command{
		Name:      "search",
		Usage:     "search stored context",
		ArgsUsage: "<query>",
		Flags: append(workspaceFlags(),
			&cli.StringSliceFlag{Name: "kind", Usage: "restrict to a record kind (repeatable)"},
			&cli.BoolFlag{Name: "include-stale", Usage: "also show superseded and archived records"},
			&cli.IntFlag{Name: "limit", Value: 10},
			&cli.BoolFlag{Name: "json", Usage: "print the raw result"},
		),
		Action: runSearch,
	}
}

func runSearch(ctx context.Context, cmd *cli.Command) error {
	query := strings.Join(cmd.Args().Slice(), " ")
	if query == "" {
		return fmt.Errorf(`a query is required`)
	}

	rt, err := openRuntime(ctx, cmd, true)
	if err != nil {
		return err
	}
	defer rt.Close()

	conditions, err := parseConditions(cmd.StringSlice("condition"))
	if err != nil {
		return err
	}

	kinds := make([]mecp.RecordKind, 0, len(cmd.StringSlice("kind")))
	for _, k := range cmd.StringSlice("kind") {
		kind := mecp.RecordKind(k)
		if !kind.Valid() {
			return fmt.Errorf(`unknown record kind %q`, k)
		}
		kinds = append(kinds, kind)
	}

	ws := workspaceFrom(cmd)
	if ws.Repository == "" && ws.RootURI == "" {
		// The CLI is a diagnostic tool, so it makes the implicit workspace
		// explicit rather than refusing like the agent-facing path does.
		ws.RootURI = "file://" + mustGetwd()
	}

	res, err := rt.svc.Search(ctx, mecp.SearchRequest{
		Caller:       callerFor(rt, cmd),
		Query:        query,
		Workspace:    ws,
		TaskKind:     mecp.TaskKind(cmd.String("task-kind")),
		Conditions:   conditions,
		Kinds:        kinds,
		IncludeStale: cmd.Bool("include-stale"),
		Limit:        cmd.Int("limit"),
	})
	if err != nil {
		return err
	}

	if cmd.Bool("json") {
		return printJSON(res)
	}

	if len(res.Items) == 0 {
		fmt.Println("No records match within this scope.")
	}
	for _, item := range res.Items {
		fmt.Printf("%s  [%s / %s / %s]\n", item.RecordID, item.Kind, item.Effect, item.Authority)
		fmt.Printf("    %s\n", item.Statement)
		if len(item.MatchReasons) > 0 {
			fmt.Printf("    why: %s\n", strings.Join(item.MatchReasons, "; "))
		}
	}
	printWarnings(res.Warnings)
	return nil
}

// callerFor picks the identity a diagnostic command runs as. With no --client
// it uses the administrative identity, which sees everything; with --client it
// reproduces exactly what that agent profile would get.
//
// The identity is reproduced, but the origin is not: a run under --client
// audits as the CLI, so reproducing what an agent sees never leaves a line that
// reads as the agent's own call.
func callerFor(rt *runtime, cmd *cli.Command) mecp.Caller {
	if id := cmd.String("client"); id != "" {
		return rt.cfg.Caller(id).WithOrigin(mecp.OriginCLI)
	}
	return rt.cfg.AdminCaller().WithOrigin(mecp.OriginCLI)
}

func workspaceFrom(cmd *cli.Command) mecp.Workspace {
	root := cmd.String("root")
	if root == "" {
		root = mustGetwd()
	}

	ws := mecp.Workspace{
		RootURI:       "file://" + root,
		Repository:    cmd.String("repository"),
		Revision:      cmd.String("revision"),
		Branch:        cmd.String("branch"),
		RelevantPaths: cmd.StringSlice("path"),
	}
	if ws.Repository == "" {
		ws.Repository = discoverRemote(root)
	}
	if ws.Revision == "" {
		ws.Revision = discoverRevision(root)
	}
	if ws.Branch == "" {
		ws.Branch = discoverBranch(root)
	}
	return ws
}

func parseConditions(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf(`--condition expects key=value, got %q`, pair)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func printPack(pack *mecp.ContextPack) {
	fmt.Println(pack.Summary)
	if pack.Scope.Repository != "" {
		fmt.Printf("Scope: %s", pack.Scope.Repository)
		if pack.Scope.Revision != "" {
			fmt.Printf(" @ %s", pack.Scope.Revision)
		}
		fmt.Println()
	}
	fmt.Println()

	for _, item := range pack.Items {
		fmt.Printf("%s  [%s / %s / %s / %s]\n", item.RecordID, item.Kind, item.Effect, item.Authority, item.Validation)
		fmt.Printf("    %s\n", item.Statement)
		if item.Rationale != "" {
			fmt.Printf("    because: %s\n", item.Rationale)
		}
		if item.EvidenceSummary != "" {
			fmt.Printf("    %s\n", item.EvidenceSummary)
		}
		if len(item.MatchReasons) > 0 {
			fmt.Printf("    why: %s\n", strings.Join(item.MatchReasons, "; "))
		}
		fmt.Println()
	}

	for _, c := range pack.Conflicts {
		fmt.Printf("conflict on %q: %s\n    %s\n", c.Subject, strings.Join(c.RecordIDs, ", "), c.Explanation)
		fmt.Printf("    recommendation: %s\n", c.Recommendation)
	}
	printWarnings(pack.Warnings)

	fmt.Printf("\nApproximately %d of %d tokens used; %d record(s) omitted.\n",
		pack.Budget.EstimatedTokensUsed, pack.Budget.RequestedTokens, pack.Budget.OmittedItemCount)
}

func printWarnings(warnings []mecp.Warning) {
	for _, w := range warnings {
		fmt.Printf("warning %s: %s", w.Code, w.Message)
		if len(w.RecordIDs) > 0 {
			fmt.Printf(" (%s)", strings.Join(w.RecordIDs, ", "))
		}
		fmt.Println()
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func joinTaskKinds() string {
	names := make([]string, 0, len(mecp.AllTaskKinds))
	for _, k := range mecp.AllTaskKinds {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}
