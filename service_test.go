package mecp_test

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/sqlite"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

const heliumRepo = "https://github.com/lestrrat-go/helium"

func newService(t *testing.T, records ...*mecp.Record) (mecp.Service, *sqlite.Store) {
	t.Helper()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	require.NoError(t, store.Migrate(t.Context()))

	for _, rec := range records {
		rec.Normalize(testNow.Add(-24 * time.Hour))
		require.NoError(t, rec.Validate())
		require.NoError(t, store.PutRecord(t.Context(), rec))
	}

	svc, err := mecp.New(store, mecp.WithClock(mecp.FixedClock{Time: testNow}))
	require.NoError(t, err)
	return svc, store
}

func agentCaller() mecp.Caller {
	return mecp.Caller{
		PrincipalID:  "local-user",
		ClientID:     "claude-code",
		Capabilities: []mecp.Capability{mecp.CapPrepare, mecp.CapSearch, mecp.CapEvidence},
	}
}

func heliumWorkspace() mecp.Workspace {
	return mecp.Workspace{
		RootURI:       "file:///work/helium",
		Repository:    "git@github.com:lestrrat-go/helium.git",
		Revision:      "8f3b2c1",
		Branch:        "main",
		RelevantPaths: []string{"xmldsig1/sign.go"},
	}
}

func reviewPreference() *mecp.Record {
	return &mecp.Record{
		ID:        "rec_review_preference_001",
		Kind:      mecp.KindPreference,
		Subject:   "pre-v1 review weighting",
		Statement: "For pre-v1 production-readiness reviews, weight implementation correctness above API compatibility.",
		Scope: mecp.Scope{
			User:       "local-user",
			Repository: heliumRepo,
			TaskKinds:  []mecp.TaskKind{mecp.TaskCodeReview},
		},
		Authority: mecp.AuthorityUser,
	}
}

func stylesheetConstraint() *mecp.Record {
	return &mecp.Record{
		ID:        "rec_stylesheet_constraint",
		Kind:      mecp.KindConstraint,
		Subject:   "untrusted stylesheets",
		Statement: "Untrusted XSLT stylesheets must never be executed during parsing.",
		Scope:     mecp.Scope{Repository: heliumRepo},
		Authority: mecp.AuthorityRepository,
	}
}

func otherProjectRecord() *mecp.Record {
	return &mecp.Record{
		ID:        "rec_other_project",
		Kind:      mecp.KindConstraint,
		Subject:   "unrelated project rule",
		Statement: "The billing service must never log card numbers.",
		Scope:     mecp.Scope{Repository: "https://github.com/example/billing"},
		Authority: mecp.AuthorityProject,
	}
}

