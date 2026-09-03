package mecp

import (
	"slices"
	"strings"
)

// Capability is a coarse permission granted to a client profile. Capabilities
// gate which tools a client sees and whether it may read verbatim source text.
//
// There is deliberately no per-record privacy level. Everything in the store
// exists to be sent to a model, so the rule is not to store what you are
// unwilling to send. See docs/design-deltas.md.
type Capability string

const (
	CapPrepare  Capability = "context:prepare"
	CapSearch   Capability = "context:search"
	CapEvidence Capability = "context:evidence"
	CapPropose  Capability = "context:propose"
	CapAdmin    Capability = "context:admin"
)

// AllCapabilities lists every capability in a stable order.
var AllCapabilities = []Capability{
	CapPrepare,
	CapSearch,
	CapEvidence,
	CapPropose,
	CapAdmin,
}

func (c Capability) Valid() bool { return slices.Contains(AllCapabilities, c) }

// Caller is the trusted identity of whoever is asking. The MCP adapter derives
// it from server configuration; it is never accepted as a tool argument,
// because a tool argument is model-controlled input.
//
// On the stdio transport the process boundary is the real identity: whoever can
// launch the server already runs as the user and can read the database
// directly. The client profile is therefore a convenience for shaping what each
// host sees, not a security control. That changes the day a socket or HTTP
// endpoint appears, which is when authentication becomes mandatory.
type Caller struct {
	PrincipalID         string
	ClientID            string
	Capabilities        []Capability
	AllowedRepositories []string
	AllowedRoots        []string
}

// Has reports whether the caller holds a capability. An admin caller holds
// every capability.
func (c Caller) Has(cap Capability) bool {
	if slices.Contains(c.Capabilities, CapAdmin) {
		return true
	}
	return slices.Contains(c.Capabilities, cap)
}

// RepositoryAllowed reports whether the caller may query records scoped to a
// repository. An empty AllowedRepositories list means the profile is not
// restricted by repository.
//
// With privacy labels gone, this and the principal are what keep one project's
// context out of another.
func (c Caller) RepositoryAllowed(canonical string) bool {
	if len(c.AllowedRepositories) == 0 {
		return true
	}
	for _, allowed := range c.AllowedRepositories {
		if strings.EqualFold(CanonicalRepository(allowed), canonical) {
			return true
		}
	}
	return false
}

// Validate reports the first structural problem with a caller identity.
func (c Caller) Validate() error {
	if c.PrincipalID == "" {
		return errorf(CodeUnauthorizedScope, "caller has no principal")
	}
	if c.ClientID == "" {
		return errorf(CodeUnauthorizedScope, "caller has no client profile")
	}
	if len(c.Capabilities) == 0 {
		return errorf(CodeUnauthorizedScope, "client profile %q grants no capabilities", c.ClientID)
	}
	for _, cap := range c.Capabilities {
		if !cap.Valid() {
			return errorf(CodeUnauthorizedScope, "client profile %q declares unknown capability %q", c.ClientID, cap)
		}
	}
	return nil
}
