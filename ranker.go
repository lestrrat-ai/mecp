package mecp

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Candidate is one record under consideration, together with everything the
// ranker and packer learned about it. Nothing here is persisted; a candidate
// exists only for the duration of a single call.
type Candidate struct {
	Record       *Record
	Relevance    float64
	Scope        ScopeMatch
	Validation   ValidationStatus
	Effect       Effect
	Score        float64
	MatchReasons []string
	Superseded   bool
	Duplicate    bool
}

// RankRequest carries the per-call context a ranker needs. It is passed by
// value so that ranking cannot mutate service state.
type RankRequest struct {
	Query     string
	TaskKind  TaskKind
	Workspace Workspace
	Now       time.Time
}

// Ranker orders candidates. The default implementation is deterministic and
// explains itself, because a ranking an agent cannot inspect is a ranking the
// user cannot correct.
type Ranker interface {
	// Version identifies the scoring model. It participates in cache keys.
	Version() string
	// Rank scores candidates in place and sorts them best-first.
	Rank(req RankRequest, cands []*Candidate)
}

// RankWeights are the tunable coefficients of the default ranker. They are
// configuration, not part of any public contract.
type RankWeights struct {
	Relevance         float64
	Specificity       float64
	Authority         float64
	Freshness         float64
	Recency           float64
	KindPriority      float64
	ExactMatch        float64
	StalePenalty      float64
	SupersededPenalty float64
	RedundancyPenalty float64
}

// DefaultRankWeights returns the shipped scoring model. Authority outweighs
// lexical relevance on purpose: a well-worded agent inference must not outrank
// an explicit user decision that merely uses different words.
func DefaultRankWeights() RankWeights {
	return RankWeights{
		Relevance:         1.0,
		Specificity:       1.2,
		Authority:         1.5,
		Freshness:         0.8,
		Recency:           0.3,
		KindPriority:      0.9,
		ExactMatch:        0.6,
		StalePenalty:      1.0,
		SupersededPenalty: 2.5,
		RedundancyPenalty: 0.8,
	}
}

// NewRanker returns the default deterministic ranker.
func NewRanker(w RankWeights) Ranker { return &defaultRanker{w: w} }

type defaultRanker struct {
	w RankWeights
}

func (r *defaultRanker) Version() string { return "deterministic/1" }

func (r *defaultRanker) Rank(req RankRequest, cands []*Candidate) {
	terms := identifierTokens(req.Query)
	for _, c := range cands {
		r.score(req, terms, c)
	}
	sortCandidates(cands)
	r.penalizeRedundancy(cands)
	sortCandidates(cands)
}

func (r *defaultRanker) score(req RankRequest, terms []string, c *Candidate) {
	rec := c.Record
	var reasons []string

	score := r.w.Relevance * c.Relevance
	if c.Relevance > 0 {
		reasons = append(reasons, fmt.Sprintf("lexical relevance %.2f", c.Relevance))
	}

	specificity := float64(c.Scope.Specificity) / float64(MaxScopeSpecificity)
	score += r.w.Specificity * specificity
	reasons = append(reasons, c.Scope.Reasons...)

	authority := float64(rec.Authority.Tier()) / float64(len(AllAuthorities)-1)
	score += r.w.Authority * authority
	reasons = append(reasons, "authority: "+string(rec.Authority))

	freshness := freshnessScore(c.Validation.State)
	score += r.w.Freshness * freshness

	score += r.w.Recency * recencyScore(rec, req.Now)
	score += r.w.KindPriority * kindPriority(req.TaskKind, rec.Kind)

	if matched := matchedIdentifiers(terms, rec); len(matched) > 0 {
		score += r.w.ExactMatch * math.Min(1, float64(len(matched))/2)
		reasons = append(reasons, "exact match: "+strings.Join(matched, ", "))
	}

	switch c.Validation.State {
	case ValidationStale:
		score -= r.w.StalePenalty * 0.5
		reasons = append(reasons, "stale: "+c.Validation.Reason)
	case ValidationFailed:
		score -= r.w.StalePenalty
		reasons = append(reasons, "evidence unavailable: "+c.Validation.Reason)
	}

	if c.Superseded || rec.Status == StatusSuperseded {
		score -= r.w.SupersededPenalty
		reasons = append(reasons, "superseded by a newer record")
	}
	if rec.Status == StatusDisputed {
		score -= r.w.StalePenalty * 0.5
		reasons = append(reasons, "disputed: sources disagree")
	}

	score *= 0.5 + 0.5*rec.Confidence

	c.Score = score
	c.MatchReasons = dedupeStrings(append(reasons, c.MatchReasons...))
}