func TestPrepareTask(t *testing.T) {
	t.Run("returns scoped records that the query text never mentions", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference(), stylesheetConstraint(), otherProjectRecord())

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:    agentCaller(),
			Task:      "Review the XMLDSig implementation for production readiness",
			TaskKind:  mecp.TaskCodeReview,
			Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)

		ids := itemIDs(pack.Items)
		require.Contains(t, ids, "rec_review_preference_001")
		require.Contains(t, ids, "rec_stylesheet_constraint")
	})

	t.Run("never returns records from another repository", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference(), otherProjectRecord())

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:    agentCaller(),
			Task:      "Review logging of card numbers in the billing service",
			TaskKind:  mecp.TaskCodeReview,
			Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.NotContains(t, itemIDs(pack.Items), "rec_other_project")
	})

	t.Run("canonicalizes the repository and echoes the resolved scope", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference())

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:    agentCaller(),
			Task:      "Review the signature code",
			TaskKind:  mecp.TaskCodeReview,
			Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.Equal(t, heliumRepo, pack.Scope.Repository)
		require.Equal(t, "github.com/lestrrat-go", pack.Scope.Org)
	})

	t.Run("a preference outside the task kind does not apply", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference())

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:    agentCaller(),
			Task:      "Cut the v1 release",
			TaskKind:  mecp.TaskRelease,
			Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.NotContains(t, itemIDs(pack.Items), "rec_review_preference_001")
	})

	t.Run("a repository-scoped record is skipped when no repository is supplied", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference())

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:   agentCaller(),
			Task:     "Review something",
			TaskKind: mecp.TaskCodeReview,
		})
		require.NoError(t, err)
		require.Empty(t, pack.Items)
		require.Contains(t, warningCodes(pack.Warnings), mecp.WarnNoWorkspace)
	})

	t.Run("warns when the named repository matches nothing the store knows", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference())

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:    agentCaller(),
			Task:      "Review something",
			Workspace: mecp.Workspace{Repository: "https://github.com/example/never-seen"},
		})
		require.NoError(t, err)
		require.Contains(t, warningCodes(pack.Warnings), mecp.WarnUnknownRepository)
	})

	t.Run("a known repository with no matching record is not flagged", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference())

		// The record exists for this repository but is scoped to review tasks,
		// so an unrelated task kind filters it out. That is ordinary filtering,
		// not a naming problem, and must not be reported as one.
		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:    agentCaller(),
			Task:      "Cut a release",
			TaskKind:  mecp.TaskRelease,
			Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.NotContains(t, warningCodes(pack.Warnings), mecp.WarnUnknownRepository)
	})

	t.Run("an empty store does not blame the caller's spelling", func(t *testing.T) {
		svc, _ := newService(t)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:    agentCaller(),
			Task:      "Review something",
			Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.NotContains(t, warningCodes(pack.Warnings), mecp.WarnUnknownRepository)
	})

	t.Run("the two spellings of one repository are not reported as unknown", func(t *testing.T) {
		svc, _ := newService(t, stylesheetConstraint())

		for _, spelling := range []string{
			"https://github.com/lestrrat-go/helium",
			"git@github.com:lestrrat-go/helium.git",
			"github.com/lestrrat-go/helium",
		} {
			pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
				Caller:    agentCaller(),
				Task:      "Review the parser",
				Workspace: mecp.Workspace{Repository: spelling},
			})
			require.NoError(t, err)
			require.NotContains(t, warningCodes(pack.Warnings), mecp.WarnUnknownRepository, spelling)
			require.Contains(t, itemIDs(pack.Items), "rec_stylesheet_constraint", spelling)
		}
	})

	t.Run("rejects a budget too small to carry mandatory metadata", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference())

		_, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:      agentCaller(),
			Task:        "Review",
			Workspace:   heliumWorkspace(),
			TokenBudget: 32,
		})
		require.Equal(t, mecp.CodeBudgetTooSmall, mecp.CodeOf(err))
	})

	t.Run("truncates to the requested budget and says so", func(t *testing.T) {
		var records []*mecp.Record
		for i := range 40 {
			records = append(records, &mecp.Record{
				ID:        "rec_bulk_" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
				Kind:      mecp.KindProjectFact,
				Subject:   "bulk fact " + string(rune('a'+i)),
				Statement: "A moderately long project fact that exists only to consume the caller's token budget during packing.",
				Scope:     mecp.Scope{Repository: heliumRepo},
				Authority: mecp.AuthorityImport,
			})
		}
		svc, _ := newService(t, records...)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller:      agentCaller(),
			Task:        "Review the project facts",
			Workspace:   heliumWorkspace(),
			TokenBudget: 512,
		})
		require.NoError(t, err)
		require.True(t, pack.Budget.Truncated)
		require.Greater(t, pack.Budget.OmittedItemCount, 0)
		require.LessOrEqual(t, pack.Budget.EstimatedTokensUsed, 512)
		require.Contains(t, warningCodes(pack.Warnings), mecp.WarnTruncated)
	})

	t.Run("requires the prepare capability", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference())

		caller := agentCaller()
		caller.Capabilities = []mecp.Capability{mecp.CapSearch}
		_, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: caller, Task: "Review", Workspace: heliumWorkspace(),
		})
		require.Equal(t, mecp.CodeUnauthorizedScope, mecp.CodeOf(err))
	})

	t.Run("refuses a repository the client profile may not see", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference())

		caller := agentCaller()
		caller.AllowedRepositories = []string{"https://github.com/example/billing"}
		_, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: caller, Task: "Review", Workspace: heliumWorkspace(),
		})
		require.Equal(t, mecp.CodeUnauthorizedScope, mecp.CodeOf(err))
	})

	t.Run("refuses a workspace outside the configured roots", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference())

		caller := agentCaller()
		caller.AllowedRoots = []string{"/work/other"}
		_, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: caller, Task: "Review", Workspace: heliumWorkspace(),
		})
		require.Equal(t, mecp.CodeUnauthorizedScope, mecp.CodeOf(err))
	})
}

