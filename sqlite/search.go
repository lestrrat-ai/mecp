package sqlite

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/lestrrat-ai/mecp"
)

// maxQueryTerms bounds how many tokens of a caller-supplied query reach FTS5.
// A task description can be thousands of words; the index does not get better
// after the first couple of dozen distinct terms, and an unbounded expression
// is a denial-of-service surface.
const maxQueryTerms = 24

// minPrefixLength is the shortest token that gets prefix matching. Prefixing
// two-letter tokens matches most of the index.
const minPrefixLength = 4

// bm25Weights weights the indexed columns. Subject and statement carry the
// normalized assertion, so they outweigh rationale and the verbatim evidence
// excerpt, which is untrusted text and should not dominate ranking.
const bm25Weights = `0.0, 5.0, 3.0, 1.5, 2.0, 0.5`

// defaultMinScore is the absolute bm25 floor a hit must clear to be returned
// at all, regardless of how it compares to other hits in the same result set.
// Relevance alone cannot express this: it is normalized against the best hit
// in the same call, so a query that matches nothing relevant still returns a
// top hit scored a confident 1.00. In principle this is the floor that lets a
// query the store cannot answer come back empty.
//
// It defaults to 0 (disabled) because bm25's magnitude is corpus-relative: it
// comes from an inverse-document-frequency term that shrinks toward zero as a
// corpus gets smaller, so a fixed absolute floor tuned against one store size
// silently swallows every real match in a smaller one. This was found by
// running the existing small-corpus unit tests against a floor of 5.5, tuned
// with margin below the weakest genuine match in a 177-record corpus, 5.85:
// every one of them lost its expected hit. A deployment with a large, stable corpus
// that wants this floor anyway can opt in with sqlite.WithMinSearchScore; it
// is a policy choice, not a mechanism the store should force on everyone.
const defaultMinScore = 0

// defaultMinRelevance is the relative floor: a hit scoring below this
// fraction of the best hit in the same result set is dropped as noise
// trailing behind a good answer, rather than kept just because something else
// matched worse. Unlike defaultMinScore this is corpus-size safe, since a
// lone hit is always relevant to itself (relevance 1.0) whatever the corpus:
// it only ever trims a weaker hit that is genuinely far behind a better one in
// the same call. The value leaves comfortable margin below the weakest
// genuine match's own relative score, 0.871, measured over a 177-record
// corpus of 28 labelled queries. It is a policy choice, not a mechanism, so
// it is also settable via sqlite.WithMinSearchRelevance.
const defaultMinRelevance = 0.7

// SearchRecords applies the structured pre-filter and then ranks the survivors
// lexically. Authorization happens in the same WHERE clause as the MATCH, so a
// disallowed row never contributes to a score or a count.
func (s *Store) SearchRecords(ctx context.Context, q mecp.SearchQuery) ([]mecp.ScoredRecord, error) {
	terms := QueryTerms(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}

	where, args, ok := buildWhere(q.RecordQuery)
	if !ok {
		return nil, nil
	}

	matchArgs := append([]any{ftsExpression(terms)}, args...)
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT ` + recordColumns + `, bm25(records_fts, ` + bm25Weights + `) AS score
		FROM records_fts
		JOIN records r ON r.id = records_fts.record_id
		JOIN record_scopes sc ON sc.record_id = r.id
		WHERE records_fts MATCH ? AND ` + strings.Join(where, " AND ") + `
		ORDER BY score, r.id
		LIMIT ?`
	matchArgs = append(matchArgs, limit)

	rows, err := s.db.QueryContext(ctx, query, matchArgs...)
	if err != nil {
		return nil, fmt.Errorf(`failed to search records: %w`, err)
	}
	defer rows.Close()

	var (
		recs   []*mecp.Record
		scores []float64
		best   float64
	)
	for rows.Next() {
		rec, score, err := scanScoredRecord(rows)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		// bm25 returns a negative number where a smaller value is a better
		// match, so flip the sign to get an increasing relevance.
		scores = append(scores, -score)
		if -score > best {
			best = -score
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if _, err := s.hydrate(ctx, recs); err != nil {
		return nil, err
	}

	out := make([]mecp.ScoredRecord, 0, len(recs))
	for i, rec := range recs {
		if scores[i] < s.minScore {
			continue
		}
		relevance := 0.5
		if best > 0 {
			relevance = scores[i] / best
		}
		if relevance < s.minRelevance {
			continue
		}
		out = append(out, mecp.ScoredRecord{
			Record:    rec,
			Relevance: relevance,
			RawScore:  scores[i],
			Terms:     matchingTerms(terms, rec),
		})
	}
	return out, nil
}

func scanScoredRecord(rows rowScanner) (*mecp.Record, float64, error) {
	var score float64
	rec, err := scanRecord(rows, &score)
	if err != nil {
		return nil, 0, err
	}
	return rec, score, nil
}

// QueryTerms reduces a natural-language query to the distinct, meaningful
// tokens worth sending to the index.
func QueryTerms(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' && r != '/'
	})

	var out []string
	for _, f := range fields {
		f = strings.Trim(f, "-._/")
		if len(f) < 2 {
			continue
		}
		if _, stop := stopWords[f]; stop {
			continue
		}
		out = append(out, f)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > maxQueryTerms {
		out = out[:maxQueryTerms]
	}
	return out
}

// ftsExpression renders terms as a quoted FTS5 OR expression. Quoting is what
// keeps caller text from being interpreted as FTS operators.
func ftsExpression(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		quoted := `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		if len([]rune(t)) >= minPrefixLength {
			quoted += "*"
		}
		parts = append(parts, quoted)
	}
	return strings.Join(parts, " OR ")
}

// matchingTerms reports which query terms actually appear in a record, for the
// human-readable match reasons attached to results.
func matchingTerms(terms []string, rec *mecp.Record) []string {
	haystack := strings.ToLower(rec.Subject + " " + rec.Statement + " " + rec.Rationale + " " + strings.Join(rec.Tags, " "))
	var out []string
	for _, t := range terms {
		if strings.Contains(haystack, t) {
			out = append(out, t)
		}
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

// stopWords are dropped from queries. The list is deliberately short: an
// aggressive stop list hurts recall on technical prose.
var stopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {}, "but": {},
	"by": {}, "do": {}, "does": {}, "for": {}, "from": {}, "has": {}, "have": {},
	"how": {}, "if": {}, "in": {}, "into": {}, "is": {}, "it": {}, "its": {}, "of": {},
	"on": {}, "or": {}, "that": {}, "the": {}, "their": {}, "then": {}, "there": {},
	"these": {}, "they": {}, "this": {}, "to": {}, "was": {}, "were": {}, "what": {},
	"when": {}, "which": {}, "why": {}, "will": {}, "with": {}, "you": {}, "your": {},
	"can": {}, "should": {}, "use": {}, "my": {},
}