func (r *defaultRanker) penalizeRedundancy(cands []*Candidate) {
	seen := make(map[string]struct{}, len(cands))
	for _, c := range cands {
		key := c.Record.NormalizedSubject() + "|" + string(c.Record.Kind)
		if _, dup := seen[key]; dup {
			c.Duplicate = true
			c.Score -= r.w.RedundancyPenalty
			c.MatchReasons = append(c.MatchReasons, "redundant with a higher-ranked record on the same subject")
			continue
		}
		seen[key] = struct{}{}
	}
}

// sortCandidates orders best-first with a total order, so that two runs over
// the same data always produce the same result.
func sortCandidates(cands []*Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if at, bt := a.Record.Authority.Tier(), b.Record.Authority.Tier(); at != bt {
			return at > bt
		}
		if a.Scope.Specificity != b.Scope.Specificity {
			return a.Scope.Specificity > b.Scope.Specificity
		}
		if !a.Record.ValidFrom.Equal(b.Record.ValidFrom) {
			return a.Record.ValidFrom.After(b.Record.ValidFrom)
		}
		return a.Record.ID < b.Record.ID
	})
}

func freshnessScore(state ValidationState) float64 {
	switch state {
	case ValidationValid:
		return 1.0
	case ValidationUnverified:
		return 0.6
	case ValidationStale:
		return 0.1
	default:
		return 0
	}
}

// recencyScore decays linearly over two years. It is a tiebreaker, not a
// substitute for supersession: an old decision that nothing replaced is still
// the current decision.
func recencyScore(rec *Record, now time.Time) float64 {
	anchor := rec.ValidFrom
	if rec.LastVerifiedAt != nil && rec.LastVerifiedAt.After(anchor) {
		anchor = *rec.LastVerifiedAt
	}
	if anchor.IsZero() {
		return 0
	}
	age := now.Sub(anchor)
	if age <= 0 {
		return 1
	}
	const window = 2 * 365 * 24 * time.Hour
	if age >= window {
		return 0
	}
	return 1 - float64(age)/float64(window)
}

var baseKindPriority = map[RecordKind]float64{
	KindConstraint:          1.00,
	KindDecision:            0.90,
	KindPreference:          0.80,
	KindRejectedAlternative: 0.70,
	KindProjectFact:         0.60,
	KindOpenQuestion:        0.50,
	KindHistoricalEvent:     0.45,
	KindArtifactReference:   0.35,
	KindObservation:         0.30,
}

// taskKindBoost nudges the kinds that matter most for a given operation. The
// deltas are small: they reorder near-ties rather than overriding authority.
var taskKindBoost = map[TaskKind]map[RecordKind]float64{
	TaskCodeReview: {
		KindRejectedAlternative: 0.15,
		KindHistoricalEvent:     0.10,
		KindDecision:            0.05,
	},
	TaskSecurityReview: {
		KindConstraint:  0.15,
		KindProjectFact: 0.05,
	},
	TaskRelease: {
		KindProjectFact:     0.15,
		KindHistoricalEvent: 0.05,
	},
	TaskDesign: {
		KindRejectedAlternative: 0.15,
		KindOpenQuestion:        0.10,
	},
	TaskDebugging: {
		KindHistoricalEvent: 0.15,
		KindProjectFact:     0.10,
	},
	TaskResearch: {
		KindArtifactReference: 0.15,
		KindOpenQuestion:      0.10,
	},
	TaskImplementation: {
		KindPreference: 0.05,
		KindConstraint: 0.05,
	},
}

func kindPriority(task TaskKind, kind RecordKind) float64 {
	p := baseKindPriority[kind]
	if boosts, ok := taskKindBoost[task]; ok {
		p += boosts[kind]
	}
	return math.Min(p, 1.2)
}

// identifierPattern selects query tokens worth matching exactly: package paths,
// symbols, versions, issue numbers. Ordinary prose words are left to FTS.
var identifierPattern = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._/\-]{2,}`)

func identifierTokens(query string) []string {
	if query == "" {
		return nil
	}
	var out []string
	for _, tok := range identifierPattern.FindAllString(query, -1) {
		if !looksLikeIdentifier(tok) {
			continue
		}
		out = append(out, strings.ToLower(tok))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func looksLikeIdentifier(tok string) bool {
	if strings.ContainsAny(tok, "./_-") {
		return true
	}
	var hasUpper, hasDigit, hasLower bool
	for _, r := range tok {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	return hasDigit || (hasUpper && hasLower)
}

func matchedIdentifiers(terms []string, rec *Record) []string {
	if len(terms) == 0 {
		return nil
	}
	haystack := strings.ToLower(strings.Join([]string{rec.Subject, rec.Statement, rec.Scope.Repository, strings.Join(rec.Tags, " ")}, " "))
	var out []string
	for _, t := range terms {
		if strings.Contains(haystack, t) {
			out = append(out, t)
		}
	}
	return out
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
