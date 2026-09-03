package mecp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEvent is the local record of one service call. It deliberately omits
// task text and record statements: an audit trail that copies the data it is
// auditing doubles the disclosure surface.
type AuditEvent struct {
	At           time.Time      `json:"at"`
	PrincipalID  string         `json:"principal_id"`
	ClientID     string         `json:"client_id"`
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
