package mecp

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RecordKind classifies what a record asserts. The kind determines the default
// effect the record has on an agent (see Effect) and how aggressively the
// record must be revalidated before it is presented as current guidance.
type RecordKind string

const (
	KindConstraint          RecordKind = "constraint"
	KindPreference          RecordKind = "preference"
	KindDecision            RecordKind = "decision"
	KindRejectedAlternative RecordKind = "rejected_alternative"
	KindHistoricalEvent     RecordKind = "historical_event"
	KindProjectFact         RecordKind = "project_fact"
	KindObservation         RecordKind = "observation"
	KindOpenQuestion        RecordKind = "open_question"
	KindArtifactReference   RecordKind = "artifact_reference"
)

// AllRecordKinds lists every valid RecordKind in a stable order.
var AllRecordKinds = []RecordKind{
	KindConstraint,
	KindPreference,
	KindDecision,
	KindRejectedAlternative,
	KindHistoricalEvent,
	KindProjectFact,
	KindObservation,
	KindOpenQuestion,
	KindArtifactReference,
}

func (k RecordKind) Valid() bool { return slices.Contains(AllRecordKinds, k) }

// Effect tells the agent how to treat a returned record. It is deliberately
// coarser than RecordKind: an agent must not treat every retrieved sentence as
// an instruction, so anything that is not a current, sufficiently authoritative
// rule degrades to informational.
type Effect string

const (
	EffectConstraint    Effect = "constraint"
	EffectPreference    Effect = "preference"
	EffectInformational Effect = "informational"
)

// Authority describes why a record should be trusted. It is independent from
// retrieval relevance: a high similarity score never promotes an agent
// inference above an explicit user decision.
type Authority string

const (
	AuthorityRepository Authority = "repository_authoritative"
	AuthorityUser       Authority = "explicit_user"
	AuthorityProject    Authority = "explicit_project"
	AuthorityImport     Authority = "sourced_import"
	AuthorityObserved   Authority = "observed_behavior"
	AuthorityInferred   Authority = "agent_inferred"
	AuthorityUnverified Authority = "unverified_import"
)

// AllAuthorities lists every valid Authority from strongest to weakest.
var AllAuthorities = []Authority{
	AuthorityRepository,
	AuthorityUser,
	AuthorityProject,
	AuthorityImport,
	AuthorityObserved,
	AuthorityInferred,
	AuthorityUnverified,
}

func (a Authority) Valid() bool { return slices.Contains(AllAuthorities, a) }

// Tier returns the precedence rank of an authority. Larger is stronger, and
// AuthorityUnverified is 0. An unknown authority also ranks 0 so that a
// corrupted or future value can never outrank a known one.
func (a Authority) Tier() int {
	idx := slices.Index(AllAuthorities, a)
	if idx < 0 {
		return 0
	}
	return len(AllAuthorities) - 1 - idx
}

// Directive reports whether an authority is strong enough for a record to be
// presented to an agent as a rule rather than as background information.
func (a Authority) Directive() bool {
	switch a {
	case AuthorityRepository, AuthorityUser, AuthorityProject:
		return true
	default:
		return false
	}
}

// RecordStatus is the lifecycle state of a record.
//
//	proposed -> active -> superseded -> archived
//	             |           |
//	             v           v
//	          disputed     (restored as a new record)
//	             |
//	             v
//	           stale
type RecordStatus string

const (
	StatusProposed   RecordStatus = "proposed"
	StatusActive     RecordStatus = "active"
	StatusSuperseded RecordStatus = "superseded"
	StatusArchived   RecordStatus = "archived"
	StatusDisputed   RecordStatus = "disputed"
	StatusStale      RecordStatus = "stale"
)

// AllRecordStatuses lists every valid RecordStatus in a stable order.
var AllRecordStatuses = []RecordStatus{
	StatusProposed,
	StatusActive,
	StatusSuperseded,
	StatusArchived,
	StatusDisputed,
	StatusStale,
}

func (s RecordStatus) Valid() bool { return slices.Contains(AllRecordStatuses, s) }

