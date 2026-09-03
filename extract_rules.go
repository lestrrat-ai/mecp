package mecp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
}

// AcceptedRule reports one rule that became a proposal.
type AcceptedRule struct {
	ProposalID string `json:"proposal_id"`
	Subject    string `json:"subject"`
	Statement  string `json:"statement"`
	Line       int    `json:"line"`
	Created    bool   `json:"created"`
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

// ExtractRulesResult is the outcome of one extraction.
type ExtractRulesResult struct {
	DocumentPath string         `json:"document_path"`
	ContentHash  string         `json:"content_hash"`
	Accepted     []AcceptedRule `json:"accepted"`
	Rejected     []RejectedRule `json:"rejected,omitempty"`
	Blocked      []BlockedRule  `json:"blocked,omitempty"`
	// CreatedCount is how many proposals this call added to the queue.
	CreatedCount int `json:"created_count"`
	// PendingCount is how many were already waiting for review from an earlier
	// run of the same extraction.
	PendingCount int       `json:"pending_count"`
	Warnings     []Warning `json:"warnings,omitempty"`
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
			if outcome.accepted.Created {
				result.CreatedCount++
			} else {
				result.PendingCount++
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

	proposal := &Proposal{
		ID: NewID("prop"),
		// The key is the document and the quote, so re-running an extraction
		// over an unchanged document does not queue everything twice.
		Key:         "doc:" + doc.Path + ":" + HashContent(quote),
		Status:      ProposalPending,
		PrincipalID: req.Caller.PrincipalID,
		ClientID:    req.Caller.ClientID,
		Kind:        rule.Kind,
		Subject:     subject,
		Statement:   statement,
		Rationale:   strings.TrimSpace(rule.Rationale),
		Scope:       scope,
		Tags:        slices.Clone(rule.Tags),
		Evidence: []Source{{
			ID:      NewID("src"),
			Type:    SourceFile,
			Locator: "file://" + doc.Path,
			// The document's hash, so an approved record can be revalidated
			// against the file and go stale once the file changes.
			ContentHash:      doc.ContentHash,
			ExactExcerpt:     fmt.Sprintf("line %d: %s", line, quote),
			CapturedAt:       now,
			ValidationPolicy: ValidateFileAndHash,
		}},
		CreatedAt: now,
	}

	stored, created, err := s.store.PutProposal(ctx, proposal)
	if err != nil {
		return refuse(RejectedRule{Statement: statement, Quote: quote,
			Reason: "could not be stored: " + err.Error()})
	}

	// A key that matches an already-decided proposal means nothing was stored.
	// Reporting that as an acceptance is how a caller comes to believe it filed
	// rules it did not.
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
		ProposalID: stored.ID,
		Subject:    stored.Subject,
		Statement:  stored.Statement,
		Line:       line,
		Created:    created,
	}}
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
