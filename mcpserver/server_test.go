package mcpserver_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/mcpserver"
	"github.com/lestrrat-ai/mecp/sqlite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

const heliumRepo = "https://github.com/lestrrat-go/helium"

func agentCaller() mecp.Caller {
	return mecp.Caller{
		PrincipalID:  "local-user",
		ClientID:     "claude-code",
		Capabilities: []mecp.Capability{mecp.CapPrepare, mecp.CapSearch, mecp.CapEvidence},
	}
}

// connect wires a server and client over in-memory transports, which exercises
// the real JSON-RPC encoding, schema validation, and structured-output path.
func connect(t *testing.T, caller mecp.Caller, records ...*mecp.Record) *mcp.ClientSession {
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

	srv, err := mcpserver.New(svc, caller)
	require.NoError(t, err)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(t.Context(), serverTransport, nil); err != nil {
		t.Fatalf("failed to connect server: %s", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })
	return session
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

func TestToolListing(t *testing.T) {
	t.Run("read-only client gets the three read tools in a stable order", func(t *testing.T) {
		session := connect(t, agentCaller())

		res, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)

		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		require.Equal(t, []string{
			mcpserver.ToolGetRecords,
			mcpserver.ToolPrepareTask,
			mcpserver.ToolSearch,
		}, names)
	})

	t.Run("read tools advertise the read-only annotations", func(t *testing.T) {
		session := connect(t, agentCaller())

		res, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		for _, tool := range res.Tools {
			require.NotNil(t, tool.Annotations, tool.Name)
			require.True(t, tool.Annotations.ReadOnlyHint, tool.Name)
			require.NotNil(t, tool.Annotations.OpenWorldHint, tool.Name)
			require.False(t, *tool.Annotations.OpenWorldHint, tool.Name)
		}
	})

	t.Run("the proposal tool is absent without the capability", func(t *testing.T) {
		session := connect(t, agentCaller())

		res, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		for _, tool := range res.Tools {
			require.NotEqual(t, mcpserver.ToolProposeRecord, tool.Name)
		}
	})

	t.Run("the proposal tool appears once the capability is granted", func(t *testing.T) {
		caller := agentCaller()
		caller.Capabilities = append(caller.Capabilities, mecp.CapPropose)
		session := connect(t, caller)

		res, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)

		var found bool
		for _, tool := range res.Tools {
			if tool.Name == mcpserver.ToolProposeRecord {
				found = true
				require.False(t, tool.Annotations.ReadOnlyHint)
			}
		}
		require.True(t, found)
	})

	t.Run("tool names are usable as function-calling names", func(t *testing.T) {
		session := connect(t, agentCaller())

		res, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		for _, tool := range res.Tools {
			for _, r := range tool.Name {
				valid := r == '_' || r == '-' ||
					(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
				require.True(t, valid, "tool name %q contains %q, which several hosts reject", tool.Name, r)
			}
		}
	})
}

func TestPrepareTaskTool(t *testing.T) {
	session := connect(t, agentCaller(), stylesheetConstraint())

	t.Run("returns a structured context pack and a text summary", func(t *testing.T) {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: mcpserver.ToolPrepareTask,
			Arguments: map[string]any{
				"task":      "Review the XSLT handling for production readiness",
				"task_kind": "code_review",
				"workspace": map[string]any{
					"root_uri":       "file:///work/helium",
					"repository":     "git@github.com:lestrrat-go/helium.git",
					"revision":       "8f3b2c1",
					"relevant_paths": []string{"xmldsig1/"},
				},
			},
		})
		require.NoError(t, err)
		require.False(t, res.IsError, contentText(res))

		var pack mecp.ContextPack
		decodeStructured(t, res, &pack)
		require.Len(t, pack.Items, 1)
		require.Equal(t, "rec_stylesheet_constraint", pack.Items[0].RecordID)
		require.Equal(t, mecp.EffectConstraint, pack.Items[0].Effect)
		require.NotEmpty(t, pack.ContextID)

		require.Contains(t, contentText(res), "1 records")
	})

	t.Run("applies the default task kind and budget", func(t *testing.T) {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      mcpserver.ToolPrepareTask,
			Arguments: map[string]any{"task": "Look at the parser"},
		})
		require.NoError(t, err)
		require.False(t, res.IsError, contentText(res))

		var pack mecp.ContextPack
		decodeStructured(t, res, &pack)
		require.Equal(t, mecp.DefaultTokenBudget, pack.Budget.RequestedTokens)
	})

	t.Run("rejects an unknown argument", func(t *testing.T) {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      mcpserver.ToolPrepareTask,
			Arguments: map[string]any{"task": "Review", "principal_id": "someone-else"},
		})
		require.NoError(t, err)
		require.True(t, res.IsError, "the schema must reject arguments it does not declare")
	})

	t.Run("rejects a missing task", func(t *testing.T) {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      mcpserver.ToolPrepareTask,
			Arguments: map[string]any{"task_kind": "code_review"},
		})
		require.NoError(t, err)
		require.True(t, res.IsError)
	})

	t.Run("rejects a task kind outside the enum", func(t *testing.T) {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      mcpserver.ToolPrepareTask,
			Arguments: map[string]any{"task": "Review", "task_kind": "sabotage"},
		})
		require.NoError(t, err)
		require.True(t, res.IsError)
	})
}

