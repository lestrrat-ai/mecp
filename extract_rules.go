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

// DocumentStore reads the documents that rules are extracted from.
//
// It is an interface so the domain package stays free of filesystem access, and
// so the implementation can enforce which documents are readable at all. That
// restriction matters: this is the one place where a caller names a path, and
// reporting whether a quote appears in a file is enough to read that file back
// a piece at a time.
type DocumentStore interface {
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

// ExtractRulesResult is the outcome of one extraction.
type ExtractRulesResult struct {
	DocumentPath  string         `json:"document_path"`
	ContentHash   string         `json:"content_hash"`
	Accepted      []AcceptedRule `json:"accepted"`
	Rejected      []RejectedRule `json:"rejected,omitempty"`
	CreatedCount  int            `json:"created_count"`
	ExistingCount int            `json:"existing_count"`
	Warnings      []Warning      `json:"warnings,omitempty"`
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
		accepted, rejected := s.extractOne(ctx, req, rule, doc, lines, now)
		if rejected != nil {
			result.Rejected = append(result.Rejected, *rejected)
			continue
		}
		result.Accepted = append(result.Accepted, *accepted)
		if accepted.Created {
			result.CreatedCount++
		} else {
			result.ExistingCount++
		}
	}

	if len(result.Rejected) > 0 {
		result.Warnings = append(result.Warnings, Warning{
			Code: WarnSourceUnavailable,
			Message: fmt.Sprintf(
				"%d rule(s) were refused because their quoted text does not appear in %s; re-read the document and quote it exactly",
				len(result.Rejected), doc.Path),
		})
	}

	s.writeAudit(ctx, req.Caller, AuditEvent{
		Operation:   "extract_rules",
		Scope:       EffectiveScope{Principal: req.Caller.PrincipalID, Repository: req.Scope.Repository},
		ResultCount: len(result.Accepted),
	}, start)

	return result, nil
}

func (s *service) extractOne(ctx context.Context, req ExtractRulesRequest, rule ExtractedRule, doc *Document, lines []string, now time.Time) (*AcceptedRule, *RejectedRule) {
	statement := strings.TrimSpace(rule.Statement)
	if statement == "" {
		return nil, &RejectedRule{Quote: rule.Quote, Reason: "the rule has no statement"}
	}
	if !rule.Kind.Valid() {
		return nil, &RejectedRule{Statement: statement, Quote: rule.Quote,
			Reason: fmt.Sprintf("unknown record kind %q", rule.Kind)}
	}

	quote := strings.TrimSpace(rule.Quote)
	if quote == "" {
		return nil, &RejectedRule{Statement: statement,
			Reason: "the rule quotes nothing, so it cannot be checked against the document"}
	}

	line := findQuote(lines, quote)
	if line == 0 {
		return nil, &RejectedRule{Statement: statement, Quote: quote,
			Reason: "the quoted text does not appear in the document"}
	}

	scope := req.Scope.Clone()
	if rule.Scope != nil {
		scope = rule.Scope.Clone()
	}
	scope.Normalize()
	if scope.User == "" {
		scope.User = req.Caller.PrincipalID
	}
	if scope.Repository != "" && !req.Caller.RepositoryAllowed(scope.Repository) {
		return nil, &RejectedRule{Statement: statement, Quote: quote,
			Reason: fmt.Sprintf("client may not propose records for repository %s", scope.Repository)}
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
		return nil, &RejectedRule{Statement: statement, Quote: quote,
			Reason: "could not be stored: " + err.Error()}
	}

	return &AcceptedRule{
		ProposalID: stored.ID,
		Subject:    stored.Subject,
		Statement:  stored.Statement,
		Line:       line,
		Created:    created,
	}, nil
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
