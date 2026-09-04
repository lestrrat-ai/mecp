package mecp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// maxExtractedRules bounds one extraction call. A document holding more rules
// than this should be split, and an unbounded batch is a denial-of-service
// surface.
const maxExtractedRules = 200

// Document is an instruction file rules were extracted from.
type Document struct {
	// Path is the resolved absolute path.
	Path string
	// Content is the whole file.
	Content string
	// ContentHash is the "sha256:<hex>" digest of Content.
	ContentHash string
}

// DocumentReader reads the instruction documents that rules are extracted from.
// It only reads: nothing here persists a document, and the file stays the thing
// a record is later revalidated against.
//
// It is an interface so the domain package stays free of filesystem access, and
// so the implementation can enforce which documents are readable at all. That
// restriction matters: this is the one place where a caller names a path, and
// reporting whether a quote appears in a file is enough to read that file back
// a piece at a time.
type DocumentReader interface {
	Read(ctx context.Context, path string) (*Document, error)
}

// HashContent returns the digest form used for document and source hashes.
func HashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ExtractedRule is one rule a caller read out of a document.
//
// Quote is what makes this trustworthy. The caller supplies the exact text the
// rule came from, and the service checks it against the document itself, so a
// model cannot file a rule the document does not contain.
type ExtractedRule struct {
	Kind      RecordKind
	Subject   string
	Statement string
	Rationale string
	Quote     string
	Tags      []string
	// Scope overrides the request-wide scope for this one rule.
	Scope *Scope
}

// ExtractRulesRequest asks to turn a document's rules into proposals.
type ExtractRulesRequest struct {
	Caller Caller
	// DocumentPath names the file the rules were read from.
	DocumentPath string
	// Scope applies to every rule that does not carry its own.
	Scope Scope
	Rules []ExtractedRule
	// Authority is what records from this document claim. The caller sets it
	// from configuration rather than the model choosing it, because whether a
	// document is the user's own writing is not something a model can know.
	Authority Authority
}

// AcceptedRule reports one rule that was stored. It is either an active record
// or, when something about it needs a person, a pending proposal.
type AcceptedRule struct {
	// RecordID is set when the rule was activated directly.
	RecordID string `json:"record_id,omitempty"`
	// ProposalID is set when the rule was held for review instead.
	ProposalID string `json:"proposal_id,omitempty"`
	Subject    string `json:"subject"`
	Statement  string `json:"statement"`
	Line       int    `json:"line"`
	Created    bool   `json:"created"`
	// NeedsReview says why this one was held back, and is empty when it went
	// straight in.
	NeedsReview []ReviewFlag `json:"needs_review,omitempty"`
}

// RejectedRule reports one rule that did not, and why.
type RejectedRule struct {
	Statement string `json:"statement"`
	Quote     string `json:"quote"`
	Reason    string `json:"reason"`
}

// BlockedRule reports a rule that matches a proposal already decided. It is
// neither accepted nor refused: the rule is fine, but the same quote from the
// same document has been ruled on before, so filing it again would either
// duplicate a record or reopen a decision behind the user's back.
type BlockedRule struct {
	Statement  string         `json:"statement"`
	ProposalID string         `json:"proposal_id"`
	Status     ProposalStatus `json:"status"`
	Reason     string         `json:"reason"`
}

// UncoveredLine is a line the document's own structure marks as a rule that no
// submitted quote accounts for.
type UncoveredLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// ExtractRulesResult is the outcome of one extraction.
type ExtractRulesResult struct {
	DocumentPath string         `json:"document_path"`
	ContentHash  string         `json:"content_hash"`
	Accepted     []AcceptedRule `json:"accepted"`
	Rejected     []RejectedRule `json:"rejected,omitempty"`
	Blocked      []BlockedRule  `json:"blocked,omitempty"`
	// ActivatedCount is how many rules became records without needing anyone.
	ActivatedCount int `json:"activated_count"`
	// ReviewCount is how many were held back for a person to look at.
	ReviewCount int `json:"review_count"`
	// PendingCount is how many were already waiting from an earlier run.
	PendingCount int `json:"pending_count"`
	// Retired names records from this document whose quoted line no longer
	// appears in it, which happens when the document is edited between
	// extractions.
	Retired []string `json:"retired,omitempty"`
	// Uncovered lists lines the document presents as rules that no submitted
	// quote covers. It is advice, not an accusation: skipping prose is often
	// right, and skipping a table of specifics usually is not.
	Uncovered []UncoveredLine `json:"uncovered,omitempty"`
	Warnings  []Warning       `json:"warnings,omitempty"`
}

