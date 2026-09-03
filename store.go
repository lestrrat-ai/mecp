package mecp

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by a Store when an addressed row does not exist.
var ErrNotFound = errors.New("mecp: not found")

// RecordQuery is a structured pre-filter. Every field here is applied by the
// store itself, before any full-text matching, so that an unauthorized row
// never reaches ranking, snippet generation, or even a result count.
//
// PrincipalID is the security-critical field: a store implementation must
// apply it unconditionally.
type RecordQuery struct {
	PrincipalID string

	// RestrictRepositories turns on repository filtering. When it is false the
	// query is not filtered by repository at all, which only the administrative
	// interface should do.
	//
	// When it is true, a record is admitted if its scope names one of
	// Repositories, or if it names no repository and AllowGlobal is set. An
	// empty Repositories list with RestrictRepositories set therefore admits
	// global records only, which is what a request carrying no workspace wants.
	RestrictRepositories bool
	Repositories         []string
	AllowGlobal          bool

	Kinds    []RecordKind
	Statuses []RecordStatus
	Tags     []string
	IDs      []string
	Subject  string

	// At bounds the validity interval. A zero time disables the check, which
	// the administrative CLI uses to list historical records.
	At time.Time

	Limit  int
	Offset int
}

// SearchQuery adds a full-text expression to the structured pre-filter.
type SearchQuery struct {
	RecordQuery

	// Text is a user or agent supplied natural-language query. The store is
	// responsible for turning it into a safe FTS expression; callers must not
	// pass FTS syntax through.
	Text string
}

// ScoredRecord pairs a record with the store's lexical relevance for a query.
// Relevance is normalized to [0,1], where 1 is the best match in the result
// set. It is a retrieval signal only and never establishes authority.
type ScoredRecord struct {
	Record    *Record
	Relevance float64
	Terms     []string
}

// ProposalStatus is the review state of an agent-submitted proposal.
type ProposalStatus string

const (
	ProposalPending  ProposalStatus = "pending_review"
	ProposalApproved ProposalStatus = "approved"
	ProposalRejected ProposalStatus = "rejected"
)

// AllProposalStatuses lists every valid ProposalStatus in a stable order.
var AllProposalStatuses = []ProposalStatus{ProposalPending, ProposalApproved, ProposalRejected}

// Proposal is an inactive suggestion awaiting user review. A proposal never
// participates in retrieval and cannot override an existing record.
type Proposal struct {
	ID                  string         `json:"proposal_id" yaml:"proposal_id"`
	Key                 string         `json:"proposal_key" yaml:"proposal_key"`
	Status              ProposalStatus `json:"status" yaml:"status"`
	PrincipalID         string         `json:"principal_id" yaml:"principal_id"`
	ClientID            string         `json:"client_id" yaml:"client_id"`
	Kind                RecordKind     `json:"kind" yaml:"kind"`
	Subject             string         `json:"subject" yaml:"subject"`
	Statement           string         `json:"statement" yaml:"statement"`
	Rationale           string         `json:"rationale,omitempty" yaml:"rationale,omitempty"`
	Scope               Scope          `json:"scope" yaml:"scope"`
	Tags                []string       `json:"tags,omitempty" yaml:"tags,omitempty"`
	Evidence            []Source       `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	SupersedesRecordIDs []string       `json:"supersedes_record_ids,omitempty" yaml:"supersedes_record_ids,omitempty"`
	CreatedAt           time.Time      `json:"created_at" yaml:"created_at"`
	DecidedAt           *time.Time     `json:"decided_at,omitempty" yaml:"decided_at,omitempty"`
	DecidedBy           string         `json:"decided_by,omitempty" yaml:"decided_by,omitempty"`
	DecisionNote        string         `json:"decision_note,omitempty" yaml:"decision_note,omitempty"`
	ResultRecordID      string         `json:"result_record_id,omitempty" yaml:"result_record_id,omitempty"`
}

// ProposalQuery filters a proposal listing.
type ProposalQuery struct {
	PrincipalID string
	Statuses    []ProposalStatus
	Limit       int
	Offset      int
}

// Store is the persistence boundary of the service. Implementations must be
// safe for concurrent use.
type Store interface {
	// Migrate brings the schema up to the version this build expects.
	Migrate(ctx context.Context) error

	// PutRecord inserts or replaces a record together with its scope, sources,
	// relationships, and search index entries, in one transaction.
	PutRecord(ctx context.Context, rec *Record) error

	// GetRecord returns one record regardless of scope. Authorization is the
	// caller's responsibility; the administrative CLI relies on this.
	GetRecord(ctx context.Context, id string) (*Record, error)

	// DeleteRecord removes a record and every index, relationship, and source
	// row that referenced it.
	DeleteRecord(ctx context.Context, id string) error

	// QueryRecords returns records matching a structured filter, ordered by ID
	// so that repeated calls are stable.
	QueryRecords(ctx context.Context, q RecordQuery) ([]*Record, error)

	// SearchRecords applies the same structured filter and then ranks the
	// survivors lexically.
	SearchRecords(ctx context.Context, q SearchQuery) ([]ScoredRecord, error)

	// SupersededBy returns the IDs of records that supersede the given record.
	SupersededBy(ctx context.Context, ids []string) (map[string][]string, error)

	// KnownRepositories returns every canonical repository that at least one
	// record is scoped to, sorted. It exists so the service can tell the
	// difference between "nothing is stored for this repository" and "you named
	// a repository this store has never heard of".
	KnownRepositories(ctx context.Context) ([]string, error)

	// PutProposal stores a proposal, returning the existing one when its key
	// has already been used. The boolean reports whether a new row was created.
	PutProposal(ctx context.Context, p *Proposal) (*Proposal, bool, error)

	// GetProposal returns one proposal by ID.
	GetProposal(ctx context.Context, id string) (*Proposal, error)

	// UpdateProposal writes back a reviewed proposal.
	UpdateProposal(ctx context.Context, p *Proposal) error

	// QueryProposals lists proposals matching a filter, newest first.
	QueryProposals(ctx context.Context, q ProposalQuery) ([]*Proposal, error)

	// ContentVersion returns a token that changes whenever stored records
	// change. It participates in cache keys so a cached context pack cannot
	// outlive an edit.
	ContentVersion(ctx context.Context) (string, error)

	Close() error
}