// Guidance reports whether a record in this state may contribute active
// guidance. Superseded and archived records remain retrievable as history but
// never appear as current instruction.
func (s RecordStatus) Guidance() bool { return s == StatusActive }

// ValidationPolicy selects how a record's continued truth is checked. Policies
// that need to touch the filesystem or Git are delegated to a SourceResolver so
// that the domain package stays free of I/O.
type ValidationPolicy string

const (
	ValidateNone        ValidationPolicy = "none"
	ValidateEvidence    ValidationPolicy = "evidence_exists"
	ValidateContentHash ValidationPolicy = "content_hash_matches"
	ValidateGitAncestor ValidationPolicy = "git_revision_ancestor"
	ValidateFileAndHash ValidationPolicy = "file_path_and_hash"
	// ValidateQuotePresent keeps a record fresh while the exact text it was
	// drawn from is still in its source file. It suits a rule extracted from a
	// document, where hashing the whole file would mark every rule stale
	// whenever any one of them was edited.
	ValidateQuotePresent ValidationPolicy = "quote_present"
	ValidateReviewAfter  ValidationPolicy = "review_after"
	ValidateManualReview ValidationPolicy = "manual"
)

// AllValidationPolicies lists every valid ValidationPolicy in a stable order.
var AllValidationPolicies = []ValidationPolicy{
	ValidateNone,
	ValidateEvidence,
	ValidateContentHash,
	ValidateGitAncestor,
	ValidateFileAndHash,
	ValidateQuotePresent,
	ValidateReviewAfter,
	ValidateManualReview,
}

func (p ValidationPolicy) Valid() bool { return slices.Contains(AllValidationPolicies, p) }

// TaskKind narrows which records apply to the work an agent is about to do.
type TaskKind string

const (
	TaskImplementation TaskKind = "implementation"
	TaskCodeReview     TaskKind = "code_review"
	TaskSecurityReview TaskKind = "security_review"
	TaskDesign         TaskKind = "design"
	TaskDebugging      TaskKind = "debugging"
	TaskRelease        TaskKind = "release"
	TaskResearch       TaskKind = "research"
	TaskOther          TaskKind = "other"
)

// AllTaskKinds lists every valid TaskKind in a stable order.
var AllTaskKinds = []TaskKind{
	TaskImplementation,
	TaskCodeReview,
	TaskSecurityReview,
	TaskDesign,
	TaskDebugging,
	TaskRelease,
	TaskResearch,
	TaskOther,
}

func (k TaskKind) Valid() bool { return slices.Contains(AllTaskKinds, k) }

// SourceType identifies what kind of artifact backs a record.
type SourceType string

const (
	SourceConversation SourceType = "conversation"
	SourceADR          SourceType = "adr"
	SourceIssue        SourceType = "issue"
	SourcePullRequest  SourceType = "pull_request"
	SourceCommit       SourceType = "commit"
	SourceFile         SourceType = "file"
	SourceNote         SourceType = "note"
	SourceChatExport   SourceType = "chat_export"
	SourceAgentMemory  SourceType = "agent_memory"
	SourceOther        SourceType = "other"
)

// AllSourceTypes lists every valid SourceType in a stable order.
var AllSourceTypes = []SourceType{
	SourceConversation,
	SourceADR,
	SourceIssue,
	SourcePullRequest,
	SourceCommit,
	SourceFile,
	SourceNote,
	SourceChatExport,
	SourceAgentMemory,
	SourceOther,
}

func (t SourceType) Valid() bool { return slices.Contains(AllSourceTypes, t) }