// ExtractRules turns rules a caller read out of an instruction document into
// pending proposals.
//
// Judgement about what is a rule belongs to the caller, which is why this
// accepts an extraction rather than parsing the document itself. Everything
// after that judgement is checked here: the quote must appear in the document,
// the document is hashed so the proposal can go stale when the file changes,
// and nothing becomes active without review.
func (s *service) ExtractRules(ctx context.Context, req ExtractRulesRequest) (*ExtractRulesResult, error) {
	start := time.Now()

	if err := req.Caller.Validate(); err != nil {
		return nil, err
	}
	if !req.Caller.Has(CapPropose) {
		return nil, errorf(CodeProposalDisabled,
			"client profile %q may not propose records", req.Caller.ClientID)
	}
	if s.documents == nil {
		return nil, errorf(CodeSourceUnavailable,
			"no document roots are configured; set document_roots in the configuration to allow reading instruction files")
	}
	if strings.TrimSpace(req.DocumentPath) == "" {
		return nil, errorf(CodeInvalidRecord, "a document path is required")
	}
	if len(req.Rules) == 0 {
		return nil, errorf(CodeInvalidRecord, "at least one rule is required")
	}
	if req.Authority == "" {
		req.Authority = s.documentAuthority
	}
	if !req.Authority.Valid() {
		return nil, errorf(CodeInvalidRecord, "unknown authority %q", req.Authority)
	}
	if len(req.Rules) > maxExtractedRules {
		return nil, errorf(CodeInvalidRecord,
			"at most %d rules may be extracted at once; split the document", maxExtractedRules)
	}

	doc, err := s.documents.Read(ctx, req.DocumentPath)
	if err != nil {
		return nil, err
	}

	now := s.clock.Now()
	lines := strings.Split(doc.Content, "\n")

	result := &ExtractRulesResult{DocumentPath: doc.Path, ContentHash: doc.ContentHash}

	for _, rule := range req.Rules {
		outcome := s.extractOne(ctx, req, rule, doc, lines, now)
		switch {
		case outcome.rejected != nil:
			result.Rejected = append(result.Rejected, *outcome.rejected)
		case outcome.blocked != nil:
			result.Blocked = append(result.Blocked, *outcome.blocked)
		default:
			result.Accepted = append(result.Accepted, *outcome.accepted)
			switch {
			case !outcome.accepted.Created:
				result.PendingCount++
			case len(outcome.accepted.NeedsReview) > 0:
				result.ReviewCount++
			default:
				result.ActivatedCount++
			}
		}
	}

	if len(result.Rejected) > 0 {
		result.Warnings = append(result.Warnings, Warning{
			Code: WarnSourceUnavailable,
			Message: fmt.Sprintf(
				"%d rule(s) were refused; see each one's reason and correct it rather than refiling as is",
				len(result.Rejected)),
		})
	}
	retired, err := s.retireVanishedRules(ctx, doc)
	if err != nil {
		return nil, err
	}
	result.Retired = retired
	if len(retired) > 0 {
		result.Warnings = append(result.Warnings, Warning{
			Code: WarnSupersededRecord,
			Message: fmt.Sprintf(
				"%d record(s) from this document quoted lines it no longer contains, and were removed",
				len(retired)),
			RecordIDs: retired,
		})
	}

	result.Uncovered = uncoveredLines(doc, req.Rules)
	if len(result.Uncovered) > 0 {
		result.Warnings = append(result.Warnings, Warning{
			Code: WarnRecordNotFound,
			Message: fmt.Sprintf(
				"%d line(s) the document presents as rules are not covered by any quote you sent; "+
					"file them or say why they do not belong",
				len(result.Uncovered)),
		})
	}
	if result.ReviewCount > 0 {
		result.Warnings = append(result.Warnings, Warning{
			Code: WarnConflict,
			Message: fmt.Sprintf(
				"%d rule(s) were held for review rather than activated; each says why, and the user decides those",
				result.ReviewCount),
		})
	}
	if len(result.Blocked) > 0 {
		result.Warnings = append(result.Warnings, Warning{
			Code: WarnRecordNotFound,
			Message: fmt.Sprintf(
				"%d rule(s) were NOT stored because the same quote has already been reviewed and decided; "+
					"the user must reopen or delete those proposals before they can be filed again",
				len(result.Blocked)),
		})
	}

	s.writeAudit(ctx, req.Caller, AuditEvent{
		Operation:   "extract_rules",
		Scope:       EffectiveScope{Principal: req.Caller.PrincipalID, Repository: req.Scope.Repository},
		ResultCount: len(result.Accepted),
	}, start)

	return result, nil
}