func TestSearchTool(t *testing.T) {
	session := connect(t, agentCaller(), stylesheetConstraint())

	t.Run("reports a missing scope with a stable code and guidance", func(t *testing.T) {
		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      mcpserver.ToolSearch,
			Arguments: map[string]any{"query": "untrusted stylesheets"},
		})
		require.NoError(t, err)
		require.True(t, res.IsError)
		require.Contains(t, contentText(res), string(mecp.CodeInvalidScope))
		require.Contains(t, contentText(res), "context_id")
	})

	t.Run("answers within a prepared context", func(t *testing.T) {
		prep, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: mcpserver.ToolPrepareTask,
			Arguments: map[string]any{
				"task":      "Review parsing",
				"workspace": map[string]any{"repository": heliumRepo},
			},
		})
		require.NoError(t, err)
		var pack mecp.ContextPack
		decodeStructured(t, prep, &pack)

		res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: mcpserver.ToolSearch,
			Arguments: map[string]any{
				"query":      "What did the user say about untrusted stylesheets?",
				"context_id": pack.ContextID,
			},
		})
		require.NoError(t, err)
		require.False(t, res.IsError, contentText(res))

		var out mecp.SearchResult
		decodeStructured(t, res, &out)
		require.NotEmpty(t, out.Items)
		require.Equal(t, "rec_stylesheet_constraint", out.Items[0].RecordID)
	})
}

func TestGetRecordsTool(t *testing.T) {
	rec := stylesheetConstraint()
	rec.Sources = []mecp.Source{{
		ID: "src_adr", Type: mecp.SourceADR, Locator: "file://docs/adr/0003-xslt.md",
		ExactExcerpt: "Stylesheet execution is disabled because it is a remote code execution primitive.",
	}}
	session := connect(t, agentCaller(), rec)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: mcpserver.ToolGetRecords,
		Arguments: map[string]any{
			"record_ids":       []string{"rec_stylesheet_constraint"},
			"include_evidence": true,
		},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, contentText(res))

	var out mecp.RecordResult
	decodeStructured(t, res, &out)
	require.Len(t, out.Records, 1)
	require.Contains(t, out.Records[0].Sources[0].Excerpt, "remote code execution")
}

func TestProposeRecordTool(t *testing.T) {
	caller := agentCaller()
	caller.Capabilities = append(caller.Capabilities, mecp.CapPropose)
	session := connect(t, caller)

	args := map[string]any{
		"proposal_key": "session-123:decision:controlled-commit",
		"kind":         "decision",
		"statement":    "The release process runs the conformance suite against a controlled commit.",
		"scope":        map[string]any{"repository": heliumRepo},
		"evidence": []map[string]any{{
			"type": "conversation", "locator": "turn://42",
			"exact_excerpt": "We pin the suite to a definite commit before each release.",
		}},
	}

	first, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: mcpserver.ToolProposeRecord, Arguments: args})
	require.NoError(t, err)
	require.False(t, first.IsError, contentText(first))

	var created mecp.ProposalResult
	decodeStructured(t, first, &created)
	require.True(t, created.Created)
	require.Equal(t, mecp.ProposalPending, created.Status)

	second, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: mcpserver.ToolProposeRecord, Arguments: args})
	require.NoError(t, err)

	var repeated mecp.ProposalResult
	decodeStructured(t, second, &repeated)
	require.False(t, repeated.Created)
	require.Equal(t, created.ProposalID, repeated.ProposalID)
}

func TestProposeToolIsUnreachableWithoutCapability(t *testing.T) {
	session := connect(t, agentCaller())

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      mcpserver.ToolProposeRecord,
		Arguments: map[string]any{"proposal_key": "k", "kind": "decision", "statement": "x"},
	})
	// An unregistered tool is a protocol error, not a tool result.
	if err == nil {
		require.True(t, res.IsError)
	}
}

// decodeStructured re-encodes the client-side structured result so that it can
// be read back into the domain type the server produced.
func decodeStructured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	require.NotNil(t, res.StructuredContent, "tool returned no structured content: %s", contentText(res))
	buf, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(buf, out))
}

func contentText(res *mcp.CallToolResult) string {
	var out string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out += tc.Text
		}
	}
	return out
}
