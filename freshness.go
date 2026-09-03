package mecp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ValidationState is the outcome of checking whether a record is still true.
type ValidationState string

const (
	// ValidationValid means the record's freshness policy was checked and passed.
	ValidationValid ValidationState = "valid"
	// ValidationUnverified means the check could not run, usually because no
	// source resolver is configured or the workspace lacks a revision.
	ValidationUnverified ValidationState = "unverified"
	// ValidationStale means the record passed its review date or its evidence
	// has moved on. A stale record is demoted out of active guidance.
	ValidationStale ValidationState = "stale"
	// ValidationFailed means the supporting evidence could not be found at all.
	ValidationFailed ValidationState = "failed"
)

// Trusted reports whether a state permits the record to act as current guidance.
func (s ValidationState) Trusted() bool {
	return s == ValidationValid || s == ValidationUnverified
}

// ValidationStatus records what was checked, what happened, and when.
type ValidationStatus struct {
	Policy    ValidationPolicy `json:"policy"`
	State     ValidationState  `json:"state"`
	Reason    string           `json:"reason,omitempty"`
	CheckedAt time.Time        `json:"checked_at"`
}

// SourceResolver performs the I/O-bound half of freshness validation. Keeping
// it behind an interface lets the domain package stay free of filesystem and
// Git access, and lets tests validate policy logic without a repository.
type SourceResolver interface {
	// Exists reports whether the artifact a source points at is still present.
	Exists(ctx context.Context, src Source, ws Workspace) (bool, error)
	// ContentHash returns the current hash of the artifact, in the same
	// "sha256:<hex>" form stored on the source.
	ContentHash(ctx context.Context, src Source, ws Workspace) (string, error)
	// RevisionApplies reports whether the source's revision is an ancestor of,
	// or equal to, the workspace revision.
	RevisionApplies(ctx context.Context, src Source, ws Workspace) (bool, error)
	// Contains reports whether a source's file still holds the given text.
	Contains(ctx context.Context, src Source, ws Workspace, text string) (bool, error)
}

// Validator decides whether a record may still be presented as current.
type Validator interface {
	Validate(ctx context.Context, rec *Record, ws Workspace, now time.Time) ValidationStatus
}

// NewValidator returns the default policy engine. A nil resolver is allowed:
// policies that need I/O then report ValidationUnverified rather than failing,
// which keeps the service usable with no Git access.
func NewValidator(resolver SourceResolver) Validator {
	return &policyValidator{resolver: resolver}
}

type policyValidator struct {
	resolver SourceResolver
}

func (v *policyValidator) Validate(ctx context.Context, rec *Record, ws Workspace, now time.Time) ValidationStatus {
	st := ValidationStatus{Policy: rec.ValidationPolicy, State: ValidationValid, CheckedAt: now}
	if st.Policy == "" {
		st.Policy = ValidateNone
	}

	switch st.Policy {
	case ValidateNone:
	case ValidateReviewAfter:
		// handled by the overlay below
	case ValidateManualReview:
		if rec.LastVerifiedAt == nil {
			st.State = ValidationUnverified
			st.Reason = "record requires manual review and has never been verified"
		} else if rec.LastVerifiedAt.Before(rec.UpdatedAt) {
			st.State = ValidationStale
			st.Reason = "record changed after its last manual verification"
		}
	case ValidateEvidence:
		st = v.checkEvidence(ctx, rec, ws, st)
	case ValidateContentHash:
		st = v.checkHashes(ctx, rec, ws, st)
	case ValidateFileAndHash:
		st = v.checkEvidence(ctx, rec, ws, st)
		if st.State.Trusted() {
			st = v.checkHashes(ctx, rec, ws, st)
		}
	case ValidateQuotePresent:
		st = v.checkQuotes(ctx, rec, ws, st)
	case ValidateGitAncestor:
		st = v.checkRevision(ctx, rec, ws, st)
	default:
		st.State = ValidationUnverified
		st.Reason = fmt.Sprintf("unknown validation policy %q", st.Policy)
	}

	return applyReviewOverlay(rec, now, st)
}

// applyReviewOverlay demotes any record whose review date has passed, whatever
// its policy said. A review date is a promise the user made to look again, and
// a passing content hash does not discharge it.
func applyReviewOverlay(rec *Record, now time.Time, st ValidationStatus) ValidationStatus {
	if rec.ReviewAfter == nil || now.Before(*rec.ReviewAfter) {
		return st
	}
	if st.State == ValidationFailed {
		return st
	}
	st.State = ValidationStale
	if st.Reason == "" {
		st.Reason = "review date " + rec.ReviewAfter.UTC().Format(time.RFC3339) + " has passed"
	}
	return st
}

func (v *policyValidator) checkEvidence(ctx context.Context, rec *Record, ws Workspace, st ValidationStatus) ValidationStatus {
	if len(rec.Sources) == 0 {
		st.State = ValidationFailed
		st.Reason = "policy requires evidence but the record has no sources"
		return st
	}
	if v.resolver == nil {
		st.State = ValidationUnverified
		st.Reason = "no source resolver is configured"
		return st
	}
	var missing []string
	for _, src := range rec.Sources {
		ok, err := v.resolver.Exists(ctx, src, ws)
		if err != nil {
			st.State = ValidationUnverified
			st.Reason = "source " + src.ID + " could not be checked: " + err.Error()
			return st
		}
		if !ok {
			missing = append(missing, src.ID)
		}
	}
	if len(missing) == len(rec.Sources) {
		st.State = ValidationFailed
		st.Reason = "no supporting source could be located"
		return st
	}
	if len(missing) > 0 {
		st.State = ValidationStale
		st.Reason = "missing sources: " + strings.Join(missing, ", ")
	}
	return st
}