// extractOutcome is what happened to one rule. Exactly one field is set.
type extractOutcome struct {
	accepted *AcceptedRule
	rejected *RejectedRule
	blocked  *BlockedRule
}

func refuse(r RejectedRule) extractOutcome { return extractOutcome{rejected: &r} }

func (s *service) extractOne(ctx context.Context, req ExtractRulesRequest, rule ExtractedRule, doc *Document, lines []string, now time.Time) extractOutcome {
	statement := strings.TrimSpace(rule.Statement)
	if statement == "" {
		return refuse(RejectedRule{Quote: rule.Quote, Reason: "the rule has no statement"})
	}
	if !rule.Kind.Valid() {
		return refuse(RejectedRule{Statement: statement, Quote: rule.Quote,
			Reason: fmt.Sprintf("unknown record kind %q", rule.Kind)})
	}

	quote := strings.TrimSpace(rule.Quote)
	if quote == "" {
		return refuse(RejectedRule{Statement: statement,
			Reason: "the rule quotes nothing, so it cannot be checked against the document"})
	}

	line := findQuote(lines, quote)
	if line == 0 {
		return refuse(RejectedRule{Statement: statement, Quote: quote,
			Reason: "the quoted text does not appear in the document"})
	}

	scope := req.Scope.Clone()
	if rule.Scope != nil {
		scope = rule.Scope.Clone()
	}
	scope.Normalize()
	if scope.User == "" {
		scope.User = req.Caller.PrincipalID
	}
	// A condition matches only when a caller passes it, and no caller does
	// today. A record scoped to one can never be returned, which is exactly as
	// useless as a rule the document does not contain, so it is refused for the
	// same reason and at the same moment.
	if len(scope.Conditions) > 0 {
		return refuse(RejectedRule{Statement: statement, Quote: quote,
			Reason: "the scope uses a condition, which only matches when a caller supplies it; " +
				"scope by repository, path, or task kind instead"})
	}
	if scope.Repository != "" && !req.Caller.RepositoryAllowed(scope.Repository) {
		return refuse(RejectedRule{Statement: statement, Quote: quote,
			Reason: fmt.Sprintf("client may not propose records for repository %s", scope.Repository)})
	}

	subject := strings.TrimSpace(rule.Subject)
	if subject == "" {
		subject = deriveSubject(statement)
	}

	evidence := Source{
		ID:      NewID("src"),
		Type:    SourceFile,
		Locator: "file://" + doc.Path,
		// The document's hash, so the record is revalidated against the file
		// and goes stale by itself once the file changes. That is what makes
		// activating without review safe rather than reckless.
		ContentHash:      doc.ContentHash,
		ExactExcerpt:     fmt.Sprintf("line %d: %s", line, quote),
		CapturedAt:       now,
		ValidationPolicy: ValidateQuotePresent,
	}

	// The key is the document and the quote, normalized the same way the quote
	// is matched. Two callers quoting one line, one of them keeping its "- "
	// bullet marker, mean the same rule and must not become two records.
	key := DocumentRuleKey(doc.Path, quote)

	candidate := &Record{
		ID:               RecordIDForKey(key),
		Kind:             rule.Kind,
		Subject:          subject,
		Statement:        statement,
		Rationale:        strings.TrimSpace(rule.Rationale),
		Scope:            scope,
		Authority:        req.Authority,
		Status:           StatusActive,
		ValidationPolicy: ValidateQuotePresent,
		Tags:             slices.Clone(rule.Tags),
		Sources:          []Source{evidence},
	}
	candidate.Normalize(now)

	flags, err := s.triage(ctx, candidate, quote, doc.Path)
	if err != nil {
		return refuse(RejectedRule{Statement: statement, Quote: quote, Reason: err.Error()})
	}

	// Anything the triage flagged goes to a person. Everything else is a rule
	// the user already wrote, quoted from their own document, and it becomes a
	// record now.
	if len(flags) == 0 {
		if existing, err := s.recordForKey(ctx, candidate.ID); err != nil {
			return refuse(RejectedRule{Statement: statement, Quote: quote, Reason: err.Error()})
		} else if existing != nil {
			return extractOutcome{accepted: &AcceptedRule{
				RecordID: existing.ID, Subject: existing.Subject,
				Statement: existing.Statement, Line: line, Created: false,
			}}
		}
		if err := candidate.Validate(); err != nil {
			return refuse(RejectedRule{Statement: statement, Quote: quote, Reason: err.Error()})
		}
		if err := s.store.PutRecord(ctx, candidate); err != nil {
			return refuse(RejectedRule{Statement: statement, Quote: quote,
				Reason: "could not be stored: " + err.Error()})
		}
		return extractOutcome{accepted: &AcceptedRule{
			RecordID: candidate.ID, Subject: candidate.Subject,
			Statement: candidate.Statement, Line: line, Created: true,
		}}
	}

	proposal := &Proposal{
		ID:           NewID("prop"),
		Key:          key,
		Status:       ProposalPending,
		PrincipalID:  req.Caller.PrincipalID,
		ClientID:     req.Caller.ClientID,
		Kind:         rule.Kind,
		Subject:      subject,
		Statement:    statement,
		Rationale:    strings.TrimSpace(rule.Rationale),
		Scope:        scope,
		Tags:         slices.Clone(rule.Tags),
		Evidence:     []Source{evidence},
		CreatedAt:    now,
		DecisionNote: "held for review: " + describeFlags(flags),
	}

	stored, created, err := s.store.PutProposal(ctx, proposal)
	if err != nil {
		return refuse(RejectedRule{Statement: statement, Quote: quote,
			Reason: "could not be stored: " + err.Error()})
	}
	if !created && stored.Status != ProposalPending {
		return extractOutcome{blocked: &BlockedRule{
			Statement:  statement,
			ProposalID: stored.ID,
			Status:     stored.Status,
			Reason: fmt.Sprintf(
				"this quote was already %s, so nothing was stored; ask the user to reopen or delete proposal %s",
				stored.Status, stored.ID),
		}}
	}

	return extractOutcome{accepted: &AcceptedRule{
		ProposalID: stored.ID, Subject: stored.Subject, Statement: stored.Statement,
		Line: line, Created: created, NeedsReview: flags,
	}}
}

