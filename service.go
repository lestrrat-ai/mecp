package mecp

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/option/v3"
)

// Service is the transport-independent core. The same instance backs the MCP
// gateway, the administrative CLI, and tests.
//
// All methods are safe for concurrent use: the receiver holds validated
// configuration only, and per-call state lives in parameters and locals.
type Service interface {
	// PrepareTask builds a bounded context pack for a concrete coding task.
	PrepareTask(ctx context.Context, req PrepareTaskRequest) (*ContextPack, error)
	// Search answers a targeted follow-up question inside an authorized scope.
	Search(ctx context.Context, req SearchRequest) (*SearchResult, error)
	// GetRecords returns full records and bounded evidence for known IDs.
	GetRecords(ctx context.Context, req GetRecordsRequest) (*RecordResult, error)
	// ProposeRecord files an inactive proposal for user review.
	ProposeRecord(ctx context.Context, req ProposeRecordRequest) (*ProposalResult, error)
	// ExtractRules turns rules read out of an instruction document into
	// pending proposals, checking each quote against the document itself.
	ExtractRules(ctx context.Context, req ExtractRulesRequest) (*ExtractRulesResult, error)
}

// ScopeFilter narrows a context pack by how widely its records apply.
//
// It exists because a caller that injects context on every turn would otherwise
// resend the same universal rules forever, and each copy stays in the
// conversation. Delivering those once and then only what is specific to the
// work keeps the repetition out.
type ScopeFilter string

const (
	// ScopeFilterAll returns every applicable record.
	ScopeFilterAll ScopeFilter = ""
	// ScopeFilterGlobalOnly returns only records that apply everywhere, which
	// is what a caller wants once at the start of a session.
	ScopeFilterGlobalOnly ScopeFilter = "global_only"
	// ScopeFilterScopedOnly omits those, for a caller that already has them.
	ScopeFilterScopedOnly ScopeFilter = "scoped_only"
)

// PrepareTaskRequest asks for the context that matters for one task.
type PrepareTaskRequest struct {
	Caller                   Caller
	Task                     string
	TaskKind                 TaskKind
	Workspace                Workspace
	Conditions               map[string]string
	TokenBudget              int
	IncludeEvidenceSummaries bool
	// ScopeFilter narrows the pack to records of a given breadth.
	ScopeFilter ScopeFilter
}

