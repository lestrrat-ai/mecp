package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lestrrat-ai/mecp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// workspaceArgs mirrors the workspace object in the input schemas.
type workspaceArgs struct {
	RootURI       string   `json:"root_uri,omitempty"`
	Repository    string   `json:"repository,omitempty"`
	Revision      string   `json:"revision,omitempty"`
	Branch        string   `json:"branch,omitempty"`
	RelevantPaths []string `json:"relevant_paths,omitempty"`
}

func (w *workspaceArgs) workspace() mecp.Workspace {
	if w == nil {
		return mecp.Workspace{}
	}
	return mecp.Workspace{
		RootURI:       w.RootURI,
		Repository:    w.Repository,
		Revision:      w.Revision,
		Branch:        w.Branch,
		RelevantPaths: w.RelevantPaths,
	}
}

type prepareTaskArgs struct {
	Task                     string            `json:"task"`
	TaskKind                 string            `json:"task_kind,omitempty"`
	Workspace                *workspaceArgs    `json:"workspace,omitempty"`
	Conditions               map[string]string `json:"conditions,omitempty"`
	TokenBudget              int               `json:"token_budget,omitempty"`
	IncludeEvidenceSummaries bool              `json:"include_evidence_summaries,omitempty"`
}

func (s *Server) handlePrepareTask(ctx context.Context, _ *mcp.CallToolRequest, args prepareTaskArgs) (*mcp.CallToolResult, *mecp.ContextPack, error) {
	budget := args.TokenBudget
	if budget > s.limits.MaxTokenBudget {
		budget = s.limits.MaxTokenBudget
	}

	pack, err := s.svc.PrepareTask(ctx, mecp.PrepareTaskRequest{
		Caller:                   s.caller,
		Task:                     args.Task,
		TaskKind:                 mecp.TaskKind(args.TaskKind),
		Workspace:                args.Workspace.workspace(),
		Conditions:               args.Conditions,
		TokenBudget:              budget,
		IncludeEvidenceSummaries: args.IncludeEvidenceSummaries,
	})
	if err != nil {
		return nil, nil, toolError(err)
	}
	return textResult(describePack(pack)), pack, nil
}

type searchArgs struct {
	Query        string            `json:"query"`
	ContextID    string            `json:"context_id,omitempty"`
	Workspace    *workspaceArgs    `json:"workspace,omitempty"`
	Conditions   map[string]string `json:"conditions,omitempty"`
	TaskKind     string            `json:"task_kind,omitempty"`
	Kinds        []string          `json:"kinds,omitempty"`
	IncludeStale bool              `json:"include_stale,omitempty"`
	Limit        int               `json:"limit,omitempty"`
}

func (s *Server) handleSearch(ctx context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, *mecp.SearchResult, error) {
	limit := args.Limit
	if limit > s.limits.MaxSearchLimit {
		limit = s.limits.MaxSearchLimit
	}

	kinds := make([]mecp.RecordKind, 0, len(args.Kinds))
	for _, k := range args.Kinds {
		kinds = append(kinds, mecp.RecordKind(k))
	}

	res, err := s.svc.Search(ctx, mecp.SearchRequest{
		Caller:       s.caller,
		ContextID:    args.ContextID,
		Query:        args.Query,
		Workspace:    args.Workspace.workspace(),
		Conditions:   args.Conditions,
		TaskKind:     mecp.TaskKind(args.TaskKind),
		Kinds:        kinds,
		IncludeStale: args.IncludeStale,
		Limit:        limit,
	})
	if err != nil {
		return nil, nil, toolError(err)
	}
	return textResult(describeSearch(res)), res, nil
}

type getRecordsArgs struct {
	RecordIDs                      []string `json:"record_ids"`
	IncludeEvidence                bool     `json:"include_evidence,omitempty"`
	MaxEvidenceCharactersPerRecord int      `json:"max_evidence_characters_per_record,omitempty"`
}

