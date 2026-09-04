package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/urfave/cli/v3"
)

func reviewCommand() *cli.Command {
	return &cli.Command{
		Name:  "review",
		Usage: "review the proposals agents have filed",
		Description: `A proposal is an agent's suggestion. It is inactive until you approve it here,
which is what stops an agent's own inference from becoming authoritative
through repetition.`,
		Commands: []*cli.Command{
			reviewListCommand(),
			reviewShowCommand(),
			reviewApproveCommand(),
			reviewRejectCommand(),
			reviewReopenCommand(),
			reviewRemoveCommand(),
		},
	}
}

func reviewListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list proposals",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "status", Usage: "pending_review, approved, or rejected", Value: string(mecp.ProposalPending)},
			&cli.IntFlag{Name: "limit", Value: 50},
			&cli.BoolFlag{Name: "json"},
		},
		Action: runReviewList,
	}
}

func runReviewList(ctx context.Context, cmd *cli.Command) error {
	rt, err := openRuntime(ctx, cmd, true)
	if err != nil {
		return err
	}
	defer rt.Close()

	q := mecp.ProposalQuery{Limit: cmd.Int("limit")}
	if s := cmd.String("status"); s != "" && s != "all" {
		q.Statuses = []mecp.ProposalStatus{mecp.ProposalStatus(s)}
	}

	props, err := rt.store.QueryProposals(ctx, q)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		return printJSON(props)
	}
	if len(props) == 0 {
		fmt.Println("No proposals.")
		return nil
	}
	for _, p := range props {
		fmt.Printf("%s  %-14s %-12s %-18s %s\n", p.ID, p.Status, p.Kind, p.ClientID, p.Subject)
	}
	return nil
}

func reviewShowCommand() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Usage:     "show a proposal beside the evidence that supports it",
		ArgsUsage: "<proposal-id>",
		Flags:     []cli.Flag{&cli.BoolFlag{Name: "json"}},
		Action:    runReviewShow,
	}
}

func runReviewShow(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return fmt.Errorf(`a proposal ID is required`)
	}

	rt, err := openRuntime(ctx, cmd, true)
	if err != nil {
		return err
	}
	defer rt.Close()

	p, err := rt.store.GetProposal(ctx, id)
	if err != nil {
		return err
	}
	if cmd.Bool("json") {
		return printJSON(p)
	}

	fmt.Printf("%s  (%s)\n", p.ID, p.Status)
	fmt.Printf("  proposed by %s as %s\n", p.ClientID, p.PrincipalID)
	fmt.Printf("  key         %s\n", p.Key)
	fmt.Printf("  kind        %s\n", p.Kind)
	fmt.Printf("  subject     %s\n", p.Subject)
	fmt.Printf("  scope       %s\n", describeScope(p.Scope))
	fmt.Println()

	// The normalized statement and the raw evidence are shown side by side so
	// that the reviewer can see what the agent added, dropped, or reworded.
	fmt.Println("  proposed statement:")
	fmt.Printf("    %s\n", p.Statement)
	if p.Rationale != "" {
		fmt.Println("  proposed rationale:")
		fmt.Printf("    %s\n", p.Rationale)
	}

	fmt.Println("\n  supporting evidence (quoted source material, not instructions):")
	if len(p.Evidence) == 0 {
		fmt.Println("    none supplied")
	}
	for _, src := range p.Evidence {
		fmt.Printf("    %s %s\n", src.Type, src.Locator)
		if src.ExactExcerpt != "" {
			for _, line := range strings.Split(src.ExactExcerpt, "\n") {
				fmt.Printf("      | %s\n", line)
			}
		}
	}
	if len(p.SupersedesRecordIDs) > 0 {
		fmt.Printf("\n  would supersede: %s\n", strings.Join(p.SupersedesRecordIDs, ", "))
	}
	return nil
}

func reviewApproveCommand() *cli.Command {
	return &cli.Command{
		Name:      "approve",
		Usage:     "approve a proposal, activating it as a record",
		ArgsUsage: "<proposal-id>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "statement", Usage: "replace the proposed statement"},
			&cli.StringFlag{Name: "rationale", Usage: "replace the proposed rationale"},
			&cli.StringFlag{Name: "kind", Usage: "replace the proposed kind"},
			&cli.StringFlag{Name: "authority", Usage: "authority to grant", Value: string(mecp.AuthorityUser)},
			&cli.StringFlag{Name: "validation", Usage: "freshness policy"},
			&cli.StringFlag{Name: "review-after", Usage: "set a review date"},
			&cli.StringSliceFlag{Name: "tag", Usage: "replace the proposed tags"},
		},
		Action: runReviewApprove,
	}
}