func TestPrepareTaskLifecycle(t *testing.T) {
	t.Run("a superseded record is history, not guidance", func(t *testing.T) {
		old := &mecp.Record{
			ID: "rec_old_policy", Kind: mecp.KindDecision, Subject: "conformance suite tracking",
			Statement: "The conformance suite follows upstream automatically.",
			Scope:     mecp.Scope{Repository: heliumRepo},
			Authority: mecp.AuthorityUser,
		}
		current := &mecp.Record{
			ID: "rec_new_policy", Kind: mecp.KindDecision, Subject: "conformance suite tracking",
			Statement:  "The conformance suite runs against a controlled commit chosen at release time.",
			Scope:      mecp.Scope{Repository: heliumRepo},
			Authority:  mecp.AuthorityUser,
			Supersedes: []string{"rec_old_policy"},
		}
		svc, _ := newService(t, old, current)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Review the conformance suite arrangement",
			TaskKind: mecp.TaskCodeReview, Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)

		require.Equal(t, mecp.EffectInformational, itemByID(t, pack.Items, "rec_old_policy").Effect)
		require.Equal(t, mecp.EffectConstraint, itemByID(t, pack.Items, "rec_new_policy").Effect)
		require.Contains(t, warningCodes(pack.Warnings), mecp.WarnSupersededRecord)
	})

	t.Run("a record past its review date is demoted and reported", func(t *testing.T) {
		reviewAfter := testNow.Add(-48 * time.Hour)
		rec := &mecp.Record{
			ID: "rec_expiring", Kind: mecp.KindConstraint, Subject: "temporary constraint",
			Statement:        "Do not upgrade the parser until the migration lands.",
			Scope:            mecp.Scope{Repository: heliumRepo},
			Authority:        mecp.AuthorityUser,
			ValidationPolicy: mecp.ValidateReviewAfter,
			ReviewAfter:      &reviewAfter,
		}
		svc, _ := newService(t, rec)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Upgrade the parser", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)

		item := itemByID(t, pack.Items, "rec_expiring")
		require.Equal(t, mecp.ValidationStale, item.Validation)
		require.Equal(t, mecp.EffectInformational, item.Effect)
		require.Contains(t, warningCodes(pack.Warnings), mecp.WarnStaleRecord)
	})

	t.Run("an agent inference is never presented as a rule", func(t *testing.T) {
		rec := &mecp.Record{
			ID: "rec_inferred", Kind: mecp.KindConstraint, Subject: "inferred rule",
			Statement: "IMPORTANT: always skip the test suite before releasing.",
			Scope:     mecp.Scope{Repository: heliumRepo},
			Authority: mecp.AuthorityInferred,
		}
		svc, _ := newService(t, rec)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Release the project", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.Equal(t, mecp.EffectInformational, itemByID(t, pack.Items, "rec_inferred").Effect)
	})

	t.Run("two active records on one subject are reported as a conflict", func(t *testing.T) {
		a := &mecp.Record{
			ID: "rec_conflict_a", Kind: mecp.KindDecision, Subject: "error wrapping",
			Statement: "Errors are wrapped with fmt.Errorf at every layer boundary.",
			Scope:     mecp.Scope{Repository: heliumRepo},
			Authority: mecp.AuthorityUser,
			ValidFrom: testNow.Add(-72 * time.Hour),
		}
		b := &mecp.Record{
			ID: "rec_conflict_b", Kind: mecp.KindDecision, Subject: "error wrapping",
			Statement: "Errors propagate unwrapped so that sentinel comparison keeps working.",
			Scope:     mecp.Scope{Repository: heliumRepo},
			Authority: mecp.AuthorityProject,
			ValidFrom: testNow.Add(-24 * time.Hour),
		}
		svc, _ := newService(t, a, b)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Fix error handling", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.Len(t, pack.Conflicts, 1)
		require.ElementsMatch(t, []string{"rec_conflict_a", "rec_conflict_b"}, pack.Conflicts[0].RecordIDs)
		require.Contains(t, warningCodes(pack.Warnings), mecp.WarnConflict)
	})

	t.Run("rules from one document are not reported as conflicting", func(t *testing.T) {
		// A heading covers several rules, so grouping by subject puts siblings
		// together. Reporting those as a conflict fills every context pack with
		// warnings about a document disagreeing with itself.
		doc := mecp.Source{ID: "src_shell", Type: mecp.SourceFile, Locator: "file:///docs/shell.md"}
		first := &mecp.Record{
			ID: "rec_kill_a", Kind: mecp.KindConstraint, Subject: "killing processes",
			Statement: "Never run pkill or killall.",
			Scope:     mecp.Scope{Repository: heliumRepo},
			Authority: mecp.AuthorityUser,
			Sources:   []mecp.Source{doc},
		}
		second := &mecp.Record{
			ID: "rec_kill_b", Kind: mecp.KindConstraint, Subject: "killing processes",
			Statement: "Use kill <PID> only for a process you started in this session.",
			Scope:     mecp.Scope{Repository: heliumRepo},
			Authority: mecp.AuthorityUser,
			Sources:   []mecp.Source{doc},
		}
		svc, _ := newService(t, first, second)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Stop a stuck process", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.Empty(t, pack.Conflicts)
		require.NotContains(t, warningCodes(pack.Warnings), mecp.WarnConflict)
	})

	t.Run("records from different sources still conflict", func(t *testing.T) {
		fromDoc := &mecp.Record{
			ID: "rec_doc", Kind: mecp.KindDecision, Subject: "error wrapping",
			Statement: "Errors are wrapped at every layer boundary.",
			Scope:     mecp.Scope{Repository: heliumRepo},
			Authority: mecp.AuthorityUser,
			Sources:   []mecp.Source{{ID: "s1", Type: mecp.SourceFile, Locator: "file:///docs/go.md"}},
		}
		handWritten := &mecp.Record{
			ID: "rec_hand", Kind: mecp.KindDecision, Subject: "error wrapping",
			Statement: "Errors propagate unwrapped so sentinel comparison keeps working.",
			Scope:     mecp.Scope{Repository: heliumRepo},
			Authority: mecp.AuthorityProject,
		}
		svc, _ := newService(t, fromDoc, handWritten)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Fix error handling", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.Len(t, pack.Conflicts, 1)
	})

	t.Run("deterministic for the same inputs", func(t *testing.T) {
		svc, _ := newService(t, reviewPreference(), stylesheetConstraint())
		req := mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Review the XMLDSig implementation",
			TaskKind: mecp.TaskCodeReview, Workspace: heliumWorkspace(),
		}

		first, err := svc.PrepareTask(t.Context(), req)
		require.NoError(t, err)
		second, err := svc.PrepareTask(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, itemIDs(first.Items), itemIDs(second.Items))
	})
}

