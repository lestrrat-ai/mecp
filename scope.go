package mecp

import (
	"fmt"
	"maps"
	"net/url"
	"path"
	"slices"
	"strings"
)

// Scope describes where a record applies. Dimensions are conjunctive: every
// populated dimension must match for the record to be applicable. An empty
// dimension means "unconstrained on this axis", not "matches nothing".
//
// A record scoped to a repository never matches a request that carries no
// repository, because the service prefers returning nothing to applying a
// vaguely related record.
type Scope struct {
	User           string            `json:"user,omitempty" yaml:"user,omitempty"`
	Org            string            `json:"org,omitempty" yaml:"org,omitempty"`
	Repository     string            `json:"repository,omitempty" yaml:"repository,omitempty"`
	BranchPatterns []string          `json:"branch_patterns,omitempty" yaml:"branch_patterns,omitempty"`
	PathPatterns   []string          `json:"path_patterns,omitempty" yaml:"path_patterns,omitempty"`
	TaskKinds      []TaskKind        `json:"task_kinds,omitempty" yaml:"task_kinds,omitempty"`
	Conditions     map[string]string `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// Workspace is the caller-supplied description of where work is happening.
type Workspace struct {
	RootURI       string   `json:"root_uri,omitempty" yaml:"root_uri,omitempty"`
	Repository    string   `json:"repository,omitempty" yaml:"repository,omitempty"`
	Revision      string   `json:"revision,omitempty" yaml:"revision,omitempty"`
	Branch        string   `json:"branch,omitempty" yaml:"branch,omitempty"`
	RelevantPaths []string `json:"relevant_paths,omitempty" yaml:"relevant_paths,omitempty"`
}

// EffectiveScope is the scope the service actually resolved and authorized for
// a request. It is echoed back to the caller so an agent can see what the
// service believed it was answering about.
type EffectiveScope struct {
	Principal  string   `json:"principal"`
	Repository string   `json:"repository,omitempty"`
	Org        string   `json:"org,omitempty"`
	Branch     string   `json:"branch,omitempty"`
	Revision   string   `json:"revision,omitempty"`
	Paths      []string `json:"paths,omitempty"`
	TaskKind   TaskKind `json:"task_kind,omitempty"`
}

// ScopeMatch is the outcome of testing a scope against a request.
type ScopeMatch struct {
	Matched     bool
	Specificity int
	Label       string
	Reasons     []string
	Failure     string
}

// scope dimension weights. Repository and path are weighted highest because
// they are the dimensions that most reliably indicate a record was written
// about this exact work.
const (
	weightUser      = 1
	weightOrg       = 2
	weightRepo      = 4
	weightBranch    = 2
	weightPath      = 3
	weightTaskKind  = 2
	weightCondition = 1
)

// MaxScopeSpecificity is the highest score Scope.Match can produce with a
// single condition, and is used to normalize specificity into [0,1].
const MaxScopeSpecificity = weightUser + weightOrg + weightRepo + weightBranch + weightPath + weightTaskKind + weightCondition

// ScopeRequest is everything Scope.Match needs to decide applicability.
type ScopeRequest struct {
	Principal  string
	Workspace  Workspace
	TaskKind   TaskKind
	Conditions map[string]string
}

// Clone returns a deep copy of the scope.
func (s Scope) Clone() Scope {
	out := s
	out.BranchPatterns = slices.Clone(s.BranchPatterns)
	out.PathPatterns = slices.Clone(s.PathPatterns)
	out.TaskKinds = slices.Clone(s.TaskKinds)
	if s.Conditions != nil {
		out.Conditions = maps.Clone(s.Conditions)
	}
	return out
}

// Normalize canonicalizes repository identity and sorts pattern lists so that
// two equivalent scopes serialize identically.
func (s *Scope) Normalize() {
	s.User = strings.TrimSpace(s.User)
	s.Repository = CanonicalRepository(s.Repository)
	if s.Org == "" && s.Repository != "" {
		s.Org = RepositoryOrg(s.Repository)
	}
	s.Org = strings.ToLower(strings.TrimSpace(s.Org))

	s.BranchPatterns = normalizePatterns(s.BranchPatterns)
	s.PathPatterns = normalizePatterns(s.PathPatterns)
	slices.Sort(s.TaskKinds)
	s.TaskKinds = slices.Compact(s.TaskKinds)

	if len(s.Conditions) == 0 {
		s.Conditions = nil
		return
	}
	conds := make(map[string]string, len(s.Conditions))
	for k, v := range s.Conditions {
		conds[strings.ToLower(strings.TrimSpace(k))] = strings.ToLower(strings.TrimSpace(v))
	}
	s.Conditions = conds
}

func normalizePatterns(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// Validate reports the first structural problem with a scope.
func (s Scope) Validate() error {
	for _, k := range s.TaskKinds {
		if !k.Valid() {
			return fmt.Errorf(`scope: invalid task kind %q`, k)
		}
	}
	for _, p := range s.BranchPatterns {
		if _, err := path.Match(p, "probe"); err != nil {
			return fmt.Errorf(`scope: invalid branch pattern %q: %w`, p, err)
		}
	}
	for _, p := range s.PathPatterns {
		if _, err := path.Match(strings.TrimSuffix(p, "/"), "probe"); err != nil {
			return fmt.Errorf(`scope: invalid path pattern %q: %w`, p, err)
		}
	}
	return nil
}

// Global reports whether the scope constrains nothing beyond the principal.
func (s Scope) Global() bool {
	return s.Org == "" && s.Repository == "" && len(s.BranchPatterns) == 0 &&
		len(s.PathPatterns) == 0 && len(s.TaskKinds) == 0 && len(s.Conditions) == 0
}

// Match tests the scope against a request and reports both applicability and
// how specific the match was. Specificity feeds ranking: a record written about
// this repository and this task kind outranks a user-wide preference.
func (s Scope) Match(req ScopeRequest) ScopeMatch {
	var m ScopeMatch

	if s.User != "" {
		if !strings.EqualFold(s.User, req.Principal) {
			m.Failure = "principal mismatch"
			return m
		}
		m.Specificity += weightUser
	}

	repo := CanonicalRepository(req.Workspace.Repository)
	if s.Repository != "" {
		if repo == "" {
			m.Failure = "record is repository-scoped but the request supplied no repository"
			return m
		}
		if s.Repository != repo {
			m.Failure = "repository mismatch"
			return m
		}
		m.Specificity += weightRepo
		m.Reasons = append(m.Reasons, "scope: repository match")
	}

	if s.Org != "" {
		org := RepositoryOrg(repo)
		if org == "" || !strings.EqualFold(s.Org, org) {
			m.Failure = "organization mismatch"
			return m
		}
		m.Specificity += weightOrg
		m.Reasons = append(m.Reasons, "scope: organization match")
	}

	if len(s.BranchPatterns) > 0 {
		pattern, ok := matchAny(s.BranchPatterns, req.Workspace.Branch)
		if !ok {
			m.Failure = "branch mismatch"
			return m
		}
		if pattern != "*" {
			m.Specificity += weightBranch
			m.Reasons = append(m.Reasons, "scope: branch matches "+pattern)
		}
	}

	if len(s.PathPatterns) > 0 {
		pattern, ok := matchAnyPath(s.PathPatterns, req.Workspace.RelevantPaths)
		if !ok {
			m.Failure = "no relevant path matches the record's path scope"
			return m
		}
		m.Specificity += weightPath
		m.Reasons = append(m.Reasons, "scope: path matches "+pattern)
	}

	// An unsupplied task kind is unknown, not "other": the caller told us
	// nothing, so the restriction cannot be evaluated. Applying the record
	// without the specificity bonus keeps a host that omits the field usable,
	// while an explicitly stated kind is still matched strictly.
	if len(s.TaskKinds) > 0 {
		switch {
		case req.TaskKind == "":
			m.Reasons = append(m.Reasons, "scope: task kind was not supplied, so its task-kind restriction was not applied")
		case !slices.Contains(s.TaskKinds, req.TaskKind):
			m.Failure = "task kind mismatch"
			return m
		default:
			m.Specificity += weightTaskKind
			m.Reasons = append(m.Reasons, "scope: task kind "+string(req.TaskKind))
		}
	}

	for k, v := range s.Conditions {
		got, ok := req.Conditions[k]
		if !ok || !strings.EqualFold(got, v) {
			m.Failure = "condition " + k + " not satisfied"
			return m
		}
		m.Specificity += weightCondition
	}
	if len(s.Conditions) > 0 {
		m.Reasons = append(m.Reasons, fmt.Sprintf("scope: %d condition(s) satisfied", len(s.Conditions)))
	}

	m.Matched = true
	m.Label = s.SpecificityLabel()
	if len(m.Reasons) == 0 {
		m.Reasons = append(m.Reasons, "scope: applies to any workspace")
	}
	slices.Sort(m.Reasons)
	return m
}

// SpecificityLabel names the dimensions the scope constrains, for example
// "repository_and_task_kind". A scope that constrains nothing is "global".
func (s Scope) SpecificityLabel() string {
	var dims []string
	if s.Repository != "" {
		dims = append(dims, "repository")
	} else if s.Org != "" {
		dims = append(dims, "org")
	}
	if len(s.BranchPatterns) > 0 && !slices.Equal(s.BranchPatterns, []string{"*"}) {
		dims = append(dims, "branch")
	}
	if len(s.PathPatterns) > 0 {
		dims = append(dims, "path")
	}
	if len(s.TaskKinds) > 0 {
		dims = append(dims, "task_kind")
	}
	if len(s.Conditions) > 0 {
		dims = append(dims, "condition")
	}
	if len(dims) == 0 {
		return "global"
	}
	return strings.Join(dims, "_and_")
}

func matchAny(patterns []string, value string) (string, bool) {
	for _, p := range patterns {
		if p == "*" {
			return p, true
		}
		if value == "" {
			continue
		}
		if ok, err := path.Match(p, value); err == nil && ok {
			return p, true
		}
	}
	return "", false
}

// matchAnyPath treats a trailing slash as a directory prefix, so "xmldsig1/"
// matches "xmldsig1/sign.go" as well as the directory itself.
func matchAnyPath(patterns, values []string) (string, bool) {
	for _, p := range patterns {
		clean := strings.TrimSuffix(p, "/")
		for _, v := range values {
			v = strings.TrimPrefix(strings.TrimSpace(v), "./")
			if v == "" {
				continue
			}
			if strings.HasSuffix(p, "/") && (v == clean || strings.HasPrefix(v, clean+"/")) {
				return p, true
			}
			if ok, err := path.Match(clean, v); err == nil && ok {
				return p, true
			}
			if ok, err := path.Match(clean, path.Dir(v)); err == nil && ok {
				return p, true
			}
			// A pattern with no separator, such as "*.go", is about file names
			// rather than locations, so it is also matched against the base
			// name. path.Match's "*" never crosses a "/" on its own.
			if !strings.Contains(clean, "/") {
				if ok, err := path.Match(clean, path.Base(v)); err == nil && ok {
					return p, true
				}
			}
			if strings.HasPrefix(v, clean+"/") {
				return p, true
			}
		}
	}
	return "", false
}

// CanonicalRepository reduces the many spellings of a Git remote to one
// identity so that an SSH remote and an HTTPS remote resolve to the same
// records. Forks stay distinct: nothing here rewrites the owner.
func CanonicalRepository(in string) string {
	in = strings.TrimSpace(in)
	if in == "" {
		return ""
	}

	if !strings.Contains(in, "://") {
		// scp-like syntax: git@github.com:owner/repo.git
		if host, rest, ok := strings.Cut(in, ":"); ok && !strings.HasPrefix(rest, "//") {
			if _, h, found := strings.Cut(host, "@"); found {
				host = h
			}
			return normalizeRepoParts(host, rest)
		}
		// Bare "github.com/owner/repo", which is how an agent usually names a
		// repository when it has not read the git remote. Without this it would
		// become an identity of its own and quietly match no record.
		if host, rest, ok := strings.Cut(in, "/"); ok && looksLikeHost(host) {
			return normalizeRepoParts(host, rest)
		}
		return strings.ToLower(in)
	}

	u, err := url.Parse(in)
	if err != nil {
		return strings.ToLower(in)
	}
	if u.Scheme == "file" {
		return "file://" + path.Clean(u.Path)
	}
	return normalizeRepoParts(u.Host, u.Path)
}

func normalizeRepoParts(host, repoPath string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if _, h, ok := strings.Cut(host, "@"); ok {
		host = h
	}
	host = strings.TrimSuffix(host, ":443")
	repoPath = strings.Trim(strings.TrimSpace(repoPath), "/")
	repoPath = strings.TrimSuffix(repoPath, ".git")
	if host == "" {
		return strings.ToLower(repoPath)
	}
	return "https://" + host + "/" + repoPath
}

// RepositoryOrg extracts the owning namespace of a canonical repository, for
// example "github.com/lestrrat-go".
func RepositoryOrg(canonical string) string {
	if canonical == "" {
		return ""
	}
	trimmed := strings.TrimPrefix(canonical, "https://")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 3 {
		return ""
	}
	return strings.ToLower(parts[0] + "/" + parts[1])
}

// looksLikeHost reports whether a path's first segment is a hostname rather
// than a directory. A hostname carries a dot and is not a relative-path
// element, which is what separates "github.com/owner/repo" from "../owner/repo"
// and from an ordinary directory name.
func looksLikeHost(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	if !strings.Contains(segment, ".") {
		return false
	}
	// A leading or trailing dot is a filename, not a host.
	return !strings.HasPrefix(segment, ".") && !strings.HasSuffix(segment, ".")
}