// Source is a single piece of evidence behind a record. ExactExcerpt is
// untrusted source material: it is preserved verbatim so a reader can compare
// the original wording against the broker's normalized Statement, and it must
// never be treated as an instruction.
type Source struct {
	ID               string           `json:"source_id" yaml:"source_id"`
	Type             SourceType       `json:"type" yaml:"type"`
	Locator          string           `json:"locator" yaml:"locator"`
	Revision         string           `json:"revision,omitempty" yaml:"revision,omitempty"`
	ContentHash      string           `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
	ExactExcerpt     string           `json:"exact_excerpt,omitempty" yaml:"exact_excerpt,omitempty"`
	CapturedAt       time.Time        `json:"captured_at" yaml:"captured_at"`
	ValidationPolicy ValidationPolicy `json:"validation_policy,omitempty" yaml:"validation_policy,omitempty"`
}

// Record is a typed, scoped assertion together with its provenance and
// lifecycle metadata.
type Record struct {
	ID               string           `json:"id" yaml:"id"`
	Kind             RecordKind       `json:"kind" yaml:"kind"`
	Subject          string           `json:"subject" yaml:"subject"`
	Statement        string           `json:"statement" yaml:"statement"`
	Rationale        string           `json:"rationale,omitempty" yaml:"rationale,omitempty"`
	Scope            Scope            `json:"scope" yaml:"scope"`
	Authority        Authority        `json:"authority" yaml:"authority"`
	Status           RecordStatus     `json:"status" yaml:"status"`
	Confidence       float64          `json:"confidence" yaml:"confidence"`
	ValidFrom        time.Time        `json:"valid_from" yaml:"valid_from"`
	ValidUntil       *time.Time       `json:"valid_until,omitempty" yaml:"valid_until,omitempty"`
	ReviewAfter      *time.Time       `json:"review_after,omitempty" yaml:"review_after,omitempty"`
	LastVerifiedAt   *time.Time       `json:"last_verified_at,omitempty" yaml:"last_verified_at,omitempty"`
	ValidationPolicy ValidationPolicy `json:"validation_policy" yaml:"validation_policy"`
	Supersedes       []string         `json:"supersedes,omitempty" yaml:"supersedes,omitempty"`
	SupersededBy     string           `json:"superseded_by,omitempty" yaml:"superseded_by,omitempty"`
	ConflictGroup    string           `json:"conflict_group,omitempty" yaml:"conflict_group,omitempty"`
	Tags             []string         `json:"tags,omitempty" yaml:"tags,omitempty"`
	Sources          []Source         `json:"sources,omitempty" yaml:"sources,omitempty"`
	CreatedAt        time.Time        `json:"created_at" yaml:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at" yaml:"updated_at"`
}

// EffectFor derives how an agent should treat this record at the given time.
// Kind supplies the ceiling, and authority plus lifecycle state can only lower
// it. This is the mechanism that stops an agent inference from being read as an
// instruction.
func (r *Record) EffectFor(now time.Time) Effect {
	if !r.Status.Guidance() || !r.Active(now) {
		return EffectInformational
	}
	if !r.Authority.Directive() {
		return EffectInformational
	}
	switch r.Kind {
	case KindConstraint, KindDecision:
		return EffectConstraint
	case KindPreference:
		return EffectPreference
	default:
		return EffectInformational
	}
}

// Active reports whether now falls inside the record's validity interval. It
// says nothing about lifecycle status or freshness validation.
func (r *Record) Active(now time.Time) bool {
	if !r.ValidFrom.IsZero() && now.Before(r.ValidFrom) {
		return false
	}
	if r.ValidUntil != nil && !now.Before(*r.ValidUntil) {
		return false
	}
	return true
}

// NormalizedSubject collapses a subject to a comparison key used by conflict
// detection and deduplication.
func (r *Record) NormalizedSubject() string {
	return strings.Join(strings.Fields(strings.ToLower(r.Subject)), " ")
}

// SearchText is the text indexed for full-text retrieval.
func (r *Record) SearchText() string {
	parts := []string{r.Subject, r.Statement, r.Rationale, strings.Join(r.Tags, " ")}
	for _, src := range r.Sources {
		parts = append(parts, src.ExactExcerpt)
	}
	return strings.Join(parts, "\n")
}

