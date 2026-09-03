package mecp

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// MinimumTokenBudget is the smallest budget that can carry the mandatory
// header, warnings, and at least one record.
const MinimumTokenBudget = 256

// DefaultTokenBudget is used when a caller does not request one.
const DefaultTokenBudget = 3000

// packOverheadTokens is reserved for the summary, scope echo, conflicts, and
// warnings that accompany every pack.
const packOverheadTokens = 120

// itemOverheadTokens approximates the per-item metadata cost: identifiers,
// authority, status, scope label, and source references.
const itemOverheadTokens = 45

// ContextItem is one record as presented to an agent.
type ContextItem struct {
	RecordID         string          `json:"record_id"`
	Kind             RecordKind      `json:"kind"`
	Effect           Effect          `json:"effect"`
	Subject          string          `json:"subject"`
	Statement        string          `json:"statement"`
	Rationale        string          `json:"rationale,omitempty"`
	Authority        Authority       `json:"authority"`
	Status           RecordStatus    `json:"status"`
	ScopeSpecificity string          `json:"scope_specificity"`
	Validation       ValidationState `json:"validation"`
	LastVerifiedAt   *time.Time      `json:"last_verified_at,omitempty"`
	SourceRefs       []string        `json:"source_refs"`
	MatchReasons     []string        `json:"match_reasons,omitempty"`
	EvidenceSummary  string          `json:"evidence_summary,omitempty"`
}

// BudgetReport tells the caller how much of the requested budget was used. The
// figures are approximate because the service does not know the host model's
// tokenizer.
type BudgetReport struct {
	RequestedTokens     int  `json:"requested_tokens"`
	EstimatedTokensUsed int  `json:"estimated_tokens_used"`
	Approximate         bool `json:"approximate"`
	Truncated           bool `json:"truncated"`
	OmittedItemCount    int  `json:"omitted_item_count"`
}

// EstimateTokens returns a conservative character-based token estimate.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len([]rune(s)) + 3) / 4
}

// Packer selects the highest-value candidates that fit a budget.
type Packer interface {
	Pack(cands []*Candidate, budget int, includeEvidence bool) ([]ContextItem, BudgetReport)
}

// NewPacker returns the default packer, which fills the budget in the priority
// order given in the design: applicable constraints, then decisions and
// rejected alternatives, then preferences, then open questions, then history.
func NewPacker() Packer { return defaultPacker{} }

type defaultPacker struct{}

func (defaultPacker) Pack(cands []*Candidate, budget int, includeEvidence bool) ([]ContextItem, BudgetReport) {
	report := BudgetReport{RequestedTokens: budget, Approximate: true}

	ordered := make([]*Candidate, len(cands))
	copy(ordered, cands)
	// A stable sort by group keeps the ranker's order inside each group.
	sort.SliceStable(ordered, func(i, j int) bool {
		return packGroup(ordered[i]) < packGroup(ordered[j])
	})

	available := budget - packOverheadTokens
	items := make([]ContextItem, 0, len(ordered))
	for _, c := range ordered {
		item := toContextItem(c, includeEvidence)
		cost := itemCost(item)
		if len(items) > 0 && report.EstimatedTokensUsed+cost > available {
			report.Truncated = true
			report.OmittedItemCount++
			continue
		}
		items = append(items, item)
		report.EstimatedTokensUsed += cost
	}

	report.EstimatedTokensUsed += packOverheadTokens
	return items, report
}

func itemCost(item ContextItem) int {
	return itemOverheadTokens +
		EstimateTokens(item.Statement) +
		EstimateTokens(item.Rationale) +
		EstimateTokens(item.Subject) +
		EstimateTokens(item.EvidenceSummary) +
		EstimateTokens(strings.Join(item.MatchReasons, " "))
}

// packGroup returns the packing priority band of a candidate. Lower is packed
// first.
func packGroup(c *Candidate) int {
	if c.Effect == EffectConstraint {
		return 0
	}
	switch c.Record.Kind {
	case KindDecision, KindRejectedAlternative:
		return 1
	case KindPreference:
		return 2
	case KindOpenQuestion:
		return 3
	case KindHistoricalEvent, KindProjectFact, KindArtifactReference:
		return 4
	default:
		return 5
	}
}

func toContextItem(c *Candidate, includeEvidence bool) ContextItem {
	rec := c.Record
	item := ContextItem{
		RecordID:         rec.ID,
		Kind:             rec.Kind,
		Effect:           c.Effect,
		Subject:          rec.Subject,
		Statement:        rec.Statement,
		Rationale:        rec.Rationale,
		Authority:        rec.Authority,
		Status:           rec.Status,
		ScopeSpecificity: c.Scope.Label,
		Validation:       c.Validation.State,
		LastVerifiedAt:   rec.LastVerifiedAt,
		SourceRefs:       make([]string, 0, len(rec.Sources)),
		MatchReasons:     c.MatchReasons,
	}
	if item.ScopeSpecificity == "" {
		item.ScopeSpecificity = rec.Scope.SpecificityLabel()
	}
	for _, src := range rec.Sources {
		item.SourceRefs = append(item.SourceRefs, src.ID)
	}
	if includeEvidence {
		item.EvidenceSummary = evidenceSummary(rec)
	}
	return item
}

// evidenceSummary names the kinds of sources behind a record without quoting
// them. Verbatim excerpts are reached through get_records, which applies the
// evidence capability separately.
func evidenceSummary(rec *Record) string {
	if len(rec.Sources) == 0 {
		return ""
	}
	counts := make(map[SourceType]int, len(rec.Sources))
	for _, src := range rec.Sources {
		counts[src.Type]++
	}
	parts := make([]string, 0, len(counts))
	for _, t := range AllSourceTypes {
		if n, ok := counts[t]; ok {
			parts = append(parts, pluralize(n, string(t)))
		}
	}
	return "backed by " + strings.Join(parts, ", ")
}

func pluralize(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