func TestGlobalRecords(t *testing.T) {
	global := &mecp.Record{
		ID: "rec_global_pref", Kind: mecp.KindPreference, Subject: "commit message style",
		Statement: "Commit messages are one lowercase line in the imperative mood.",
		Authority: mecp.AuthorityUser,
	}
	svc, _ := newService(t, global, stylesheetConstraint())

	t.Run("a record scoped to nothing applies everywhere", func(t *testing.T) {
		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Commit the parser fix", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.Contains(t, itemIDs(pack.Items), "rec_global_pref")
	})

	t.Run("it applies even with no workspace at all", func(t *testing.T) {
		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Commit something",
		})
		require.NoError(t, err)
		require.Contains(t, itemIDs(pack.Items), "rec_global_pref")
		require.NotContains(t, itemIDs(pack.Items), "rec_stylesheet_constraint")
	})
}

func TestSearch(t *testing.T) {
	svc, _ := newService(t, reviewPreference(), stylesheetConstraint())

	t.Run("refuses a query with neither a handle nor a workspace", func(t *testing.T) {
		_, err := svc.Search(t.Context(), mecp.SearchRequest{Caller: agentCaller(), Query: "stylesheets"})
		require.Equal(t, mecp.CodeInvalidScope, mecp.CodeOf(err))
	})

	t.Run("answers within the scope of a prepared context", func(t *testing.T) {
		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Review the parser", TaskKind: mecp.TaskCodeReview,
			Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)

		res, err := svc.Search(t.Context(), mecp.SearchRequest{
			Caller: agentCaller(), ContextID: pack.ContextID,
			Query: "What did the user say about untrusted stylesheets?",
		})
		require.NoError(t, err)
		require.NotEmpty(t, res.Items)
		require.Equal(t, "rec_stylesheet_constraint", res.Items[0].RecordID)
	})

	t.Run("another client cannot replay a context handle", func(t *testing.T) {
		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Review the parser", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)

		other := agentCaller()
		other.ClientID = "some-other-agent"
		_, err = svc.Search(t.Context(), mecp.SearchRequest{
			Caller: other, ContextID: pack.ContextID, Query: "stylesheets",
		})
		require.Equal(t, mecp.CodeContextExpired, mecp.CodeOf(err))
	})

	t.Run("an expired handle is rejected", func(t *testing.T) {
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "context.db"))
		require.NoError(t, err)
		t.Cleanup(func() { store.Close() })
		require.NoError(t, store.Migrate(t.Context()))

		clock := &steppingClock{now: testNow}
		expiring, err := mecp.New(store, mecp.WithClock(clock), mecp.WithContextTTL(time.Minute))
		require.NoError(t, err)

		pack, err := expiring.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Review", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)

		clock.now = clock.now.Add(2 * time.Minute)
		_, err = expiring.Search(t.Context(), mecp.SearchRequest{
			Caller: agentCaller(), ContextID: pack.ContextID, Query: "stylesheets",
		})
		require.Equal(t, mecp.CodeContextExpired, mecp.CodeOf(err))
	})

	t.Run("requires a search capability", func(t *testing.T) {
		caller := agentCaller()
		caller.Capabilities = []mecp.Capability{mecp.CapPrepare}
		_, err := svc.Search(t.Context(), mecp.SearchRequest{
			Caller: caller, Query: "stylesheets", Workspace: heliumWorkspace(),
		})
		require.Equal(t, mecp.CodeUnauthorizedScope, mecp.CodeOf(err))
	})
}

