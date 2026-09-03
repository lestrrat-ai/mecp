package mecp_test

import (
	"testing"

	"github.com/lestrrat-ai/mecp"
	"github.com/stretchr/testify/require"
)

func TestCanonicalRepository(t *testing.T) {
	t.Run("equivalent spellings resolve to one identity", func(t *testing.T) {
		for _, in := range []string{
			"git@github.com:lestrrat-go/helium.git",
			"git@github.com:lestrrat-go/helium",
			"ssh://git@github.com/lestrrat-go/helium.git",
			"https://github.com/lestrrat-go/helium.git",
			"https://github.com/lestrrat-go/helium/",
			"https://GitHub.com/lestrrat-go/helium",
		} {
			require.Equal(t, heliumRepo, mecp.CanonicalRepository(in), in)
		}
	})

	t.Run("a bare host and path resolves like the URL", func(t *testing.T) {
		// This is how an agent names a repository when it has not read the git
		// remote, and it was silently becoming an identity of its own.
		for _, in := range []string{
			"github.com/lestrrat-go/helium",
			"github.com/lestrrat-go/helium.git",
			"github.com/lestrrat-go/helium/",
			"GitHub.com/lestrrat-go/helium",
		} {
			require.Equal(t, heliumRepo, mecp.CanonicalRepository(in), in)
		}
	})

	t.Run("a local path is not mistaken for a host", func(t *testing.T) {
		for _, in := range []string{
			"/home/lestrrat/dev/helium",
			"../helium",
			"./helium",
			"helium",
			"my-notes/helium",
		} {
			require.NotContains(t, mecp.CanonicalRepository(in), "https://", in)
		}
	})

	t.Run("a fork stays distinct from its upstream", func(t *testing.T) {
		fork := mecp.CanonicalRepository("git@github.com:someone-else/helium.git")
		require.NotEqual(t, heliumRepo, fork)
	})

	t.Run("empty input yields empty output", func(t *testing.T) {
		require.Empty(t, mecp.CanonicalRepository(""))
		require.Empty(t, mecp.CanonicalRepository("   "))
	})

	t.Run("organization is derived from the canonical form", func(t *testing.T) {
		require.Equal(t, "github.com/lestrrat-go", mecp.RepositoryOrg(heliumRepo))
		require.Empty(t, mecp.RepositoryOrg(""))
	})
}

func TestScopeMatch(t *testing.T) {
	req := func(mutate func(*mecp.ScopeRequest)) mecp.ScopeRequest {
		r := mecp.ScopeRequest{
			Principal: "local-user",
			Workspace: mecp.Workspace{
				Repository:    heliumRepo,
				Branch:        "main",
				RelevantPaths: []string{"xmldsig1/sign.go"},
			},
			TaskKind: mecp.TaskCodeReview,
		}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}

	t.Run("an unconstrained scope matches anything", func(t *testing.T) {
		var s mecp.Scope
		s.Normalize()
		m := s.Match(req(nil))
		require.True(t, m.Matched)
		require.Equal(t, "global", m.Label)
		require.Zero(t, m.Specificity)
	})

	t.Run("a repository-scoped record needs a repository", func(t *testing.T) {
		s := mecp.Scope{Repository: heliumRepo}
		s.Normalize()

		require.True(t, s.Match(req(nil)).Matched)

		m := s.Match(req(func(r *mecp.ScopeRequest) { r.Workspace.Repository = "" }))
		require.False(t, m.Matched)
		require.Contains(t, m.Failure, "repository")
	})

	t.Run("another principal's record never matches", func(t *testing.T) {
		s := mecp.Scope{User: "someone-else"}
		s.Normalize()
		require.False(t, s.Match(req(nil)).Matched)
	})

	t.Run("a directory path pattern matches files beneath it", func(t *testing.T) {
		s := mecp.Scope{PathPatterns: []string{"xmldsig1/"}}
		s.Normalize()
		require.True(t, s.Match(req(nil)).Matched)

		require.False(t, s.Match(req(func(r *mecp.ScopeRequest) {
			r.Workspace.RelevantPaths = []string{"xmlenc/decrypt.go"}
		})).Matched)
	})

	t.Run("a glob path pattern matches by extension", func(t *testing.T) {
		s := mecp.Scope{PathPatterns: []string{"*.go"}}
		s.Normalize()
		require.True(t, s.Match(req(nil)).Matched)
	})

	t.Run("a branch pattern is a glob", func(t *testing.T) {
		s := mecp.Scope{BranchPatterns: []string{"release/*"}}
		s.Normalize()
		require.False(t, s.Match(req(nil)).Matched)
		require.True(t, s.Match(req(func(r *mecp.ScopeRequest) {
			r.Workspace.Branch = "release/v1"
		})).Matched)
	})

	t.Run("an explicit task kind must match", func(t *testing.T) {
		s := mecp.Scope{TaskKinds: []mecp.TaskKind{mecp.TaskRelease}}
		s.Normalize()
		require.False(t, s.Match(req(nil)).Matched)
	})

	t.Run("an unsupplied task kind does not hide task-scoped records", func(t *testing.T) {
		s := mecp.Scope{TaskKinds: []mecp.TaskKind{mecp.TaskRelease}}
		s.Normalize()

		m := s.Match(req(func(r *mecp.ScopeRequest) { r.TaskKind = "" }))
		require.True(t, m.Matched)
		require.Zero(t, m.Specificity, "an unevaluated dimension must not earn a specificity bonus")
	})

	t.Run("every condition must hold", func(t *testing.T) {
		s := mecp.Scope{Conditions: map[string]string{"language": "go", "repository_type": "library"}}
		s.Normalize()

		require.False(t, s.Match(req(nil)).Matched)
		require.False(t, s.Match(req(func(r *mecp.ScopeRequest) {
			r.Conditions = map[string]string{"language": "go"}
		})).Matched)
		require.True(t, s.Match(req(func(r *mecp.ScopeRequest) {
			r.Conditions = map[string]string{"language": "Go", "repository_type": "library"}
		})).Matched)
	})

	t.Run("more constrained scopes score higher", func(t *testing.T) {
		broad := mecp.Scope{Repository: heliumRepo}
		broad.Normalize()
		narrow := mecp.Scope{
			Repository:   heliumRepo,
			PathPatterns: []string{"xmldsig1/"},
			TaskKinds:    []mecp.TaskKind{mecp.TaskCodeReview},
		}
		narrow.Normalize()

		require.Greater(t, narrow.Match(req(nil)).Specificity, broad.Match(req(nil)).Specificity)
		require.Equal(t, "repository_and_path_and_task_kind", narrow.Match(req(nil)).Label)
	})
}

