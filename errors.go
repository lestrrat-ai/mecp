package mecp

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable, machine-readable domain failure code. The MCP adapter
// surfaces it verbatim so an agent can decide whether retrying is worthwhile.
type ErrorCode string

const (
	CodeInvalidScope        ErrorCode = "invalid_scope"
	CodeUnauthorizedScope   ErrorCode = "unauthorized_scope"
	CodeAmbiguousRepository ErrorCode = "ambiguous_repository"
	CodeContextExpired      ErrorCode = "context_expired"
	CodeRecordNotFound      ErrorCode = "record_not_found"
	CodeSourceUnavailable   ErrorCode = "source_unavailable"
	CodeBudgetTooSmall      ErrorCode = "budget_too_small"
	CodeProposalDisabled    ErrorCode = "proposal_disabled"
	CodeInvalidRecord       ErrorCode = "invalid_record"
	CodeStorage             ErrorCode = "storage_error"
)

// Error is a domain failure carrying a stable code.
type Error struct {
	Code    ErrorCode
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return string(e.Code) + ": " + e.Message
	}
	return string(e.Code) + ": " + e.Message + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf reports the domain code carried by err, or an empty code when err did
// not originate in the domain layer.
func CodeOf(err error) ErrorCode {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func wrapf(code ErrorCode, err error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

// WarningCode identifies a non-fatal condition reported alongside a successful
// result. Stale records, conflicts, and unvalidated evidence are warnings, not
// errors: withholding the whole answer would be worse than flagging part of it.
type WarningCode string

const (
	WarnStaleRecord       WarningCode = "stale_record"
	WarnSupersededRecord  WarningCode = "superseded_record"
	WarnRevisionMismatch  WarningCode = "historical_revision_mismatch"
	WarnSourceUnavailable WarningCode = "source_unavailable"
	WarnTruncated         WarningCode = "context_truncated"
	WarnNoWorkspace       WarningCode = "no_workspace_supplied"
	WarnEvidenceRedacted  WarningCode = "evidence_redacted"
	WarnRecordNotFound    WarningCode = "record_not_found"
	WarnConflict          WarningCode = "conflicting_records"
	WarnDisputedRecord    WarningCode = "disputed_record"
)

// Warning is a non-fatal condition attached to a successful result.
type Warning struct {
	Code      WarningCode `json:"code"`
	Message   string      `json:"message"`
	RecordIDs []string    `json:"record_ids,omitempty"`
}
