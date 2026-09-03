package mecp

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// PrepareTask builds a bounded context pack for a concrete coding task. It is
// the normal first call before planning or executing nontrivial work.
func (s *service) PrepareTask(ctx context.Context, req PrepareTaskRequest) (*ContextPack, error) {
	start := time.Now()

	if !req.Caller.Has(CapPrepare) {
		return nil, errorf(CodeUnauthorizedScope, "client profile %q may not prepare task context", req.Caller.ClientID)
	}
	if strings.TrimSpace(req.Task) == "" {
		return nil, errorf(CodeInvalidScope, "task description is required")
	}

	// An empty task kind stays empty: it means "the host did not say", which
	// scope matching treats differently from an explicit "other".
	taskKind := req.TaskKind
	if taskKind != "" && !taskKind.Valid() {
		return nil, errorf(CodeInvalidScope, "unknown task kind %q", req.TaskKind)
	}

	budget := req.TokenBudget
	if budget == 0 {
		budget = DefaultTokenBudget
	}
	if budget < MinimumTokenBudget {
		return nil, errorf(CodeBudgetTooSmall,
			"a token budget of %d cannot carry the mandatory metadata; request at least %d", budget, MinimumTokenBudget)
	}

	scope, warnings, err := s.resolveScope(req.Caller, req.Workspace, taskKind)
	if err != nil {
		s.writeAudit(ctx, AuditEvent{
			PrincipalID: req.Caller.PrincipalID,
			ClientID:    req.Caller.ClientID,
			Operation:   "prepare_task",
			ErrorCode:   CodeOf(err),
		}, start)
		return nil, err
	}

	cands, collectWarnings, err := s.collect(ctx, collectRequest{
		Caller:           req.Caller,
		Text:             req.Task,
		Workspace:        req.Workspace,
		Repository:       scope.Repository,
		TaskKind:         taskKind,
		Conditions:       req.Conditions,
		IncludeMandatory: true,
	})
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, collectWarnings...)

	conflicts := DetectConflicts(cands)
	if len(conflicts) > 0 {
		warnings = append(warnings, Warning{
			Code:      WarnConflict,
			Message:   "active records disagree; see conflicts for the recommended handling",
			RecordIDs: conflictRecordIDs(conflicts),
		})
	}

	items, budgetReport := s.packer.Pack(cands, budget, req.IncludeEvidenceSummaries)
	if budgetReport.Truncated {
		warnings = append(warnings, Warning{
			Code:    WarnTruncated,
			Message: fmt.Sprintf("%d further records did not fit the requested budget", budgetReport.OmittedItemCount),
		})
	}

	now := s.clock.Now()
	pack := &ContextPack{
		ContextID:   NewID("ctx"),
		GeneratedAt: now,
		ExpiresAt:   now.Add(s.contextTTL),
		Scope:       scope,
		Summary:     summarize(items, conflicts),
		Items:       items,
		Conflicts:   conflicts,
		Warnings:    warnings,
		Budget:      budgetReport,
	}

	s.putHandle(&contextHandle{
		ID:          pack.ContextID,
		PrincipalID: req.Caller.PrincipalID,
		ClientID:    req.Caller.ClientID,
		Task:        req.Task,
		TaskKind:    taskKind,
		Workspace:   req.Workspace,
		Conditions:  req.Conditions,
		Scope:       scope,
		ExpiresAt:   pack.ExpiresAt,
	})

	s.writeAudit(ctx, AuditEvent{
		PrincipalID:  req.Caller.PrincipalID,
		ClientID:     req.Caller.ClientID,
		Operation:    "prepare_task",
		Scope:        scope,
		RecordIDs:    itemRecordIDs(items),
		WarningCodes: warningCodes(warnings),
		Truncated:    budgetReport.Truncated,
		ResultCount:  len(items),
	}, start)

	return pack, nil
}

// summarize states in one sentence what the pack is telling the agent, so a
// client that only renders text still conveys the shape of the result.
func summarize(items []ContextItem, conflicts []Conflict) string {
	if len(items) == 0 {
		return "No stored context applies to this task and workspace."
	}

	counts := map[Effect]int{}
	for _, item := range items {
		counts[item.Effect]++
	}

	var parts []string
	if n := counts[EffectConstraint]; n > 0 {
		parts = append(parts, pluralize(n, "applicable constraint"))
	}
	if n := counts[EffectPreference]; n > 0 {
		parts = append(parts, pluralize(n, "preference"))
	}
	if n := counts[EffectInformational]; n > 0 {
		parts = append(parts, pluralize(n, "informational record"))
	}

	summary := strings.Join(parts, ", ") + "."
	if len(conflicts) > 0 {
		summary += " " + pluralize(len(conflicts), "unresolved conflict") + " require attention."
	}
	summary += " Current user instructions and current repository files take priority over everything here."
	return strings.ToUpper(summary[:1]) + summary[1:]
}

func conflictRecordIDs(conflicts []Conflict) []string {
	var out []string
	for _, c := range conflicts {
		out = append(out, c.RecordIDs...)
	}
	return dedupeStrings(out)
}

func itemRecordIDs(items []ContextItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.RecordID)
	}
	return out
}

func warningCodes(warnings []Warning) []WarningCode {
	if len(warnings) == 0 {
		return nil
	}
	seen := make(map[WarningCode]struct{}, len(warnings))
	out := make([]WarningCode, 0, len(warnings))
	for _, w := range warnings {
		if _, ok := seen[w.Code]; ok {
			continue
		}
		seen[w.Code] = struct{}{}
		out = append(out, w.Code)
	}
	return out
}
