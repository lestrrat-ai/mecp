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

// Example_mecp_propose_and_review shows the write path. An agent may only file
// a proposal; the record becomes active when the user approves it, which is
// what keeps an agent's own inference from turning into authority by
// repetition.
func Example_mecp_propose_and_review() {
	dir, err := os.MkdirTemp("", ".tmp-propose-review-*")
	if err != nil {
		fmt.Printf("failed to create a temporary directory: %s\n", err)
		return
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

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

	svc, err := mecp.New(store, mecp.WithClock(mecp.FixedClock{Time: now}))
	if err != nil {
		fmt.Printf("failed to build the service: %s\n", err)
		return
	}

	agent := mecp.Caller{
		PrincipalID:  "local-user",
		ClientID:     "claude-code",
		Capabilities: []mecp.Capability{mecp.CapPrepare, mecp.CapSearch, mecp.CapPropose},
	}

	req := mecp.ProposeRecordRequest{
		Caller: agent,
		// The key makes retries safe: the same suggestion never queues twice.
		ProposalKey: "session-123:decision:controlled-test-commit",
		Kind:        mecp.KindDecision,
		Subject:     "release conformance testing",
		Statement:   "The release process runs the conformance suite against a controlled commit.",
		Rationale:   "The user confirmed that reproducibility comes from choosing a definite commit.",
		Scope:       mecp.Scope{Repository: "https://github.com/lestrrat-go/helium"},
		Evidence: []mecp.Source{{
			Type:         mecp.SourceConversation,
			Locator:      "turn://42",
			ExactExcerpt: "We pin the suite to a definite commit before each release.",
		}},
	}

	first, err := svc.ProposeRecord(ctx, req)
	if err != nil {
		fmt.Printf("failed to propose the record: %s\n", err)
		return
	}
	fmt.Printf("created=%t status=%s\n", first.Created, first.Status)

	repeated, err := svc.ProposeRecord(ctx, req)
	if err != nil {
		fmt.Printf("failed to repeat the proposal: %s\n", err)
		return
	}
	fmt.Printf("created=%t same=%t\n", repeated.Created, repeated.ProposalID == first.ProposalID)

	// The proposal is inert until a person acts on it.
	pending, err := store.QueryProposals(ctx, mecp.ProposalQuery{
		Statuses: []mecp.ProposalStatus{mecp.ProposalPending},
	})
	if err != nil {
		fmt.Printf("failed to list proposals: %s\n", err)
		return
	}
	fmt.Printf("pending=%d\n", len(pending))

	// ApproveProposal is not reachable from any agent-facing transport; only
	// the administrative interface calls it.
	rec, err := mecp.ApproveProposal(ctx, store, pending[0], "local-user", nil, now)
	if err != nil {
		fmt.Printf("failed to approve the proposal: %s\n", err)
		return
	}
	fmt.Printf("activated authority=%s status=%s\n", rec.Authority, rec.Status)

	pack, err := svc.PrepareTask(ctx, mecp.PrepareTaskRequest{
		Caller:    agent,
		Task:      "Cut the next release",
		TaskKind:  mecp.TaskRelease,
		Workspace: mecp.Workspace{Repository: "https://github.com/lestrrat-go/helium"},
	})
	if err != nil {
		fmt.Printf("failed to prepare the task: %s\n", err)
		return
	}
	for _, item := range pack.Items {
		fmt.Printf("%s: %s\n", item.Effect, item.Statement)
	}
	// Output:
	// created=true status=pending_review
	// created=false same=true
	// pending=1
	// activated authority=explicit_user status=active
	// constraint: The release process runs the conformance suite against a controlled commit.
}