func (s *Server) handleGetRecords(ctx context.Context, _ *mcp.CallToolRequest, args getRecordsArgs) (*mcp.CallToolResult, *mecp.RecordResult, error) {
	limit := args.MaxEvidenceCharactersPerRecord
	if limit > s.limits.MaxEvidenceCharacters {
		limit = s.limits.MaxEvidenceCharacters
	}

	res, err := s.svc.GetRecords(ctx, mecp.GetRecordsRequest{
		Caller:                         s.caller,
		RecordIDs:                      args.RecordIDs,
		IncludeEvidence:                args.IncludeEvidence,
		MaxEvidenceCharactersPerRecord: limit,
	})
	if err != nil {
		return nil, nil, toolError(err)
	}
	return textResult(describeRecords(res)), res, nil
}

type evidenceArgs struct {
	Type         string `json:"type,omitempty"`
	Locator      string `json:"locator"`
	Revision     string `json:"revision,omitempty"`
	ExactExcerpt string `json:"exact_excerpt,omitempty"`
}

type scopeArgs struct {
	Repository     string            `json:"repository,omitempty"`
	BranchPatterns []string          `json:"branch_patterns,omitempty"`
	PathPatterns   []string          `json:"path_patterns,omitempty"`
	TaskKinds      []string          `json:"task_kinds,omitempty"`
	Conditions     map[string]string `json:"conditions,omitempty"`
}

type proposeRecordArgs struct {
	ProposalKey         string         `json:"proposal_key"`
	Kind                string         `json:"kind"`
	Subject             string         `json:"subject,omitempty"`
	Statement           string         `json:"statement"`
	Rationale           string         `json:"rationale,omitempty"`
	Scope               *scopeArgs     `json:"scope,omitempty"`
	Tags                []string       `json:"tags,omitempty"`
	Evidence            []evidenceArgs `json:"evidence,omitempty"`
	SupersedesRecordIDs []string       `json:"supersedes_record_ids,omitempty"`
}

func (s *Server) handleProposeRecord(ctx context.Context, _ *mcp.CallToolRequest, args proposeRecordArgs) (*mcp.CallToolResult, *mecp.ProposalResult, error) {
	scope := args.Scope.scope()

	evidence := make([]mecp.Source, 0, len(args.Evidence))
	for _, e := range args.Evidence {
		evidence = append(evidence, mecp.Source{
			Type:         mecp.SourceType(e.Type),
			Locator:      e.Locator,
			Revision:     e.Revision,
			ExactExcerpt: e.ExactExcerpt,
		})
	}

	res, err := s.svc.ProposeRecord(ctx, mecp.ProposeRecordRequest{
		Caller:              s.caller,
		ProposalKey:         args.ProposalKey,
		Kind:                mecp.RecordKind(args.Kind),
		Subject:             args.Subject,
		Statement:           args.Statement,
		Rationale:           args.Rationale,
		Scope:               scope,
		Tags:                args.Tags,
		Evidence:            evidence,
		SupersedesRecordIDs: args.SupersedesRecordIDs,
	})
	if err != nil {
		return nil, nil, toolError(err)
	}

	verb := "recorded as already pending"
	if res.Created {
		verb = "filed for review"
	}
	return textResult(fmt.Sprintf("Proposal %s %s. It is inactive and does not change existing context.", res.ProposalID, verb)), res, nil
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// describePack summarizes a context pack for clients that do not consume
// structured output. It reports shape and warnings rather than repeating every
// record, which would double the token cost of the call.
func describePack(pack *mecp.ContextPack) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", pack.Summary)

	if pack.Scope.Repository != "" {
		fmt.Fprintf(&b, "Scope: %s", pack.Scope.Repository)
		if pack.Scope.Revision != "" {
			fmt.Fprintf(&b, " at %s", pack.Scope.Revision)
		}
		b.WriteString("\n")
	}

	counts := map[mecp.Effect]int{}
	for _, item := range pack.Items {
		counts[item.Effect]++
	}
	fmt.Fprintf(&b, "%d records: %d constraint, %d preference, %d informational.\n",
		len(pack.Items), counts[mecp.EffectConstraint], counts[mecp.EffectPreference], counts[mecp.EffectInformational])

	if len(pack.Conflicts) > 0 {
		fmt.Fprintf(&b, "%d conflict(s) need attention.\n", len(pack.Conflicts))
	}
	for _, w := range pack.Warnings {
		fmt.Fprintf(&b, "warning %s: %s\n", w.Code, w.Message)
	}
	fmt.Fprintf(&b, "Approximately %d of %d tokens used; truncated=%t.\n",
		pack.Budget.EstimatedTokensUsed, pack.Budget.RequestedTokens, pack.Budget.Truncated)
	b.WriteString("Full details are in the structured result. context_id: " + pack.ContextID)
	return b.String()
}