// Clone returns a deep copy so that callers cannot mutate stored state through
// a returned pointer.
func (r *Record) Clone() *Record {
	if r == nil {
		return nil
	}
	out := *r
	out.Scope = r.Scope.Clone()
	out.Supersedes = slices.Clone(r.Supersedes)
	out.Tags = slices.Clone(r.Tags)
	out.Sources = slices.Clone(r.Sources)
	if r.ValidUntil != nil {
		v := *r.ValidUntil
		out.ValidUntil = &v
	}
	if r.ReviewAfter != nil {
		v := *r.ReviewAfter
		out.ReviewAfter = &v
	}
	if r.LastVerifiedAt != nil {
		v := *r.LastVerifiedAt
		out.LastVerifiedAt = &v
	}
	return &out
}

// Normalize fills in defaults and trims text so that equivalent input produces
// an identical stored record.
func (r *Record) Normalize(now time.Time) {
	r.Subject = strings.TrimSpace(r.Subject)
	r.Statement = strings.TrimSpace(r.Statement)
	r.Rationale = strings.TrimSpace(r.Rationale)
	if r.Status == "" {
		r.Status = StatusActive
	}
	if r.ValidationPolicy == "" {
		r.ValidationPolicy = ValidateNone
	}
	if r.Confidence == 0 {
		r.Confidence = 1.0
	}
	if r.ValidFrom.IsZero() {
		r.ValidFrom = now
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	r.ValidFrom = r.ValidFrom.UTC()
	r.CreatedAt = r.CreatedAt.UTC()
	r.UpdatedAt = r.UpdatedAt.UTC()

	slices.Sort(r.Tags)
	r.Tags = slices.Compact(r.Tags)
	slices.Sort(r.Supersedes)
	r.Supersedes = slices.Compact(r.Supersedes)

	for i := range r.Sources {
		src := &r.Sources[i]
		if src.ID == "" {
			src.ID = NewID("src")
		}
		if src.Type == "" {
			src.Type = SourceOther
		}
		if src.CapturedAt.IsZero() {
			src.CapturedAt = now
		}
		src.CapturedAt = src.CapturedAt.UTC()
	}
	r.Scope.Normalize()
}

// Validate reports the first structural problem with a record.
func (r *Record) Validate() error {
	if r.ID == "" {
		return fmt.Errorf(`record ID is required`)
	}
	if !r.Kind.Valid() {
		return fmt.Errorf(`invalid record kind %q`, r.Kind)
	}
	if r.Statement == "" {
		return fmt.Errorf(`record %s: statement is required`, r.ID)
	}
	if r.Subject == "" {
		return fmt.Errorf(`record %s: subject is required`, r.ID)
	}
	if !r.Authority.Valid() {
		return fmt.Errorf(`record %s: invalid authority %q`, r.ID, r.Authority)
	}
	if !r.Status.Valid() {
		return fmt.Errorf(`record %s: invalid status %q`, r.ID, r.Status)
	}
	if !r.ValidationPolicy.Valid() {
		return fmt.Errorf(`record %s: invalid validation policy %q`, r.ID, r.ValidationPolicy)
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return fmt.Errorf(`record %s: confidence must be within [0,1], got %v`, r.ID, r.Confidence)
	}
	if r.ValidUntil != nil && r.ValidUntil.Before(r.ValidFrom) {
		return fmt.Errorf(`record %s: valid_until precedes valid_from`, r.ID)
	}
	for i, src := range r.Sources {
		if !src.Type.Valid() {
			return fmt.Errorf(`record %s: source %d has invalid type %q`, r.ID, i, src.Type)
		}
		if src.Locator == "" {
			return fmt.Errorf(`record %s: source %d requires a locator`, r.ID, i)
		}
	}
	return r.Scope.Validate()
}

var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// NewID returns a lexicographically time-ordered identifier with the given
// prefix. Ordering by ID therefore approximates ordering by creation time,
// which keeps CLI listings and test fixtures readable.
func NewID(prefix string) string {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(time.Now().UTC().UnixMilli()))
	if _, err := rand.Read(buf[8:]); err != nil {
		// crypto/rand.Read on a modern kernel does not fail; a broken entropy
		// source is not something this service can paper over.
		panic(fmt.Sprintf("mecp: cannot read random bytes: %s", err))
	}
	return prefix + "_" + idEncoding.EncodeToString(buf[:])
}
