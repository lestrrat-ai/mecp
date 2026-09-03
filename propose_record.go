package mecp

import (
	"context"
	"slices"
	"strings"
	"time"
)

// maxProposalTextLength bounds each free-text proposal field. A proposal is a
// suggestion, not a document dump.
const maxProposalTextLength = 8000

// ProposeRecord files an inactive proposal for user review. It never activates
// a record and never modifies existing context, which is what stops an agent
// inference from becoming authoritative through repetition.
func (s *service) ProposeRecord(ctx context.Context, req ProposeRecordRequest) (*ProposalResult, error) {
	start := time.Now()

	if err := req.Caller.Validate(); err != nil {
		return nil, err
	}
	if !req.Caller.Has(CapPropose) {
		return nil, errorf(CodeProposalDisabled,
			"client profile %q may not propose records; use the administrative interface", req.Caller.ClientID)
	}
	if strings.TrimSpace(req.ProposalKey) == "" {
		return nil, errorf(CodeInvalidRecord, "a proposal key is required so that retries do not create duplicates")
	}
	if !req.Kind.Valid() {
		return nil, errorf(CodeInvalidRecord, "unknown record kind %q", req.Kind)
	}
	if strings.TrimSpace(req.Statement) == "" {
		return nil, errorf(CodeInvalidRecord, "a statement is required")
	}
	for _, field := range []string{req.Statement, req.Rationale, req.Subject} {
		if len(field) > maxProposalTextLength {
			return nil, errorf(CodeInvalidRecord, "proposal text exceeds %d characters", maxProposalTextLength)
		}
	}

	scope := req.Scope.Clone()
	scope.Normalize()
	if scope.User == "" {
		scope.User = req.Caller.PrincipalID
	}
	if scope.Repository != "" && !req.Caller.RepositoryAllowed(scope.Repository) {
		return nil, errorf(CodeUnauthorizedScope,
			"client profile %q may not propose records for repository %s", req.Caller.ClientID, scope.Repository)
	}
	if err := scope.Validate(); err != nil {
		return nil, wrapf(CodeInvalidRecord, err, "invalid proposal scope")
	}

	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		subject = deriveSubject(req.Statement)
	}

	now := s.clock.Now()
	evidence := slices.Clone(req.Evidence)
	for i := range evidence {
		if evidence[i].ID == "" {
			evidence[i].ID = NewID("src")
		}
		if evidence[i].Type == "" {
			evidence[i].Type = SourceConversation
		}
		if evidence[i].CapturedAt.IsZero() {
			evidence[i].CapturedAt = now
		}
	}

	proposal := &Proposal{
		ID:                  NewID("prop"),
		Key:                 req.ProposalKey,
		Status:              ProposalPending,
		PrincipalID:         req.Caller.PrincipalID,
		ClientID:            req.Caller.ClientID,
		Kind:                req.Kind,
		Subject:             subject,
		Statement:           strings.TrimSpace(req.Statement),
		Rationale:           strings.TrimSpace(req.Rationale),
		Scope:               scope,
		Tags:                slices.Clone(req.Tags),
		Evidence:            evidence,
		SupersedesRecordIDs: slices.Clone(req.SupersedesRecordIDs),
		CreatedAt:           now,
	}

	stored, created, err := s.store.PutProposal(ctx, proposal)
	if err != nil {
		return nil, wrapf(CodeStorage, err, "cannot store proposal")
	}

	s.writeAudit(ctx, req.Caller, AuditEvent{
		Operation:   "propose_record",
		Scope:       EffectiveScope{Principal: req.Caller.PrincipalID, Repository: scope.Repository},
		ProposalID:  stored.ID,
		ResultCount: 1,
	}, start)

	return &ProposalResult{ProposalID: stored.ID, Status: stored.Status, Created: created}, nil
}

// deriveSubject takes the first clause of a statement as its subject when the
// proposer did not supply one.
func deriveSubject(statement string) string {
	statement = strings.TrimSpace(statement)
	if idx := strings.IndexAny(statement, ".;\n"); idx > 0 {
		statement = statement[:idx]
	}
	words := strings.Fields(statement)
	if len(words) > 12 {
		words = words[:12]
	}
	return strings.Join(words, " ")
}