func describeSearch(res *mecp.SearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d matching record(s).\n", len(res.Items))
	for _, item := range res.Items {
		fmt.Fprintf(&b, "- %s [%s, %s, %s] %s\n", item.RecordID, item.Kind, item.Effect, item.Authority, item.Subject)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "warning %s: %s\n", w.Code, w.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}

func describeRecords(res *mecp.RecordResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d record(s) returned.\n", len(res.Records))
	for _, rec := range res.Records {
		fmt.Fprintf(&b, "- %s [%s, %s] %d source(s)\n", rec.RecordID, rec.Kind, rec.Status, len(rec.Sources))
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "warning %s: %s\n", w.Code, w.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}

func asDomainError(err error, target **mecp.Error) bool { return errors.As(err, target) }

type extractedRuleArgs struct {
	Kind      string     `json:"kind"`
	Subject   string     `json:"subject,omitempty"`
	Statement string     `json:"statement"`
	Rationale string     `json:"rationale,omitempty"`
	Quote     string     `json:"quote"`
	Tags      []string   `json:"tags,omitempty"`
	Scope     *scopeArgs `json:"scope,omitempty"`
}

type extractRulesArgs struct {
	DocumentPath string              `json:"document_path"`
	Scope        *scopeArgs          `json:"scope,omitempty"`
	Rules        []extractedRuleArgs `json:"rules"`
}

func (a *scopeArgs) scope() mecp.Scope {
	if a == nil {
		return mecp.Scope{}
	}
	out := mecp.Scope{
		Repository:     a.Repository,
		BranchPatterns: a.BranchPatterns,
		PathPatterns:   a.PathPatterns,
		Conditions:     a.Conditions,
	}
	for _, k := range a.TaskKinds {
		out.TaskKinds = append(out.TaskKinds, mecp.TaskKind(k))
	}
	return out
}

func (s *Server) handleExtractRules(ctx context.Context, _ *mcp.CallToolRequest, args extractRulesArgs) (*mcp.CallToolResult, *mecp.ExtractRulesResult, error) {
	rules := make([]mecp.ExtractedRule, 0, len(args.Rules))
	for _, r := range args.Rules {
		rule := mecp.ExtractedRule{
			Kind:      mecp.RecordKind(r.Kind),
			Subject:   r.Subject,
			Statement: r.Statement,
			Rationale: r.Rationale,
			Quote:     r.Quote,
			Tags:      r.Tags,
		}
		if r.Scope != nil {
			scope := r.Scope.scope()
			rule.Scope = &scope
		}
		rules = append(rules, rule)
	}

	res, err := s.svc.ExtractRules(ctx, mecp.ExtractRulesRequest{
		Caller:       s.caller,
		DocumentPath: args.DocumentPath,
		Scope:        args.Scope.scope(),
		Rules:        rules,
	})
	if err != nil {
		return nil, nil, toolError(err)
	}
	return textResult(describeExtraction(res)), res, nil
}

func describeExtraction(res *mecp.ExtractRulesResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d rule(s) queued for review from %s (%d new, %d already pending).\n",
		len(res.Accepted), res.DocumentPath, res.CreatedCount, res.ExistingCount)
	if len(res.Rejected) > 0 {
		fmt.Fprintf(&b, "%d rule(s) refused:\n", len(res.Rejected))
		for _, r := range res.Rejected {
			fmt.Fprintf(&b, "- %s: %s\n", r.Reason, r.Statement)
		}
	}
	b.WriteString("Nothing is active until the user approves it with \"mecp review\".")
	return b.String()
}
