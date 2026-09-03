package examples_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/sqlite"
)

// Example_mecp_prepare_task shows the call an agent makes before starting work:
// it names the task and the workspace, and gets back the records that apply,
// each labelled with how much weight to give it.
func Example_mecp_prepare_task() {
	dir, err := os.MkdirTemp("", ".tmp-prepare-task-*")
	if err != nil {
		fmt.Printf("failed to create a temporary directory: %s\n", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()

	store, err := sqlite.Open(filepath.Join(dir, "context.db"))
	if err != nil {
		fmt.Printf("failed to open the store: %s\n", err)
		return
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		fmt.Printf("failed to migrate the store: %s\n", err)
		return
	}

	// A fixed clock keeps the example's freshness checks reproducible.
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	records := []*mecp.Record{
		{
			ID:        "rec_stylesheets",
			Kind:      mecp.KindConstraint,
			Subject:   "untrusted stylesheets",
			Statement: "Untrusted XSLT stylesheets must never be executed during parsing.",
			// A checked-in ADR outranks anything inferred from a conversation.
			Authority: mecp.AuthorityRepository,
			Scope:     mecp.Scope{Repository: "https://github.com/lestrrat-go/helium"},
		},
		{
			ID:        "rec_review_weighting",
			Kind:      mecp.KindPreference,
			Subject:   "pre-v1 review weighting",
			Statement: "Weight implementation correctness above API compatibility before v1.",
			Authority: mecp.AuthorityUser,
			Scope: mecp.Scope{
				Repository: "https://github.com/lestrrat-go/helium",
				TaskKinds:  []mecp.TaskKind{mecp.TaskCodeReview},
			},
		},
		{
			ID:        "rec_guess",
			Kind:      mecp.KindConstraint,
			Subject:   "inferred rule",
			Statement: "The reviewer probably wants shorter functions.",
			// An agent's own inference can be returned, but never as a rule.
			Authority: mecp.AuthorityInferred,
			Scope:     mecp.Scope{Repository: "https://github.com/lestrrat-go/helium"},
		},
	}
	for _, rec := range records {
		rec.Normalize(now)
		if err := store.PutRecord(ctx, rec); err != nil {
			fmt.Printf("failed to store %s: %s\n", rec.ID, err)
			return
		}
	}

	svc, err := mecp.New(store, mecp.WithClock(mecp.FixedClock{Time: now}))
	if err != nil {
		fmt.Printf("failed to build the service: %s\n", err)
		return
	}

	// The caller identity comes from trusted configuration, never from the
	// agent's own arguments.
	caller := mecp.Caller{
		PrincipalID:  "local-user",
		ClientID:     "claude-code",
		Capabilities: []mecp.Capability{mecp.CapPrepare, mecp.CapSearch},
	}

	pack, err := svc.PrepareTask(ctx, mecp.PrepareTaskRequest{
		Caller:   caller,
		Task:     "Review the XMLDSig implementation for production readiness",
		TaskKind: mecp.TaskCodeReview,
		Workspace: mecp.Workspace{
			RootURI:    "file:///work/helium",
			Repository: "git@github.com:lestrrat-go/helium.git",
			Revision:   "8f3b2c1",
			Branch:     "main",
		},
		TokenBudget: 3000,
	})
	if err != nil {
		fmt.Printf("failed to prepare the task: %s\n", err)
		return
	}

	fmt.Println(pack.Scope.Repository)
	for _, item := range pack.Items {
		fmt.Printf("%s: %s\n", item.Effect, item.RecordID)
	}
	// Output:
	// https://github.com/lestrrat-go/helium
	// constraint: rec_stylesheets
	// preference: rec_review_weighting
	// informational: rec_guess
}
