package mecp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultAuditLimit bounds an audit query that does not ask for a size.
const DefaultAuditLimit = 50

// maxAuditLineBytes bounds one JSONL audit line. A line longer than this is a
// corrupt log rather than an event, and refusing it keeps a damaged file from
// being read into memory whole.
const maxAuditLineBytes = 1 << 20

// AuditEvent is the local record of one service call. It deliberately omits
// task text and record statements: an audit trail that copies the data it is
// auditing doubles the disclosure surface.
//
// Origin says which interface the call came through, because the client profile
// alone cannot tell an agent's call apart from a CLI run made under the same
// profile. It is absent from events written before origins were recorded, and
// those read back as Origin "" rather than as either interface.
type AuditEvent struct {
	At           time.Time      `json:"at"`
	PrincipalID  string         `json:"principal_id"`
	ClientID     string         `json:"client_id"`
	SessionID    string         `json:"session_id,omitempty"`
	Origin       Origin         `json:"origin,omitempty"`
	Operation    string         `json:"operation"`
	Scope        EffectiveScope `json:"scope"`
	RecordIDs    []string       `json:"record_ids,omitempty"`
	WarningCodes []WarningCode  `json:"warning_codes,omitempty"`
	Truncated    bool           `json:"truncated"`
	ResultCount  int            `json:"result_count"`
	LatencyMS    int64          `json:"latency_ms"`
	ProposalID   string         `json:"proposal_id,omitempty"`
	ErrorCode    ErrorCode      `json:"error_code,omitempty"`
}

// AuditSink persists audit events.
type AuditSink interface {
	Write(ctx context.Context, ev AuditEvent) error
}

// AuditQuery bounds a read of the audit trail.
type AuditQuery struct {
	// Limit is how many of the most recent events to return. Zero or less
	// means DefaultAuditLimit.
	Limit int
	// Since drops events that happened before it. The zero value keeps every
	// event.
	Since time.Time
}

func (q AuditQuery) limit() int {
	if q.Limit <= 0 {
		return DefaultAuditLimit
	}
	return q.Limit
}

// AuditReader reads events back out of one sink's storage. Each sink that
// persists events has a matching reader, so a caller can read from whichever
// sink the configuration selected.
type AuditReader interface {
	AuditEvents(ctx context.Context, q AuditQuery) ([]AuditEvent, error)
}

// NopAudit discards events. It is the default so that a library user does not
// silently accumulate a log file they never asked for.
type NopAudit struct{}

func (NopAudit) Write(context.Context, AuditEvent) error { return nil }

// JSONLAudit appends one JSON object per line to a file with owner-only
// permissions.
type JSONLAudit struct {
	mu   sync.Mutex
	path string
}

// NewJSONLAudit creates the containing directory and returns a sink writing to
// path.
func NewJSONLAudit(path string) (*JSONLAudit, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, wrapf(CodeStorage, err, "cannot create audit directory")
	}
	return &JSONLAudit{path: path}, nil
}

func (a *JSONLAudit) Write(_ context.Context, ev AuditEvent) error {
	buf, err := json.Marshal(ev)
	if err != nil {
		return wrapf(CodeStorage, err, "cannot encode audit event")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return wrapf(CodeStorage, err, "cannot open audit log")
	}
	defer f.Close()

	if _, err := f.Write(append(buf, '\n')); err != nil {
		return wrapf(CodeStorage, err, "cannot append audit event")
	}
	return nil
}

// JSONLAuditReader reads events back out of a JSONL audit log. Reading is a
// separate type from JSONLAudit so that inspecting a trail never creates the
// file or its directory.
type JSONLAuditReader struct {
	path string
}

// NewJSONLAuditReader returns a reader over the audit log at path.
func NewJSONLAuditReader(path string) *JSONLAuditReader {
	return &JSONLAuditReader{path: path}
}

// Path reports which log the reader reads.
func (r *JSONLAuditReader) Path() string { return r.path }

// AuditEvents returns the most recent matching events, newest first, so that
// the order matches the SQLite sink. A log that does not exist yet holds no
// events, which is not an error; a line that does not decode is, because
// dropping part of an audit trail without saying so is worse than failing.
func (r *JSONLAuditReader) AuditEvents(ctx context.Context, q AuditQuery) ([]AuditEvent, error) {
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, wrapf(CodeStorage, err, "cannot open audit log %s", r.path)
	}
	defer f.Close()

	// The file is append-ordered, so the events wanted are the last ones read.
	// A ring of limit entries bounds what a long log costs to scan.
	limit := q.limit()
	ring := make([]AuditEvent, limit)
	var matched int

	scanner := bufio.NewScanner(f)
	scanner.Buffer(nil, maxAuditLineBytes)
	var line int
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line++
		buf := bytes.TrimSpace(scanner.Bytes())
		if len(buf) == 0 {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal(buf, &ev); err != nil {
			return nil, wrapf(CodeStorage, err, "cannot decode audit log %s at line %d", r.path, line)
		}
		if !q.Since.IsZero() && ev.At.Before(q.Since) {
			continue
		}
		ring[matched%limit] = ev
		matched++
	}
	if err := scanner.Err(); err != nil {
		return nil, wrapf(CodeStorage, err, "cannot read audit log %s at line %d", r.path, line+1)
	}

	n := min(matched, limit)
	out := make([]AuditEvent, n)
	for i := range n {
		out[i] = ring[(matched-1-i)%limit]
	}
	return out, nil
}

var _ AuditReader = (*JSONLAuditReader)(nil)