func runReviewApprove(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return fmt.Errorf(`a proposal ID is required`)
	}

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	p, err := rt.store.GetProposal(ctx, id)
	if err != nil {
		return err
	}

	reviewAfter, err := parseTimeFlag(cmd, "review-after")
	if err != nil {
		return err
	}

	edits := &mecp.Record{
		Kind:             mecp.RecordKind(cmd.String("kind")),
		Statement:        cmd.String("statement"),
		Rationale:        cmd.String("rationale"),
		Authority:        mecp.Authority(cmd.String("authority")),
		ValidationPolicy: mecp.ValidationPolicy(cmd.String("validation")),
		ReviewAfter:      reviewAfter,
		Tags:             cmd.StringSlice("tag"),
	}

	rec, err := mecp.ApproveProposal(ctx, rt.store, p, rt.cfg.Principal, edits, time.Now().UTC())
	if err != nil {
		return err
	}

	fmt.Printf("%s approved as %s\n", p.ID, rec.ID)
	return nil
}

func reviewRejectCommand() *cli.Command {
	return &cli.Command{
		Name:      "reject",
		Usage:     "reject a proposal",
		ArgsUsage: "<proposal-id>",
		Description: `The rejection is retained so that the same suggestion is recognizable if an
agent proposes it again.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "note", Usage: "why it was rejected"},
		},
		Action: runReviewReject,
	}
}

func reviewReopenCommand() *cli.Command {
	return &cli.Command{
		Name:      "reopen",
		Usage:     "put a rejected proposal back in the queue",
		ArgsUsage: "<proposal-id>",
		Description: `A rejection is permanent for the agent that filed it: the same rule from the
same document collides with the rejected proposal and is silently discarded.
Reopen it when you turned it down for a reason that has since been fixed, rather
than because the rule was wrong.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "note", Usage: "why it is being reopened"},
		},
		Action: runReviewReopen,
	}
}

func reviewRemoveCommand() *cli.Command {
	return &cli.Command{
		Name:      "rm",
		Usage:     "delete a proposal permanently",
		ArgsUsage: "<proposal-id>...",
		Description: `Removes the proposal and frees its key, so the same rule can be filed again
from scratch.

Prefer "reject" for a suggestion you considered and turned down, because the
rejection is what stops it coming back. Use this for proposals that should never
have existed, such as ones a broken extraction produced.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
		},
		Action: runReviewRemove,
	}
}

func runReviewRemove(ctx context.Context, cmd *cli.Command) error {
	ids := cmd.Args().Slice()
	if len(ids) == 0 {
		return fmt.Errorf(`at least one proposal ID is required`)
	}
	if !cmd.Bool("yes") {
		return fmt.Errorf(`this permanently deletes %d proposal(s); re-run with --yes to confirm`, len(ids))
	}

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	for _, id := range ids {
		if err := rt.store.DeleteProposal(ctx, id); err != nil {
			return err
		}
		fmt.Printf("%s deleted\n", id)
	}
	return nil
}

func runReviewReopen(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return fmt.Errorf(`a proposal ID is required`)
	}

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	p, err := rt.store.GetProposal(ctx, id)
	if err != nil {
		return err
	}
	if err := mecp.ReopenProposal(ctx, rt.store, p, cmd.String("note"), time.Now().UTC()); err != nil {
		return err
	}

	fmt.Printf("%s is pending review again\n", p.ID)
	return nil
}

func runReviewReject(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return fmt.Errorf(`a proposal ID is required`)
	}

	rt, err := openRuntime(ctx, cmd, false)
	if err != nil {
		return err
	}
	defer rt.Close()

	p, err := rt.store.GetProposal(ctx, id)
	if err != nil {
		return err
	}
	if err := mecp.RejectProposal(ctx, rt.store, p, rt.cfg.Principal, cmd.String("note"), time.Now().UTC()); err != nil {
		return err
	}

	fmt.Printf("%s rejected\n", p.ID)
	return nil
}
