package mecp

import (
	"time"

	"github.com/lestrrat-go/option/v3"
)

// ServiceOption configures New. Options are folded into the service's private
// configuration in the constructor, so a constructed service is immutable and
// safe to share across goroutines.
type ServiceOption interface {
	option.Interface
	serviceOption()
}

type serviceOption struct {
	option.Interface
}

func (*serviceOption) serviceOption() {}

func newServiceOption(ident, value any) ServiceOption {
	return &serviceOption{option.New(ident, value)}
}

type identClock struct{}
type identRanker struct{}
type identPacker struct{}
type identValidator struct{}
type identSourceResolver struct{}
type identValidationTTL struct{}
type identAuditSink struct{}
type identContextTTL struct{}
type identMaxCandidates struct{}
type identRepositoryAliases struct{}

// WithClock replaces the time source. Tests use it to make freshness and
// expiry deterministic.
func WithClock(c Clock) ServiceOption { return newServiceOption(identClock{}, c) }

// WithRanker replaces the scoring model.
func WithRanker(r Ranker) ServiceOption { return newServiceOption(identRanker{}, r) }

// WithPacker replaces the budget packer.
func WithPacker(p Packer) ServiceOption { return newServiceOption(identPacker{}, p) }

// WithValidator replaces the freshness engine outright. Prefer
// WithSourceResolver unless the policy logic itself needs to change.
func WithValidator(v Validator) ServiceOption { return newServiceOption(identValidator{}, v) }

// WithSourceResolver supplies the filesystem and Git access that
// evidence-backed validation policies need.
func WithSourceResolver(r SourceResolver) ServiceOption {
	return newServiceOption(identSourceResolver{}, r)
}

// WithValidationTTL bounds how long a freshness result is reused. Zero
// disables caching.
func WithValidationTTL(d time.Duration) ServiceOption {
	return newServiceOption(identValidationTTL{}, d)
}

// WithAuditSink records one audit event per call.
func WithAuditSink(a AuditSink) ServiceOption { return newServiceOption(identAuditSink{}, a) }

// WithContextTTL sets how long a context handle stays usable.
func WithContextTTL(d time.Duration) ServiceOption { return newServiceOption(identContextTTL{}, d) }

// WithMaxCandidates bounds how many records the retrieval stage may consider
// before ranking.
func WithMaxCandidates(n int) ServiceOption { return newServiceOption(identMaxCandidates{}, n) }

// WithRepositoryAliases maps alternate remote spellings onto one canonical
// repository identity. Keys and values are both canonicalized first.
func WithRepositoryAliases(m map[string]string) ServiceOption {
	return newServiceOption(identRepositoryAliases{}, m)
}

// Clock supplies the current time. Production uses SystemClock.
type Clock interface {
	Now() time.Time
}

// SystemClock reads the wall clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock returns a constant time. It exists for tests and for
// reproducing a past context pack.
type FixedClock struct {
	Time time.Time
}

func (c FixedClock) Now() time.Time { return c.Time.UTC() }
