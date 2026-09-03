package mecp

import (
	"context"
	"slices"
	"time"
)

// GetRecords returns full detail and bounded evidence for record IDs the
// caller has already discovered. It performs no search: an ID the caller was
// never shown is reported as not found rather than fetched.
func (s *service) GetRecords(ctx context.Context, req GetRecordsRequest) (*RecordResult, error) {
	start := time.Now()

	if !req.Caller.Has(CapSearch) && !req.Caller.Has(CapPrepare) {
		return nil, errorf(CodeUnauthorizedScope, "client profile %q may not read records", req.Caller.ClientID)
	}
	if err := req.Caller.Validate(); err != nil {
		return nil, err
	}
	if len(req.RecordIDs) == 0 {
		return nil, errorf(CodeInvalidScope, "at least one record ID is required")
	}
	if len(req.RecordIDs) > maxRecordIDsPerCall {
		return nil, errorf(CodeInvalidScope, "at most %d record IDs may be requested at once", maxRecordIDsPerCall)
	}

	limit := req.MaxEvidenceCharactersPerRecord
	if limit <= 0 {
		limit = defaultEvidenceCharacters
	}
	if limit > maxEvidenceCharacters {
		limit = maxEvidenceCharacters
	}

	wanted := slices.Clone(req.RecordIDs)
	slices.Sort(wanted)
	wanted = slices.Compact(wanted)

	query := RecordQuery{
		PrincipalID: req.Caller.PrincipalID,
		IDs:         wanted,
		Limit:       len(wanted),
		AllowGlobal: true,
	}
	if len(req.Caller.AllowedRepositories) > 0 {
		query.RestrictRepositories = true
		for _, repo := range req.Caller.AllowedRepositories {
			query.Repositories = append(query.Repositories, s.canonicalRepository(repo))
		}
	}

	recs, err := s.store.QueryRecords(ctx, query)
	if err != nil {
		return nil, wrapf(CodeStorage, err, "cannot load records")
	}

	ids := make([]string, 0, len(recs))
	for _, rec := range recs {
		ids = append(ids, rec.ID)
	}
	superseded, err := s.store.SupersededBy(ctx, ids)
	if err != nil {
		return nil, wrapf(CodeStorage, err, "cannot resolve supersession")
	}

	now := s.clock.Now()
	// Verbatim source text is held to a stricter bar than a record's own
	// statement: it is the raw material a record was normalized from, and it is
	// the field a prompt injection would arrive in.
	mayReadEvidence := req.Caller.Has(CapEvidence)

	var (
		details    []RecordDetail
		warnings   []Warning
		redactedIn []string
	)

	for _, rec := range recs {
		// Defence in depth: the store already filtered, but a record whose
		// repository the profile may not see must never leave this function.
		if rec.Scope.Repository != "" && !req.Caller.RepositoryAllowed(rec.Scope.Repository) {
			continue
		}

		status := s.validator.Validate(ctx, rec, Workspace{}, now)
		effect := rec.EffectFor(now)
		if !status.State.Trusted() || rec.SupersededBy != "" || len(superseded[rec.ID]) > 0 {
			effect = EffectInformational
		}

		detail := RecordDetail{
			RecordID:         rec.ID,
			Kind:             rec.Kind,
			Effect:           effect,
			Subject:          rec.Subject,
			Statement:        rec.Statement,
			Rationale:        rec.Rationale,
			Authority:        rec.Authority,
			Status:           rec.Status,
			Scope:            rec.Scope,
			Tags:             rec.Tags,
			Confidence:       rec.Confidence,
			ValidFrom:        rec.ValidFrom,
			ValidUntil:       rec.ValidUntil,
			ReviewAfter:      rec.ReviewAfter,
			LastVerifiedAt:   rec.LastVerifiedAt,
			ValidationPolicy: rec.ValidationPolicy,
			Validation:       status,
			Supersedes:       rec.Supersedes,
			SupersededBy:     supersededByList(rec, superseded[rec.ID]),
		}

		for _, src := range rec.Sources {
			view, redacted := sourceView(src, mayReadEvidence, req.IncludeEvidence, limit)
			if redacted {
				redactedIn = append(redactedIn, rec.ID)
			}
			detail.Sources = append(detail.Sources, view)
		}

		details = append(details, detail)
	}

	if missing := missingIDs(wanted, details); len(missing) > 0 {
		warnings = append(warnings, Warning{
			Code:      WarnRecordNotFound,
			Message:   "some requested records do not exist or are outside this client's authorized scope",
			RecordIDs: missing,
		})
	}
	if len(redactedIn) > 0 {
		slices.Sort(redactedIn)
		warnings = append(warnings, Warning{
			Code:      WarnEvidenceRedacted,
			Message:   "verbatim evidence was withheld because this client lacks the evidence capability",
			RecordIDs: slices.Compact(redactedIn),
		})
	}

	s.writeAudit(ctx, AuditEvent{
		PrincipalID:  req.Caller.PrincipalID,
		ClientID:     req.Caller.ClientID,
		Operation:    "get_records",
		Scope:        EffectiveScope{Principal: req.Caller.PrincipalID},
		RecordIDs:    ids,
		WarningCodes: warningCodes(warnings),
		ResultCount:  len(details),
	}, start)

	return &RecordResult{Records: details, Warnings: warnings}, nil
}

// sourceView applies the evidence policy to one source. Metadata is always
// disclosed so that the agent can tell the user where to look; the verbatim
// excerpt is what the evidence capability gates.
func sourceView(src Source, mayRead, include bool, limit int) (SourceView, bool) {
	view := SourceView{
		SourceID:    src.ID,
		Type:        src.Type,
		Locator:     src.Locator,
		Revision:    src.Revision,
		ContentHash: src.ContentHash,
		CapturedAt:  src.CapturedAt,
	}
	if !include || src.ExactExcerpt == "" {
		return view, false
	}

	if !mayRead {
		view.Redacted = true
		return view, true
	}

	runes := []rune(src.ExactExcerpt)
	if len(runes) > limit {
		view.Excerpt = string(runes[:limit])
		view.Truncated = true
	} else {
		view.Excerpt = src.ExactExcerpt
	}
	return view, false
}

func supersededByList(rec *Record, extra []string) []string {
	out := slices.Clone(extra)
	if rec.SupersededBy != "" {
		out = append(out, rec.SupersededBy)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func missingIDs(wanted []string, details []RecordDetail) []string {
	found := make(map[string]struct{}, len(details))
	for _, d := range details {
		found[d.RecordID] = struct{}{}
	}
	var missing []string
	for _, id := range wanted {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}