func TestAuthorityOrdering(t *testing.T) {
	t.Run("authority ranks explicit user above inference", func(t *testing.T) {
		require.Greater(t, mecp.AuthorityUser.Tier(), mecp.AuthorityInferred.Tier())
		require.Greater(t, mecp.AuthorityRepository.Tier(), mecp.AuthorityUser.Tier())
		require.True(t, mecp.AuthorityUser.Directive())
		require.False(t, mecp.AuthorityObserved.Directive())
	})

	t.Run("an unknown authority cannot outrank a known one", func(t *testing.T) {
		require.Zero(t, mecp.Authority("something-new").Tier())
		require.False(t, mecp.Authority("something-new").Directive())
	})
}

func TestCapabilities(t *testing.T) {
	t.Run("admin holds every capability", func(t *testing.T) {
		admin := mecp.Caller{
			PrincipalID:  "local-user",
			ClientID:     "contextctl",
			Capabilities: []mecp.Capability{mecp.CapAdmin},
		}
		for _, cap := range mecp.AllCapabilities {
			require.True(t, admin.Has(cap), cap)
		}
	})

	t.Run("reading statements does not imply reading quoted source text", func(t *testing.T) {
		caller := mecp.Caller{
			PrincipalID:  "local-user",
			ClientID:     "agent",
			Capabilities: []mecp.Capability{mecp.CapPrepare, mecp.CapSearch},
		}
		require.True(t, caller.Has(mecp.CapSearch))
		require.False(t, caller.Has(mecp.CapEvidence))
	})

	t.Run("a profile with no capabilities is unusable", func(t *testing.T) {
		caller := mecp.Caller{PrincipalID: "local-user", ClientID: "agent"}
		require.Error(t, caller.Validate())
	})

	t.Run("an unknown capability is refused rather than ignored", func(t *testing.T) {
		caller := mecp.Caller{
			PrincipalID:  "local-user",
			ClientID:     "agent",
			Capabilities: []mecp.Capability{"context:everything"},
		}
		require.Error(t, caller.Validate())
	})
}

func TestRepositoryAllowlist(t *testing.T) {
	caller := mecp.Caller{
		PrincipalID:         "local-user",
		ClientID:            "agent",
		Capabilities:        []mecp.Capability{mecp.CapPrepare},
		AllowedRepositories: []string{"git@github.com:lestrrat-go/helium.git"},
	}

	t.Run("the allowlist is compared after canonicalization", func(t *testing.T) {
		require.True(t, caller.RepositoryAllowed(heliumRepo))
	})

	t.Run("anything else is refused", func(t *testing.T) {
		require.False(t, caller.RepositoryAllowed("https://github.com/example/billing"))
	})

	t.Run("an empty allowlist means unrestricted", func(t *testing.T) {
		open := caller
		open.AllowedRepositories = nil
		require.True(t, open.RepositoryAllowed("https://github.com/example/billing"))
	})
}