// DocumentRuleKey identifies one rule within one document. It is exported
// because approving a proposal has to arrive at the same identity that direct
// activation would, or the two paths produce two records for one rule.
func DocumentRuleKey(path, quote string) string {
	return "doc:" + path + ":" + HashContent(normalizeQuote(quote))
}

// RecordIDForKey derives a record's identifier from the document and quote it
// came from. Two extractions of the same line therefore address one record,
// which makes a re-run idempotent without needing a lookup column.
func RecordIDForKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "rec_" + idEncoding.EncodeToString(sum[:12])
}

// recordForKey finds a record an earlier extraction already made from the same
// quote in the same document, so a re-run neither duplicates nor overwrites it.
func (s *service) recordForKey(ctx context.Context, id string) (*Record, error) {
	rec, err := s.store.GetRecord(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf(`cannot check for an existing record: %w`, err)
	}
	return rec, nil
}

func describeFlags(flags []ReviewFlag) string {
	parts := make([]string, 0, len(flags))
	for _, f := range flags {
		parts = append(parts, string(f.Reason))
	}
	return strings.Join(parts, ", ")
}

// retireVanishedRules deletes records drawn from this document whose quoted
// line is no longer in it.
//
// Editing a document gives its rules new quotes and therefore new records, and
// without this the old ones stay active while pointing at text that is gone.
// They are deleted rather than archived, because the document is the source of
// truth and its own history already records what it used to say. Keeping a
// second copy here only clutters every listing with rules that no longer exist.
func (s *service) retireVanishedRules(ctx context.Context, doc *Document) ([]string, error) {
	existing, err := s.store.QueryRecords(ctx, RecordQuery{
		Statuses: []RecordStatus{StatusActive},
		Limit:    maxExtractedRules * 4,
	})
	if err != nil {
		return nil, wrapf(CodeStorage, err, "cannot load records for this document")
	}

	lines := strings.Split(doc.Content, "\n")
	locator := "file://" + doc.Path

	var retired []string
	for _, rec := range existing {
		quote, ok := quotedFrom(rec, locator)
		if !ok {
			continue
		}
		if findQuote(lines, quote) > 0 {
			continue
		}
		if err := s.store.DeleteRecord(ctx, rec.ID); err != nil {
			return nil, wrapf(CodeStorage, err, "cannot remove record %s", rec.ID)
		}
		retired = append(retired, rec.ID)
	}
	return retired, nil
}