func (v *policyValidator) checkHashes(ctx context.Context, rec *Record, ws Workspace, st ValidationStatus) ValidationStatus {
	if v.resolver == nil {
		st.State = ValidationUnverified
		st.Reason = "no source resolver is configured"
		return st
	}
	var checked int
	for _, src := range rec.Sources {
		if src.ContentHash == "" {
			continue
		}
		got, err := v.resolver.ContentHash(ctx, src, ws)
		if err != nil {
			st.State = ValidationUnverified
			st.Reason = "source " + src.ID + " could not be hashed: " + err.Error()
			return st
		}
		checked++
		if !strings.EqualFold(got, src.ContentHash) {
			st.State = ValidationStale
			st.Reason = "source " + src.ID + " content changed since capture"
			return st
		}
	}
	if checked == 0 {
		st.State = ValidationUnverified
		st.Reason = "policy requires content hashes but none are recorded"
	}
	return st
}

// checkQuotes asks only whether each source's own text is still there. A
// document holds many rules, and one of them changing says nothing about the
// others, so this is what keeps an edit from demoting a whole file's worth.
func (v *policyValidator) checkQuotes(ctx context.Context, rec *Record, ws Workspace, st ValidationStatus) ValidationStatus {
	if v.resolver == nil {
		st.State = ValidationUnverified
		st.Reason = "no source resolver is configured"
		return st
	}

	var checked int
	for _, src := range rec.Sources {
		text := quotedText(src)
		if text == "" {
			continue
		}
		ok, err := v.resolver.Contains(ctx, src, ws, text)
		if err != nil {
			st.State = ValidationUnverified
			st.Reason = "source " + src.ID + " could not be checked: " + err.Error()
			return st
		}
		checked++
		if !ok {
			st.State = ValidationStale
			st.Reason = "source " + src.ID + " no longer contains the text this record was drawn from"
			return st
		}
	}
	if checked == 0 {
		st.State = ValidationUnverified
		st.Reason = "policy requires quoted evidence but none is recorded"
	}
	return st
}

// quotedText strips the line prefix an extraction adds, leaving the original
// text to look for.
func quotedText(src Source) string {
	quote := src.ExactExcerpt
	if rest, found := strings.CutPrefix(quote, "line "); found {
		if _, after, ok := strings.Cut(rest, ": "); ok {
			return after
		}
	}
	return quote
}

func (v *policyValidator) checkRevision(ctx context.Context, rec *Record, ws Workspace, st ValidationStatus) ValidationStatus {
	if ws.Revision == "" {
		st.State = ValidationUnverified
		st.Reason = "workspace supplied no revision"
		return st
	}
	if v.resolver == nil {
		st.State = ValidationUnverified
		st.Reason = "no source resolver is configured"
		return st
	}
	var checked int
	for _, src := range rec.Sources {
		if src.Revision == "" {
			continue
		}
		ok, err := v.resolver.RevisionApplies(ctx, src, ws)
		if err != nil {
			st.State = ValidationUnverified
			st.Reason = "revision for source " + src.ID + " could not be checked: " + err.Error()
			return st
		}
		checked++
		if !ok {
			st.State = ValidationStale
			st.Reason = "source " + src.ID + " describes revision " + src.Revision + ", which does not apply to " + ws.Revision
			return st
		}
	}
	if checked == 0 {
		st.State = ValidationUnverified
		st.Reason = "policy requires a source revision but none is recorded"
	}
	return st
}

// NewCachingValidator wraps a validator with a bounded-lifetime cache. Git and
// filesystem checks are too expensive to repeat for every record on every call.
func NewCachingValidator(inner Validator, ttl time.Duration) Validator {
	if ttl <= 0 {
		return inner
	}
	return &cachingValidator{inner: inner, ttl: ttl, entries: make(map[string]cachedValidation)}
}

type cachedValidation struct {
	status  ValidationStatus
	expires time.Time
}

type cachingValidator struct {
	inner   Validator
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]cachedValidation
}

func (v *cachingValidator) Validate(ctx context.Context, rec *Record, ws Workspace, now time.Time) ValidationStatus {
	key := strings.Join([]string{rec.ID, rec.UpdatedAt.UTC().Format(time.RFC3339Nano), ws.Revision, ws.Repository}, "|")

	v.mu.Lock()
	entry, ok := v.entries[key]
	v.mu.Unlock()
	if ok && now.Before(entry.expires) {
		// A cached "valid" must still respect a review date that has since passed.
		return applyReviewOverlay(rec, now, entry.status)
	}

	status := v.inner.Validate(ctx, rec, ws, now)

	v.mu.Lock()
	v.entries[key] = cachedValidation{status: status, expires: now.Add(v.ttl)}
	v.mu.Unlock()
	return status
}