// ContextPack is the bounded result of preparing context for a task.
type ContextPack struct {
	ContextID   string         `json:"context_id"`
	GeneratedAt time.Time      `json:"generated_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
	Scope       EffectiveScope `json:"scope"`
	Summary     string         `json:"summary"`
	Items       []ContextItem  `json:"items"`
	Conflicts   []Conflict     `json:"conflicts"`
	Warnings    []Warning      `json:"warnings"`
	Budget      BudgetReport   `json:"budget"`
}

// SearchRequest is a targeted follow-up query. Either ContextID or Workspace
// must supply the scope; a request that supplies neither is rejected rather
// than answered from an unbounded scope.
type SearchRequest struct {
	Caller       Caller
	ContextID    string
	Query        string
	Workspace    Workspace
	TaskKind     TaskKind
	Conditions   map[string]string
	Kinds        []RecordKind
	IncludeStale bool
	Limit        int
}

// SearchItem is one ranked search hit.
type SearchItem struct {
	RecordID         string          `json:"record_id"`
	Kind             RecordKind      `json:"kind"`
	Effect           Effect          `json:"effect"`
	Subject          string          `json:"subject"`
	Statement        string          `json:"statement"`
	Authority        Authority       `json:"authority"`
	Status           RecordStatus    `json:"status"`
	ScopeSpecificity string          `json:"scope_specificity"`
	Validation       ValidationState `json:"validation"`
	LastVerifiedAt   *time.Time      `json:"last_verified_at,omitempty"`
	SourceRefs       []string        `json:"source_refs"`
	MatchReasons     []string        `json:"match_reasons,omitempty"`
}

// SearchResult holds ranked matches for a follow-up query.
type SearchResult struct {
	ContextID string         `json:"context_id,omitempty"`
	Scope     EffectiveScope `json:"scope"`
	Items     []SearchItem   `json:"items"`
	Warnings  []Warning      `json:"warnings"`
}

// GetRecordsRequest fetches full detail for records already discovered.
type GetRecordsRequest struct {
	Caller                         Caller
	RecordIDs                      []string
	IncludeEvidence                bool
	MaxEvidenceCharactersPerRecord int
}

// SourceView is a source as disclosed to a caller. Excerpt carries untrusted
// source text and is empty when the caller lacks the evidence capability;
// Redacted then says so explicitly rather than letting the absence look like
// "no evidence exists".
type SourceView struct {
	SourceID    string     `json:"source_id"`
	Type        SourceType `json:"type"`
	Locator     string     `json:"locator"`
	Revision    string     `json:"revision,omitempty"`
	ContentHash string     `json:"content_hash,omitempty"`
	CapturedAt  time.Time  `json:"captured_at"`
	Excerpt     string     `json:"exact_excerpt,omitempty"`
	Truncated   bool       `json:"excerpt_truncated,omitempty"`
	Redacted    bool       `json:"redacted,omitempty"`
}

// RecordDetail is the full disclosure of one record.
type RecordDetail struct {
	RecordID         string           `json:"record_id"`
	Kind             RecordKind       `json:"kind"`
	Effect           Effect           `json:"effect"`
	Subject          string           `json:"subject"`
	Statement        string           `json:"statement"`
	Rationale        string           `json:"rationale,omitempty"`
	Authority        Authority        `json:"authority"`
	Status           RecordStatus     `json:"status"`
	Scope            Scope            `json:"scope"`
	Tags             []string         `json:"tags,omitempty"`
	Confidence       float64          `json:"confidence"`
	ValidFrom        time.Time        `json:"valid_from"`
	ValidUntil       *time.Time       `json:"valid_until,omitempty"`
	ReviewAfter      *time.Time       `json:"review_after,omitempty"`
	LastVerifiedAt   *time.Time       `json:"last_verified_at,omitempty"`
	ValidationPolicy ValidationPolicy `json:"validation_policy"`
	Validation       ValidationStatus `json:"validation"`
	Supersedes       []string         `json:"supersedes,omitempty"`
	SupersededBy     []string         `json:"superseded_by,omitempty"`
	Sources          []SourceView     `json:"sources,omitempty"`
}

// RecordResult is the response to GetRecords.
type RecordResult struct {
	Records  []RecordDetail `json:"records"`
	Warnings []Warning      `json:"warnings"`
}

// ProposeRecordRequest suggests a durable record for user review.
type ProposeRecordRequest struct {
	Caller              Caller
	ProposalKey         string
	Kind                RecordKind
	Subject             string
	Statement           string
	Rationale           string
	Scope               Scope
	Tags                []string
	Evidence            []Source
	SupersedesRecordIDs []string
}

// ProposalResult is the response to ProposeRecord.
type ProposalResult struct {
	ProposalID string         `json:"proposal_id"`
	Status     ProposalStatus `json:"status"`
	Created    bool           `json:"created"`
}

const (
	defaultContextTTL    = time.Hour
	defaultValidationTTL = 15 * time.Minute
	defaultMaxCandidates = 500
	defaultSearchLimit   = 8
	maxSearchLimit       = 50
	// defaultEvidenceCharacters bounds excerpt disclosure when the caller does
	// not ask for a specific limit.
	defaultEvidenceCharacters = 2000
	maxEvidenceCharacters     = 20000
	maxRecordIDsPerCall       = 64
)

type service struct {
	store             Store
	clock             Clock
	ranker            Ranker
	packer            Packer
	validator         Validator
	audit             AuditSink
	documents         DocumentReader
	documentAuthority Authority
	contextTTL        time.Duration
	maxCandidates     int
	aliases           map[string]string

	mu      sync.Mutex
	handles map[string]*contextHandle
}

type contextHandle struct {
	ID          string
	PrincipalID string
	ClientID    string
	Task        string
	TaskKind    TaskKind
	Workspace   Workspace
	Conditions  map[string]string
	Scope       EffectiveScope
	ExpiresAt   time.Time
}

// New builds a Service over a store.
func New(store Store, options ...ServiceOption) (Service, error) {
	if store == nil {
		return nil, errorf(CodeStorage, "a store is required")
	}

	svc := &service{
		store:         store,
		clock:         SystemClock{},
		packer:        NewPacker(),
		audit:         NopAudit{},
		contextTTL:    defaultContextTTL,
		maxCandidates: defaultMaxCandidates,
		handles:       make(map[string]*contextHandle),
	}

	var (
		ranker        Ranker
		validator     Validator
		resolver      SourceResolver
		validationTTL = defaultValidationTTL
		aliases       map[string]string
	)

	for _, opt := range options {
		switch opt.Ident().(type) {
		case identClock:
			svc.clock = option.MustGet[Clock](opt)
		case identRanker:
			ranker = option.MustGet[Ranker](opt)
		case identPacker:
			svc.packer = option.MustGet[Packer](opt)
		case identValidator:
			validator = option.MustGet[Validator](opt)
		case identSourceResolver:
			resolver = option.MustGet[SourceResolver](opt)
		case identValidationTTL:
			validationTTL = option.MustGet[time.Duration](opt)
		case identAuditSink:
			svc.audit = option.MustGet[AuditSink](opt)
		case identDocumentReader:
			svc.documents = option.MustGet[DocumentReader](opt)
		case identDocumentAuthority:
			svc.documentAuthority = option.MustGet[Authority](opt)
		case identContextTTL:
			svc.contextTTL = option.MustGet[time.Duration](opt)
		case identMaxCandidates:
			svc.maxCandidates = option.MustGet[int](opt)
		case identRepositoryAliases:
			aliases = option.MustGet[map[string]string](opt)
		}
	}

	if ranker == nil {
		ranker = NewRanker(DefaultRankWeights())
	}
	svc.ranker = ranker

	if validator == nil {
		validator = NewValidator(resolver)
	}
	svc.validator = NewCachingValidator(validator, validationTTL)

	if len(aliases) > 0 {
		svc.aliases = make(map[string]string, len(aliases))
		for from, to := range aliases {
			svc.aliases[CanonicalRepository(from)] = CanonicalRepository(to)
		}
	}

	if svc.documentAuthority == "" {
		// Document roots are named deliberately in configuration, so what they
		// hold is the user's own writing rather than something found.
		svc.documentAuthority = AuthorityUser
	}
	if svc.maxCandidates <= 0 {
		svc.maxCandidates = defaultMaxCandidates
	}
	return svc, nil
}

// canonicalRepository applies configured aliases on top of syntactic
// canonicalization, so a fork's remote never silently resolves to the upstream
// unless the user linked them.
func (s *service) canonicalRepository(in string) string {
	canon := CanonicalRepository(in)
	if canon == "" {
		return ""
	}
	if target, ok := s.aliases[canon]; ok && target != "" {
		return target
	}
	return canon
}

// resolveScope authorizes a request and produces the scope the service will
// answer about. It fails closed: an unauthorized repository is an error, not a
// silently narrowed result.
func (s *service) resolveScope(caller Caller, ws Workspace, taskKind TaskKind) (EffectiveScope, []Warning, error) {
	if err := caller.Validate(); err != nil {
		return EffectiveScope{}, nil, err
	}

	repo := s.canonicalRepository(ws.Repository)
	if ws.Repository != "" && repo == "" {
		return EffectiveScope{}, nil, errorf(CodeAmbiguousRepository,
			"repository %q could not be canonicalized; supply a canonical URL", ws.Repository)
	}
	if repo != "" && !caller.RepositoryAllowed(repo) {
		return EffectiveScope{}, nil, errorf(CodeUnauthorizedScope,
			"client profile %q may not query repository %s", caller.ClientID, repo)
	}
	if err := s.checkRoot(caller, ws.RootURI); err != nil {
		return EffectiveScope{}, nil, err
	}

	scope := EffectiveScope{
		Principal:  caller.PrincipalID,
		Repository: repo,
		Org:        RepositoryOrg(repo),
		Branch:     strings.TrimSpace(ws.Branch),
		Revision:   strings.TrimSpace(ws.Revision),
		Paths:      slices.Clone(ws.RelevantPaths),
		TaskKind:   taskKind,
	}

	var warnings []Warning
	if repo == "" {
		warnings = append(warnings, Warning{
			Code:    WarnNoWorkspace,
			Message: "no repository was supplied, so repository-scoped records were not considered",
		})
	}
	return scope, warnings, nil
}

// checkRoot refuses a workspace outside the roots the profile allows. Without
// it, a client could name any path on the machine as its workspace.
func (s *service) checkRoot(caller Caller, rootURI string) error {
	if len(caller.AllowedRoots) == 0 || rootURI == "" {
		return nil
	}
	root := strings.TrimPrefix(rootURI, "file://")
	for _, allowed := range caller.AllowedRoots {
		allowed = strings.TrimPrefix(allowed, "file://")
		if root == allowed || strings.HasPrefix(root, strings.TrimSuffix(allowed, "/")+"/") {
			return nil
		}
	}
	return errorf(CodeUnauthorizedScope, "workspace root %q is outside the roots allowed for client %q", rootURI, caller.ClientID)
}

// collectRequest describes one retrieval pass.
type collectRequest struct {
	Caller       Caller
	Text         string
	Workspace    Workspace
	Repository   string
	TaskKind     TaskKind
	Conditions   map[string]string
	Kinds        []RecordKind
	IncludeStale bool
	ScopeFilter  ScopeFilter
	// IncludeMandatory pulls in directly-scoped directive records even when
	// they do not match the query text. Task preparation needs this; a targeted
	// follow-up search does not.
	IncludeMandatory bool
}

// collect runs the authorization, retrieval, validation, and ranking pipeline
// shared by PrepareTask and Search.
func (s *service) collect(ctx context.Context, req collectRequest) ([]*Candidate, []Warning, error) {
	now := s.clock.Now()

	statuses := []RecordStatus{StatusActive, StatusDisputed, StatusStale}
	if req.IncludeStale {
		statuses = append(statuses, StatusSuperseded, StatusArchived)
	}

	base := RecordQuery{
		PrincipalID:          req.Caller.PrincipalID,
		Kinds:                req.Kinds,
		Statuses:             statuses,
		At:                   now,
		Limit:                s.maxCandidates,
		RestrictRepositories: true,
		AllowGlobal:          true,
	}
	if req.Repository != "" {
		base.Repositories = []string{req.Repository}
	}

	byID := make(map[string]*Candidate)

	if req.IncludeMandatory {
		mandatory := base
		mandatory.Kinds = intersectKinds(req.Kinds, []RecordKind{KindConstraint, KindDecision, KindPreference, KindProjectFact, KindOpenQuestion})
		recs, err := s.store.QueryRecords(ctx, mandatory)
		if err != nil {
			return nil, nil, wrapf(CodeStorage, err, "cannot load scoped records")
		}
		for _, rec := range recs {
			addCandidate(byID, rec, 0, nil)
		}
	}

	if strings.TrimSpace(req.Text) != "" {
		hits, err := s.store.SearchRecords(ctx, SearchQuery{RecordQuery: base, Text: req.Text})
		if err != nil {
			return nil, nil, wrapf(CodeStorage, err, "cannot search records")
		}
		for _, hit := range hits {
			addCandidate(byID, hit.Record, hit.Relevance, hit.Terms)
		}
	}

	scopeReq := ScopeRequest{
		Principal:  req.Caller.PrincipalID,
		Workspace:  req.Workspace,
		TaskKind:   req.TaskKind,
		Conditions: req.Conditions,
	}

	applicable := make([]*Candidate, 0, len(byID))
	for _, c := range byID {
		match := c.Record.Scope.Match(scopeReq)
		if !match.Matched {
			continue
		}
		// Breadth is a property of the record's scope, not of the match. Every
		// record names a principal, which scores on the user dimension, so
		// specificity is never zero and cannot stand in for "applies
		// everywhere".
		switch req.ScopeFilter {
		case ScopeFilterGlobalOnly:
			if !c.Record.Scope.Global() {
				continue
			}
		case ScopeFilterScopedOnly:
			if c.Record.Scope.Global() {
				continue
			}
		}
		c.Scope = match
		applicable = append(applicable, c)
	}

	warnings, err := s.annotate(ctx, applicable, req, now)
	if err != nil {
		return nil, nil, err
	}

	unknown, err := s.unknownRepositoryWarning(ctx, req.Repository)
	if err != nil {
		return nil, nil, err
	}
	if unknown != nil {
		warnings = append(warnings, *unknown)
	}

	s.ranker.Rank(RankRequest{
		Query:     req.Text,
		TaskKind:  req.TaskKind,
		Workspace: req.Workspace,
		Now:       now,
	}, applicable)

	return applicable, warnings, nil
}

// unknownRepositoryWarning reports a repository the store has never seen. An
// empty result is otherwise indistinguishable from having nothing stored, which
// hides the most likely cause: the caller named the repository differently from
// however the records were written.
//
// Nothing is reported when no record is scoped to any repository, because then
// the store genuinely has nothing to say and the caller is not being misled.
func (s *service) unknownRepositoryWarning(ctx context.Context, repository string) (*Warning, error) {
	if repository == "" {
		return nil, nil
	}
	known, err := s.store.KnownRepositories(ctx)
	if err != nil {
		return nil, wrapf(CodeStorage, err, "cannot list known repositories")
	}
	if len(known) == 0 || slices.Contains(known, repository) {
		return nil, nil
	}
	return &Warning{
		Code: WarnUnknownRepository,
		Message: fmt.Sprintf(
			"no record is scoped to %s, though %d other repositor%s stored; check that the repository is named the way the records were written",
			repository, len(known), pluralVerb(len(known))),
	}, nil
}

func pluralVerb(n int) string {
	if n == 1 {
		return "y is"
	}
	return "ies are"
}

// annotate fills in supersession, freshness, and effect for every candidate,
// and reports what the caller should be suspicious of.
func (s *service) annotate(ctx context.Context, cands []*Candidate, req collectRequest, now time.Time) ([]Warning, error) {
	if len(cands) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Record.ID)
	}
	superseded, err := s.store.SupersededBy(ctx, ids)
	if err != nil {
		return nil, wrapf(CodeStorage, err, "cannot resolve supersession")
	}

	var (
		staleIDs      []string
		supersededIDs []string
		mismatchIDs   []string
		disputedIDs   []string
	)

	for _, c := range cands {
		rec := c.Record
		c.Superseded = rec.SupersededBy != "" || len(superseded[rec.ID]) > 0
		c.Validation = s.validator.Validate(ctx, rec, req.Workspace, now)

		effect := rec.EffectFor(now)
		if !c.Validation.State.Trusted() {
			effect = EffectInformational
		}
		if c.Superseded {
			effect = EffectInformational
		}
		c.Effect = effect

		switch c.Validation.State {
		case ValidationStale:
			staleIDs = append(staleIDs, rec.ID)
		case ValidationFailed:
			staleIDs = append(staleIDs, rec.ID)
		}
		if c.Superseded {
			supersededIDs = append(supersededIDs, rec.ID)
		}
		if rec.Status == StatusDisputed {
			disputedIDs = append(disputedIDs, rec.ID)
		}
		if revisionMismatch(rec, req.Workspace) {
			mismatchIDs = append(mismatchIDs, rec.ID)
		}
	}

	var warnings []Warning
	if len(staleIDs) > 0 {
		slices.Sort(staleIDs)
		warnings = append(warnings, Warning{
			Code:      WarnStaleRecord,
			Message:   "some records failed freshness validation and are reported as informational only",
			RecordIDs: slices.Compact(staleIDs),
		})
	}
	if len(supersededIDs) > 0 {
		slices.Sort(supersededIDs)
		warnings = append(warnings, Warning{
			Code:      WarnSupersededRecord,
			Message:   "some records have been superseded and are retained as history",
			RecordIDs: supersededIDs,
		})
	}
	if len(disputedIDs) > 0 {
		slices.Sort(disputedIDs)
		warnings = append(warnings, Warning{
			Code:      WarnDisputedRecord,
			Message:   "credible sources disagree about some records and no resolution has been recorded",
			RecordIDs: disputedIDs,
		})
	}
	if len(mismatchIDs) > 0 {
		slices.Sort(mismatchIDs)
		warnings = append(warnings, Warning{
			Code:      WarnRevisionMismatch,
			Message:   "some records describe a different revision than the one supplied",
			RecordIDs: mismatchIDs,
		})
	}
	return warnings, nil
}

// revisionMismatch reports a record whose every revision-bearing source
// describes some other revision. A record with no revision-bearing source
// makes no claim about revisions and is not flagged.
func revisionMismatch(rec *Record, ws Workspace) bool {
	if ws.Revision == "" {
		return false
	}
	var withRevision int
	for _, src := range rec.Sources {
		if src.Revision == "" {
			continue
		}
		withRevision++
		if strings.HasPrefix(ws.Revision, src.Revision) || strings.HasPrefix(src.Revision, ws.Revision) {
			return false
		}
	}
	return withRevision > 0
}

func addCandidate(byID map[string]*Candidate, rec *Record, relevance float64, terms []string) {
	existing, ok := byID[rec.ID]
	if !ok {
		c := &Candidate{Record: rec, Relevance: relevance}
		if len(terms) > 0 {
			c.MatchReasons = append(c.MatchReasons, "matched terms: "+strings.Join(terms, ", "))
		}
		byID[rec.ID] = c
		return
	}
	if relevance > existing.Relevance {
		existing.Relevance = relevance
	}
	if len(terms) > 0 {
		existing.MatchReasons = append(existing.MatchReasons, "matched terms: "+strings.Join(terms, ", "))
	}
}

func intersectKinds(requested, allowed []RecordKind) []RecordKind {
	if len(requested) == 0 {
		return allowed
	}
	out := make([]RecordKind, 0, len(requested))
	for _, k := range requested {
		if slices.Contains(allowed, k) {
			out = append(out, k)
		}
	}
	return out
}

func (s *service) putHandle(h *contextHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	for id, existing := range s.handles {
		if now.After(existing.ExpiresAt) {
			delete(s.handles, id)
		}
	}
	s.handles[h.ID] = h
}

// takeHandle resolves a context handle. The handle is an optimization, not a
// credential: the caller identity is rechecked here, and every downstream
// authorization runs again regardless.
func (s *service) takeHandle(caller Caller, id string) (*contextHandle, error) {
	s.mu.Lock()
	h, ok := s.handles[id]
	s.mu.Unlock()

	if !ok {
		return nil, errorf(CodeContextExpired, "context handle %q is unknown or has expired", id)
	}
	if s.clock.Now().After(h.ExpiresAt) {
		return nil, errorf(CodeContextExpired, "context handle %q has expired", id)
	}
	if h.PrincipalID != caller.PrincipalID || h.ClientID != caller.ClientID {
		// Report the same failure as an unknown handle so that a replayed
		// handle from another client cannot be distinguished from a typo.
		return nil, errorf(CodeContextExpired, "context handle %q is unknown or has expired", id)
	}
	return h, nil
}

// writeAudit stamps the parts of an event that every call site would otherwise
// have to repeat, and records it. The caller identity is copied here rather
// than at each site so that a new operation cannot ship an event that is
// missing who asked or which interface they asked through.
func (s *service) writeAudit(ctx context.Context, caller Caller, ev AuditEvent, start time.Time) {
	ev.At = s.clock.Now()
	ev.LatencyMS = time.Since(start).Milliseconds()
	ev.PrincipalID = caller.PrincipalID
	ev.ClientID = caller.ClientID
	ev.Origin = caller.Origin
	ev.SessionID = caller.SessionID
	// An audit failure must not fail the user's call; it is recorded locally
	// and best-effort by design.
	_ = s.audit.Write(ctx, ev)
}
