package mecp

import (
	"context"
	"fmt"
	"strings"
)

// ReviewReason says why a rule needs a person to look at it. A rule with no
// reason is activated directly.
//
// The review step exists to stop an agent's guess becoming authority. A rule
// copied out of a document the user wrote, whose quote has already been checked
// against that document, is not a guess: what the model chose is the wording,
// the kind, and the scope. Those are worth checking only when they went
// somewhere surprising, and a queue holding everything is a queue nobody works.
type ReviewReason string

const (
	// ReviewDrifted means the statement no longer resembles the text it claims
	// to come from, so the normalization may have changed the meaning.
	ReviewDrifted ReviewReason = "statement_drifted_from_quote"
	// ReviewConflicts means an active record already says something different
	// about the same subject in an overlapping scope.
	ReviewConflicts ReviewReason = "conflicts_with_active_record"
	// ReviewDuplicates means an active record already covers this subject, so
	// activating this one would leave two records to keep in step.
	ReviewDuplicates ReviewReason = "duplicates_an_active_record"
)

// minQuoteGrounding is how much of a statement must come from the text it
// quotes before it is taken as a rewording rather than an invention.
const minQuoteGrounding = 0.34

// ReviewFlag is one reason a rule was held back.
type ReviewFlag struct {
	Reason  ReviewReason `json:"reason"`
	Detail  string       `json:"detail"`
	Related []string     `json:"related_record_ids,omitempty"`
}

// triage decides whether a rule can be activated directly, and says why not
// when it cannot.
//
// sourceDoc is the document the rule came from. Records already extracted from
// that same document are siblings rather than rivals: a heading covers several
// rules, and a document saying five things about killing processes is not a
// document contradicting itself.
func (s *service) triage(ctx context.Context, rec *Record, quote, sourceDoc string) ([]ReviewFlag, error) {
	var flags []ReviewFlag

	if grounding := quoteGrounding(rec.Statement, quote); grounding < minQuoteGrounding {
		flags = append(flags, ReviewFlag{
			Reason: ReviewDrifted,
			Detail: fmt.Sprintf(
				"only %.0f%% of the statement's wording appears in the line it quotes, so most of it came from somewhere else",
				grounding*100),
		})
	}

	// Only active records can be contradicted or duplicated. A superseded one
	// is history and is meant to differ.
	existing, err := s.store.QueryRecords(ctx, RecordQuery{
		PrincipalID: rec.Scope.User,
		Subject:     rec.Subject,
		Statuses:    []RecordStatus{StatusActive},
		Limit:       16,
	})
	if err != nil {
		return nil, wrapf(CodeStorage, err, "cannot check for existing records")
	}

	var duplicates, conflicts []string
	for _, other := range existing {
		if other.ID == rec.ID {
			continue
		}
		// A near-identical statement is a duplicate wherever it came from,
		// because two records saying the same thing have to be kept in step.
		if statementSimilarity(other.Statement, rec.Statement) >= 0.8 {
			duplicates = append(duplicates, other.ID)
			continue
		}
		if fromDocument(other, sourceDoc) {
			continue
		}
		conflicts = append(conflicts, other.ID)
	}

	if len(duplicates) > 0 {
		flags = append(flags, ReviewFlag{
			Reason:  ReviewDuplicates,
			Detail:  "an active record already says nearly the same thing about this subject",
			Related: duplicates,
		})
	}
	if len(conflicts) > 0 {
		flags = append(flags, ReviewFlag{
			Reason:  ReviewConflicts,
			Detail:  "an active record covers this subject and says something different",
			Related: conflicts,
		})
	}

	return flags, nil
}

// fromDocument reports whether a record was extracted from the given document.
func fromDocument(rec *Record, path string) bool {
	if path == "" {
		return false
	}
	for _, src := range rec.Sources {
		if strings.TrimPrefix(src.Locator, "file://") == path {
			return true
		}
	}
	return false
}

// quoteGrounding is the share of the statement's words that appear in the text
// it quotes.
//
// The direction matters. Asking how much of the quote survives punishes
// summarizing, and summarizing is the job: a table row carries a rule plus its
// explanation plus its formatting, and a good record keeps only the rule.
// Asking how much of the statement came from the quote instead catches the
// failure that matters, which is a statement mostly made up of words the source
// never used.
func quoteGrounding(statement, quote string) float64 {
	have := contentWords(statement)
	if len(have) == 0 {
		return 0
	}
	source := contentWords(quote)

	var grounded int
	for w := range have {
		if _, ok := source[w]; ok {
			grounded++
		}
	}
	return float64(grounded) / float64(len(have))
}

// contentWords reduces text to the words that carry meaning, dropping markup
// and the short function words that any two sentences share.
func contentWords(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, f := range strings.Fields(strings.ToLower(s)) {
		f = strings.Trim(f, "`*_~()[]{}<>.,;:!?\"'|—-")
		if len(f) < 3 {
			continue
		}
		if _, stop := triageStopWords[f]; stop {
			continue
		}
		out[f] = struct{}{}
	}
	return out
}

// triageStopWords are words that say nothing about whether a statement is
// grounded in its source. Alongside ordinary function words, this holds the
// imperative glue a normalized rule acquires: a source table row says "BANNED"
// where the record says "never run", and neither phrasing is evidence of
// invention.
var triageStopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "not": {}, "use": {}, "using": {}, "with": {},
	"when": {}, "that": {}, "this": {}, "into": {}, "than": {}, "then": {}, "them": {},
	"are": {}, "was": {}, "its": {}, "but": {}, "any": {}, "all": {}, "you": {},
	"your": {}, "from": {}, "line": {},
	"never": {}, "always": {}, "must": {}, "run": {}, "prefer": {}, "avoid": {},
	"instead": {}, "rather": {}, "only": {}, "banned": {}, "exceptions": {},
}
