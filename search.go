package mecp

import (
	"context"
	"strings"
	"time"
)

// Search answers a targeted follow-up question inside an authorized scope. It
// is not an unrestricted personal-data search: without either a live context
// handle or an explicit workspace, the request is rejected.
func (s *service) Search(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	start := time.Now()

	if !req.Caller.Has(CapSearch) {
		return nil, errorf(CodeUnauthorizedScope, "client profile %q may not search context", req.Caller.ClientID)
	}
	if strings.TrimSpace(req.Query) == "" {
		return nil, errorf(CodeInvalidScope, "a query is required")
	}
	for _, k := range req.Kinds {
		if !k.Valid() {
			return nil, errorf(CodeInvalidScope, "unknown record kind %q", k)
		}
	}

	workspace := req.Workspace
	taskKind := req.TaskKind
	conditions := req.Conditions

	if req.ContextID != "" {
		h, err := s.takeHandle(req.Caller, req.ContextID)
		if err != nil {
			return nil, err
		}
		workspace = h.Workspace
		if taskKind == "" {
			taskKind = h.TaskKind
		}
		if len(conditions) == 0 {
			conditions = h.Conditions
		}
	} else if workspace.Repository == "" && workspace.RootURI == "" {
		return nil, errorf(CodeInvalidScope,
			"supply either a context_id from a previous prepare_task call or a workspace")
	}

	if taskKind != "" && !taskKind.Valid() {
		return nil, errorf(CodeInvalidScope, "unknown task kind %q", req.TaskKind)
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	scope, warnings, err := s.resolveScope(req.Caller, workspace, taskKind)
	if err != nil {
		s.writeAudit(ctx, req.Caller, AuditEvent{
			Operation: "search",
			ErrorCode: CodeOf(err),
		}, start)
		return nil, err
	}

	cands, collectWarnings, err := s.collect(ctx, collectRequest{
		Caller:       req.Caller,
		Text:         req.Query,
		Workspace:    workspace,
		Repository:   scope.Repository,
		TaskKind:     taskKind,
		Conditions:   conditions,
		Kinds:        req.Kinds,
		IncludeStale: req.IncludeStale,
	})
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, collectWarnings...)

	if len(cands) > limit {
		cands = cands[:limit]
	}

	items := make([]SearchItem, 0, len(cands))
	for _, c := range cands {
		rec := c.Record
		item := SearchItem{
			RecordID:         rec.ID,
			Kind:             rec.Kind,
			Effect:           c.Effect,
			Subject:          rec.Subject,
			Statement:        rec.Statement,
			Authority:        rec.Authority,
			Status:           rec.Status,
			ScopeSpecificity: c.Scope.Label,
			Validation:       c.Validation.State,
			LastVerifiedAt:   rec.LastVerifiedAt,
			SourceRefs:       make([]string, 0, len(rec.Sources)),
			MatchReasons:     c.MatchReasons,
		}
		for _, src := range rec.Sources {
			item.SourceRefs = append(item.SourceRefs, src.ID)
		}
		items = append(items, item)
	}

	result := &SearchResult{
		ContextID: req.ContextID,
		Scope:     scope,
		Items:     items,
		Warnings:  warnings,
	}

	s.writeAudit(ctx, req.Caller, AuditEvent{
		Operation:    "search",
		Scope:        scope,
		RecordIDs:    searchRecordIDs(items),
		WarningCodes: warningCodes(warnings),
		ResultCount:  len(items),
	}, start)

	return result, nil
}

func searchRecordIDs(items []SearchItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.RecordID)
	}
	return out
}