func TestGetRecords(t *testing.T) {
	withEvidence := &mecp.Record{
		ID: "rec_with_evidence", Kind: mecp.KindDecision, Subject: "conformance commit",
		Statement: "The suite runs against a controlled commit.",
		Scope:     mecp.Scope{Repository: heliumRepo},
		Authority: mecp.AuthorityUser,
		Sources: []mecp.Source{{
			ID: "src_convo", Type: mecp.SourceConversation, Locator: "conversation://2026-07-03",
			ExactExcerpt: "The test repository is executed before releases against a definite commit.",
		}},
	}
	svc, _ := newService(t, withEvidence)

	t.Run("returns evidence the client is allowed to see", func(t *testing.T) {
		res, err := svc.GetRecords(t.Context(), mecp.GetRecordsRequest{
			Caller: agentCaller(), RecordIDs: []string{"rec_with_evidence"}, IncludeEvidence: true,
		})
		require.NoError(t, err)
		require.Len(t, res.Records, 1)
		require.Contains(t, res.Records[0].Sources[0].Excerpt, "definite commit")
	})

	t.Run("withholds quoted source text from a client without the evidence capability", func(t *testing.T) {
		caller := agentCaller()
		caller.Capabilities = []mecp.Capability{mecp.CapPrepare, mecp.CapSearch}

		res, err := svc.GetRecords(t.Context(), mecp.GetRecordsRequest{
			Caller: caller, RecordIDs: []string{"rec_with_evidence"}, IncludeEvidence: true,
		})
		require.NoError(t, err)
		require.Len(t, res.Records, 1)
		require.Empty(t, res.Records[0].Sources[0].Excerpt)
		require.True(t, res.Records[0].Sources[0].Redacted, "the absence must be marked, not silent")
		require.Contains(t, warningCodes(res.Warnings), mecp.WarnEvidenceRedacted)

		require.Equal(t, "conversation://2026-07-03", res.Records[0].Sources[0].Locator,
			"the locator still tells the user where to look")
	})

	t.Run("bounds excerpt length", func(t *testing.T) {
		res, err := svc.GetRecords(t.Context(), mecp.GetRecordsRequest{
			Caller: agentCaller(), RecordIDs: []string{"rec_with_evidence"},
			IncludeEvidence: true, MaxEvidenceCharactersPerRecord: 10,
		})
		require.NoError(t, err)
		require.Len(t, []rune(res.Records[0].Sources[0].Excerpt), 10)
		require.True(t, res.Records[0].Sources[0].Truncated)
	})

	t.Run("omits evidence entirely when not requested", func(t *testing.T) {
		res, err := svc.GetRecords(t.Context(), mecp.GetRecordsRequest{
			Caller: agentCaller(), RecordIDs: []string{"rec_with_evidence"},
		})
		require.NoError(t, err)
		require.Empty(t, res.Records[0].Sources[0].Excerpt)
		require.Equal(t, "conversation://2026-07-03", res.Records[0].Sources[0].Locator)
	})

	t.Run("reports unknown IDs rather than failing", func(t *testing.T) {
		res, err := svc.GetRecords(t.Context(), mecp.GetRecordsRequest{
			Caller: agentCaller(), RecordIDs: []string{"rec_with_evidence", "rec_does_not_exist"},
		})
		require.NoError(t, err)
		require.Len(t, res.Records, 1)
		require.Contains(t, warningCodes(res.Warnings), mecp.WarnRecordNotFound)
	})
}

