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

// minQuoteOverlap is how much of the original wording a statement must retain
// before it is taken as a rewording rather than a rewrite. Normalizing a table
// row into a sentence drops a lot of punctuation and little meaning, so the bar
// is deliberately low; it catches a statement that shares almost nothing with
// its source.
const minQuoteOverlap = 0.34

// ReviewFlag is one reason a rule was held back.
type ReviewFlag struct {
	Reason  ReviewReason `json:"reason"`
	Detail  string       `json:"detail"`
	Related []string     `json:"related_record_ids,omitempty"`
}

// triage decides whether a rule can be activated directly, and says why not
// when it cannot.
func (s *service) triage(ctx context.Context, rec *Record, quote string) ([]ReviewFlag, error) {
	var flags []ReviewFlag

	if overlap := quoteOverlap(rec.Statement, quote); overlap < minQuoteOverlap {
		flags = append(flags, ReviewFlag{
			Reason: ReviewDrifted,
			Detail: fmt.Sprintf(
				"the statement keeps %.0f%% of the wording of the line it came from, which is little enough that the meaning may have changed",
				overlap*100),
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
		if statementSimilarity(other.Statement, rec.Statement) >= 0.8 {
			duplicates = append(duplicates, other.ID)
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

// quoteOverlap is the share of the quote's words that survive into the
// statement. It measures in one direction on purpose: a statement may add words
// to make the rule readable out of context, and that is normalization working
// rather than drift.
func quoteOverlap(statement, quote string) float64 {
	want := contentWords(quote)
	if len(want) == 0 {
		return 1
	}
	have := contentWords(statement)

	var kept int
	for w := range want {
		if _, ok := have[w]; ok {
			kept++
		}
	}
	return float64(kept) / float64(len(want))
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

var triageStopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "not": {}, "use": {}, "using": {}, "with": {},
	"when": {}, "that": {}, "this": {}, "into": {}, "than": {}, "then": {}, "them": {},
	"are": {}, "was": {}, "its": {}, "but": {}, "any": {}, "all": {}, "you": {},
	"your": {}, "from": {}, "line": {},
}