// ApproveProposal turns a reviewed proposal into an active record. It is not
// reachable from the agent-facing tools: only the administrative interface
// calls it, which is what keeps the write path under user control.
func ApproveProposal(ctx context.Context, store Store, p *Proposal, approver string, edits *Record, now time.Time) (*Record, error) {
	if p.Status != ProposalPending {
		return nil, errorf(CodeInvalidRecord, "proposal %s is already %s", p.ID, p.Status)
	}

	rec := &Record{
		ID:               NewID("rec"),
		Kind:             p.Kind,
		Subject:          p.Subject,
		Statement:        p.Statement,
		Rationale:        p.Rationale,
		Scope:            p.Scope.Clone(),
		Authority:        AuthorityUser,
		Status:           StatusActive,
		ValidationPolicy: ValidateNone,
		Tags:             slices.Clone(p.Tags),
		Sources:          slices.Clone(p.Evidence),
		Supersedes:       slices.Clone(p.SupersedesRecordIDs),
	}
	if edits != nil {
		applyEdits(rec, edits)
	}
	rec.Normalize(now)
	if err := rec.Validate(); err != nil {
		return nil, wrapf(CodeInvalidRecord, err, "approved record is not valid")
	}

	if err := store.PutRecord(ctx, rec); err != nil {
		return nil, wrapf(CodeStorage, err, "cannot store approved record")
	}
	for _, id := range rec.Supersedes {
		old, err := store.GetRecord(ctx, id)
		if err != nil {
			return nil, wrapf(CodeStorage, err, "cannot load superseded record %s", id)
		}
		old.SupersededBy = rec.ID
		old.Status = StatusSuperseded
		old.UpdatedAt = now
		if err := store.PutRecord(ctx, old); err != nil {
			return nil, wrapf(CodeStorage, err, "cannot mark record %s superseded", id)
		}
	}

	p.Status = ProposalApproved
	p.DecidedAt = &now
	p.DecidedBy = approver
	p.ResultRecordID = rec.ID
	if err := store.UpdateProposal(ctx, p); err != nil {
		return nil, wrapf(CodeStorage, err, "cannot record proposal approval")
	}
	return rec, nil
}

// RejectProposal records a review decision without creating a record. The
// rejection is retained so the same suggestion is not proposed again forever.
func RejectProposal(ctx context.Context, store Store, p *Proposal, reviewer, note string, now time.Time) error {
	if p.Status != ProposalPending {
		return errorf(CodeInvalidRecord, "proposal %s is already %s", p.ID, p.Status)
	}
	p.Status = ProposalRejected
	p.DecidedAt = &now
	p.DecidedBy = reviewer
	p.DecisionNote = note
	if err := store.UpdateProposal(ctx, p); err != nil {
		return wrapf(CodeStorage, err, "cannot record proposal rejection")
	}
	return nil
}

// applyEdits overlays the reviewer's changes onto the record built from a
// proposal. Only fields the reviewer actually set are taken.
func applyEdits(rec, edits *Record) {
	if edits.Kind != "" {
		rec.Kind = edits.Kind
	}
	if edits.Subject != "" {
		rec.Subject = edits.Subject
	}
	if edits.Statement != "" {
		rec.Statement = edits.Statement
	}
	if edits.Rationale != "" {
		rec.Rationale = edits.Rationale
	}
	if edits.Authority != "" {
		rec.Authority = edits.Authority
	}
	if edits.ValidationPolicy != "" {
		rec.ValidationPolicy = edits.ValidationPolicy
	}
	if edits.ReviewAfter != nil {
		rec.ReviewAfter = edits.ReviewAfter
	}
	if len(edits.Tags) > 0 {
		rec.Tags = slices.Clone(edits.Tags)
	}
	if !edits.Scope.Global() {
		rec.Scope = edits.Scope.Clone()
	}
}