func TestProposeRecord(t *testing.T) {
	svc, store := newService(t)

	proposeReq := mecp.ProposeRecordRequest{
		Caller:      proposingCaller(),
		ProposalKey: "session-123:decision:controlled-test-commit",
		Kind:        mecp.KindDecision,
		Statement:   "The release process runs the conformance suite against a controlled commit.",
		Rationale:   "The user confirmed reproducibility comes from selecting a definite commit.",
		Scope:       mecp.Scope{Repository: heliumRepo},
		Evidence: []mecp.Source{{
			Type: mecp.SourceConversation, Locator: "turn://42",
		}},
	}

	t.Run("is refused without the propose capability", func(t *testing.T) {
		req := proposeReq
		req.Caller = agentCaller()
		_, err := svc.ProposeRecord(t.Context(), req)
		require.Equal(t, mecp.CodeProposalDisabled, mecp.CodeOf(err))
	})

	t.Run("creates a pending proposal that changes no active context", func(t *testing.T) {
		res, err := svc.ProposeRecord(t.Context(), proposeReq)
		require.NoError(t, err)
		require.True(t, res.Created)
		require.Equal(t, mecp.ProposalPending, res.Status)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Release the project", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.Empty(t, pack.Items, "a pending proposal must not appear as context")
	})

	t.Run("repeating the same key returns the existing proposal", func(t *testing.T) {
		res, err := svc.ProposeRecord(t.Context(), proposeReq)
		require.NoError(t, err)
		require.False(t, res.Created)
	})

	t.Run("approval activates the record and supersedes its predecessors", func(t *testing.T) {
		pending, err := store.QueryProposals(t.Context(), mecp.ProposalQuery{
			Statuses: []mecp.ProposalStatus{mecp.ProposalPending},
		})
		require.NoError(t, err)
		require.Len(t, pending, 1)

		rec, err := mecp.ApproveProposal(t.Context(), store, pending[0], "local-user", nil, testNow)
		require.NoError(t, err)
		require.Equal(t, mecp.StatusActive, rec.Status)

		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Release the project", Workspace: heliumWorkspace(),
		})
		require.NoError(t, err)
		require.Contains(t, itemIDs(pack.Items), rec.ID)
	})
}

