package mecp

import (
	"slices"
	"strings"
)

// Capability is a coarse permission granted to a client profile. Capabilities
// gate both tool availability and the sensitivity ceiling applied before
// retrieval.
type Capability string

const (
	CapPrepare          Capability = "context:prepare"
	CapSearchProject    Capability = "context:search:project"
	CapSearchPersonal   Capability = "context:search:personal"
	CapEvidenceProject  Capability = "context:evidence:project"
	CapEvidencePersonal Capability = "context:evidence:personal"
	CapPropose          Capability = "context:propose"
	CapAdmin            Capability = "context:admin"
)

// AllCapabilities lists every capability in a stable order.
var AllCapabilities = []Capability{
	CapPrepare,
	CapSearchProject,
	CapSearchPersonal,
	CapEvidenceProject,
	CapEvidencePersonal,
	CapPropose,
	CapAdmin,
}

func (c Capability) Valid() bool { return slices.Contains(AllCapabilities, c) }

// Caller is the trusted identity of whoever is asking. The MCP adapter derives
// it from server configuration; it is never accepted as a tool argument,
// because a tool argument is model-controlled input.
type Caller struct {
	PrincipalID         string
	ClientID            string
	Capabilities        []Capability
	MaxSensitivity      Sensitivity
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

// SensitivityCeiling returns the highest sensitivity this caller may see. It is
// the lower of the configured ceiling and what the caller's capabilities imply,
// so a misconfigured profile cannot widen disclosure by accident.
func (c Caller) SensitivityCeiling() Sensitivity {
	implied := SensitivityPublic
	if c.Has(CapSearchProject) || c.Has(CapPrepare) || c.Has(CapEvidenceProject) {
		implied = SensitivityProject
	}
	if c.Has(CapSearchPersonal) || c.Has(CapEvidencePersonal) {
		implied = SensitivityPersonal
	}
	if slices.Contains(c.Capabilities, CapAdmin) {
		implied = SensitivityRestricted
	}

	configured := c.MaxSensitivity
	if !configured.Valid() {
		return implied
	}
	if configured.Level() < implied.Level() {
		return configured
	}
	return implied
}

// EvidenceCeiling returns the highest sensitivity whose verbatim source
// excerpts may be disclosed. Evidence is held to a stricter bar than record
// statements: a client may learn that a personal preference applies without
// being handed the conversation it came from.
func (c Caller) EvidenceCeiling() Sensitivity {
	if slices.Contains(c.Capabilities, CapAdmin) {
		return SensitivityRestricted
	}
	if c.Has(CapEvidencePersonal) {
		return minSensitivity(SensitivityPersonal, c.SensitivityCeiling())
	}
	if c.Has(CapEvidenceProject) {
		return minSensitivity(SensitivityProject, c.SensitivityCeiling())
	}
	return SensitivityPublic
}

func minSensitivity(a, b Sensitivity) Sensitivity {
	if a.Level() <= b.Level() {
		return a
	}
	return b
}

// RepositoryAllowed reports whether the caller may query records scoped to a
// repository. An empty AllowedRepositories list means the profile is not
// restricted by repository.
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