// quotedFrom returns the text a record quoted from one document, with the line
// prefix the extraction added stripped back off.
func quotedFrom(rec *Record, locator string) (string, bool) {
	for _, src := range rec.Sources {
		if src.Locator != locator {
			continue
		}
		quote := src.ExactExcerpt
		if _, rest, ok := strings.Cut(quote, ": "); ok && strings.HasPrefix(quote, "line ") {
			quote = rest
		}
		if quote != "" {
			return quote, true
		}
	}
	return "", false
}

// uncoveredLines reports what the document's own structure marks as a rule that
// no submitted quote accounts for.
//
// This runs on every extraction rather than being something a caller remembers
// to check. A model deciding what counts as a rule will sometimes keep a
// section's headline and drop the table of specifics underneath it, and the
// quote check cannot see that: the headline really is in the document, so the
// record is properly grounded and merely toothless. Only comparing against the
// whole document catches an omission.
func uncoveredLines(doc *Document, rules []ExtractedRule) []UncoveredLine {
	outline := (&Distiller{}).DistillContent(doc.Content, doc.Path, doc.ContentHash)
	if len(outline.Lines) == 0 {
		return nil
	}

	lines := strings.Split(doc.Content, "\n")
	covered := make(map[int]struct{}, len(rules))
	for _, rule := range rules {
		if line := findQuote(lines, rule.Quote); line > 0 {
			covered[line] = struct{}{}
		}
	}

	var out []UncoveredLine
	for i, line := range outline.Lines {
		if _, ok := covered[line]; ok {
			continue
		}
		text := ""
		if line-1 < len(lines) {
			text = strings.TrimSpace(lines[line-1])
		}
		out = append(out, UncoveredLine{Line: line, Text: text})
		_ = i
	}
	return out
}

// findQuote returns the 1-based line where a quote begins, or zero when the
// document does not contain it. Whitespace is normalized on both sides, because
// a model retyping a line will not reproduce its indentation, but the words
// themselves must match.
func findQuote(lines []string, quote string) int {
	want := normalizeQuote(quote)
	if want == "" {
		return 0
	}

	for i, line := range lines {
		if strings.Contains(normalizeQuote(line), want) {
			return i + 1
		}
	}

	// A quote may span lines, so fall back to matching against the whole
	// document with its line breaks flattened.
	joined := make([]string, 0, len(lines))
	for i, line := range lines {
		joined = append(joined, normalizeQuote(line))
		if strings.Contains(strings.Join(joined, " "), want) {
			return firstLineOfSpan(joined, want, i)
		}
	}
	return 0
}

// firstLineOfSpan finds where a multi-line match started, so the recorded line
// points at the beginning of the rule rather than its last line.
func firstLineOfSpan(joined []string, want string, end int) int {
	for start := end; start >= 0; start-- {
		if !strings.Contains(strings.Join(joined[start:end+1], " "), want) {
			return start + 2
		}
	}
	return 1
}

func normalizeQuote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "-*+#> \t")
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