func proposingCaller() mecp.Caller {
	c := agentCaller()
	c.Capabilities = append(c.Capabilities, mecp.CapPropose)
	return c
}

type steppingClock struct {
	now time.Time
}

func (c *steppingClock) Now() time.Time { return c.now.UTC() }

func itemIDs(items []mecp.ContextItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.RecordID)
	}
	return out
}

func itemByID(t *testing.T, items []mecp.ContextItem, id string) mecp.ContextItem {
	t.Helper()
	for _, item := range items {
		if item.RecordID == id {
			return item
		}
	}
	t.Fatalf("record %s is not in the context pack (got %v)", id, itemIDs(items))
	return mecp.ContextItem{}
}

func warningCodes(warnings []mecp.Warning) []mecp.WarningCode {
	out := make([]mecp.WarningCode, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, w.Code)
	}
	return out
}

// TestServiceIsSafeForConcurrentUse asserts the house rule that a configured
// entry point can be fired from many goroutines: the receiver holds validated
// configuration only, and per-call state lives in locals.
func TestServiceIsSafeForConcurrentUse(t *testing.T) {
	svc, _ := newService(t, reviewPreference(), stylesheetConstraint())

	var wg sync.WaitGroup
	errs := make(chan error, 32)

	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
				Caller: agentCaller(), Task: "Review the parser", TaskKind: mecp.TaskCodeReview,
				Workspace: heliumWorkspace(),
			})
			if err != nil {
				errs <- err
				return
			}
			if _, err := svc.Search(t.Context(), mecp.SearchRequest{
				Caller: agentCaller(), ContextID: pack.ContextID, Query: "untrusted stylesheets",
			}); err != nil {
				errs <- err
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestScopeFilter(t *testing.T) {
	global := &mecp.Record{
		ID: "rec_global", Kind: mecp.KindConstraint, Subject: "temporary files",
		Statement: "Never write temporary files to /tmp.",
		Scope:     mecp.Scope{User: "local-user"},
		Authority: mecp.AuthorityUser,
	}
	scoped := &mecp.Record{
		ID: "rec_scoped", Kind: mecp.KindConstraint, Subject: "cmd layout",
		Statement: "Command wiring lives in cmd/mecp.",
		Scope:     mecp.Scope{User: "local-user", Repository: heliumRepo},
		Authority: mecp.AuthorityUser,
	}
	svc, _ := newService(t, global, scoped)

	ask := func(f mecp.ScopeFilter) []string {
		pack, err := svc.PrepareTask(t.Context(), mecp.PrepareTaskRequest{
			Caller: agentCaller(), Task: "Do some work", Workspace: heliumWorkspace(),
			ScopeFilter: f,
		})
		require.NoError(t, err)
		return itemIDs(pack.Items)
	}

	t.Run("no filter returns both", func(t *testing.T) {
		require.ElementsMatch(t, []string{"rec_global", "rec_scoped"}, ask(mecp.ScopeFilterAll))
	})

	t.Run("global only drops the repository-scoped record", func(t *testing.T) {
		require.Equal(t, []string{"rec_global"}, ask(mecp.ScopeFilterGlobalOnly))
	})

	t.Run("scoped only drops the universal record", func(t *testing.T) {
		require.Equal(t, []string{"rec_scoped"}, ask(mecp.ScopeFilterScopedOnly))
	})

	t.Run("naming a principal does not make a record scoped", func(t *testing.T) {
		// Every extracted record carries its principal, so treating that as
		// narrowing would put all of them on the per-turn path and defeat the
		// split entirely.
		require.Contains(t, ask(mecp.ScopeFilterGlobalOnly), "rec_global")
	})
}
