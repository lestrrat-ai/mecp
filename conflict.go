package mecp

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ConflictRecommendation is the service's deterministic advice for handling a
// pair of records that disagree. It is derived from authority, lifecycle, and
// dates. No language model participates: a model may one day be used to notice
// that two records disagree, but never to decide which one wins.
type ConflictRecommendation string

const (
	ConflictPreferNewerAuthoritative ConflictRecommendation = "prefer_newer_authoritative_source"
	ConflictAskUser                  ConflictRecommendation = "ask_user_if_material"
	ConflictIgnoreHistorical         ConflictRecommendation = "ignore_historical_record"
	ConflictRevalidateRepository     ConflictRecommendation = "repository_source_requires_revalidation"
)

// Conflict reports two or more active records that apply to the same scope and
// subject without a supersession relationship between them.
type Conflict struct {
	Subject        string                 `json:"subject"`
	RecordIDs      []string               `json:"record_ids"`
	Recommendation ConflictRecommendation `json:"recommendation"`
	Explanation    string                 `json:"explanation"`
}

// DetectConflicts groups applicable candidates by explicit conflict group, or
// by normalized subject when no group was declared, and reports groups whose
// members disagree.
//
// Only records that would act on the agent are considered. Two informational
// history entries about the same subject are not a conflict; they are history.
func DetectConflicts(cands []*Candidate) []Conflict {
	groups := make(map[string][]*Candidate)
	for _, c := range cands {
		if !conflictEligible(c) {
			continue
		}
		key := c.Record.ConflictGroup
		if key == "" {
			key = "subject:" + c.Record.NormalizedSubject()
		}
		groups[key] = append(groups[key], c)
	}

	var out []Conflict
	for key, members := range groups {
		if len(members) < 2 {
			continue
		}
		if !membersDisagree(members) {
			continue
		}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.Record.ID)
		}
		slices.Sort(ids)

		rec, why := recommendation(members)
		out = append(out, Conflict{
			Subject:        conflictSubject(key, members),
			RecordIDs:      ids,
			Recommendation: rec,
			Explanation:    why,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Subject != out[j].Subject {
			return out[i].Subject < out[j].Subject
		}
		return strings.Join(out[i].RecordIDs, ",") < strings.Join(out[j].RecordIDs, ",")
	})
	return out
}

func conflictSubject(key string, members []*Candidate) string {
	if strings.HasPrefix(key, "subject:") {
		return members[0].Record.Subject
	}
	return key
}

func conflictEligible(c *Candidate) bool {
	if c.Superseded || c.Record.SupersededBy != "" {
		return false
	}
	switch c.Record.Status {
	case StatusActive, StatusDisputed:
	default:
		return false
	}
	switch c.Record.Kind {
	case KindConstraint, KindDecision, KindPreference, KindProjectFact:
		return true
	default:
		return c.Record.ConflictGroup != ""
	}
}

// membersDisagree reports whether a group holds at least two materially
// different statements. Near-identical wording is redundancy, which the ranker
// already handles, not a disagreement worth interrupting the agent about.
//
// Two records extracted from one document are also not a disagreement. A
// heading covers several rules, so grouping by subject puts siblings together,
// and a document saying five things about killing processes is not a document
// contradicting itself.
func membersDisagree(members []*Candidate) bool {
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			a, b := members[i].Record, members[j].Record
			if slices.Contains(a.Supersedes, b.ID) || slices.Contains(b.Supersedes, a.ID) {
				continue
			}
			if shareASource(a, b) {
				continue
			}
			if statementSimilarity(a.Statement, b.Statement) < 0.8 {
				return true
			}
		}
	}
	return false
}

// shareASource reports whether two records were drawn from the same artifact,
// which makes them siblings rather than rivals.
func shareASource(a, b *Record) bool {
	if len(a.Sources) == 0 || len(b.Sources) == 0 {
		return false
	}
	locators := make(map[string]struct{}, len(a.Sources))
	for _, src := range a.Sources {
		if src.Locator != "" {
			locators[src.Locator] = struct{}{}
		}
	}
	for _, src := range b.Sources {
		if _, ok := locators[src.Locator]; ok {
			return true
		}
	}
	return false
}

// statementSimilarity is the Jaccard overlap of the two statements' word sets.
func statementSimilarity(a, b string) float64 {
	as := wordSet(a)
	bs := wordSet(b)
	if len(as) == 0 || len(bs) == 0 {
		return 0
	}
	var inter int
	for w := range as {
		if _, ok := bs[w]; ok {
			inter++
		}
	}
	union := len(as) + len(bs) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func wordSet(s string) map[string]struct{} {
	fields := strings.Fields(strings.ToLower(s))
	out := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, ".,;:()\"'`")
		if f == "" {
			continue
		}
		out[f] = struct{}{}
	}
	return out
}

func recommendation(members []*Candidate) (ConflictRecommendation, string) {
	sorted := slices.Clone(members)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Record, sorted[j].Record
		if at, bt := a.Authority.Tier(), b.Authority.Tier(); at != bt {
			return at > bt
		}
		if !a.ValidFrom.Equal(b.ValidFrom) {
			return a.ValidFrom.After(b.ValidFrom)
		}
		return a.ID < b.ID
	})
	top, next := sorted[0].Record, sorted[1].Record

	if top.Authority == AuthorityRepository && next.Authority != AuthorityRepository {
		return ConflictRevalidateRepository, fmt.Sprintf(
			"%s comes from repository-authoritative material and disagrees with %s; confirm the checked-in source before acting on either",
			top.ID, next.ID)
	}
	if next.Kind == KindHistoricalEvent || next.Status != StatusActive {
		return ConflictIgnoreHistorical, fmt.Sprintf(
			"%s is historical or no longer active; prefer %s", next.ID, top.ID)
	}
	if top.Authority.Tier() > next.Authority.Tier() && !top.ValidFrom.Before(next.ValidFrom) {
		return ConflictPreferNewerAuthoritative, fmt.Sprintf(
			"%s carries higher authority (%s) and is not older than %s (%s)",
			top.ID, top.Authority, next.ID, next.Authority)
	}
	return ConflictAskUser, fmt.Sprintf(
		"%s and %s apply to the same scope with comparable authority and neither supersedes the other",
		top.ID, next.ID)
}
