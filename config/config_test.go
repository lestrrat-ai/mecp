package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/config"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoad(t *testing.T) {
	t.Run("reads profiles and durations", func(t *testing.T) {
		path := write(t, `
principal: lestrrat
database: /tmp/does-not-need-to-exist.db
audit: none
validation:
  ttl: 30m
  git: true
defaults:
  context_ttl: 2h
clients:
  claude-code:
    capabilities: [context:prepare, context:search:project]
    max_sensitivity: project
  trusted:
    capabilities: [context:prepare, context:search:personal, context:evidence:personal, context:propose]
    max_sensitivity: personal
`)

		cfg, err := config.Load(path)
		require.NoError(t, err)
		require.Equal(t, "lestrrat", cfg.Principal)
		require.True(t, cfg.Validation.Git)
		require.Equal(t, 30*time.Minute, cfg.Validation.TTL.Duration())
		require.Equal(t, 2*time.Hour, cfg.Defaults.ContextTTL.Duration())
		require.Len(t, cfg.Clients, 2)
	})

	t.Run("an explicit path that does not exist is an error", func(t *testing.T) {
		_, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"))
		require.Error(t, err)
	})

	t.Run("rejects an unknown capability", func(t *testing.T) {
		path := write(t, `
principal: lestrrat
database: /tmp/x.db
clients:
  rogue:
    capabilities: [context:everything]
`)
		_, err := config.Load(path)
		require.Error(t, err)
		require.Contains(t, err.Error(), "context:everything")
	})

	t.Run("rejects an unknown audit sink", func(t *testing.T) {
		path := write(t, "principal: lestrrat\ndatabase: /tmp/x.db\naudit: syslog\n")
		_, err := config.Load(path)
		require.Error(t, err)
		require.Contains(t, err.Error(), "syslog")
	})
}

func TestCallerResolution(t *testing.T) {
	path := write(t, `
principal: lestrrat
database: /tmp/x.db
allowed_roots: [/work]
clients:
  default:
    capabilities: [context:prepare, context:search:project]
    max_sensitivity: project
  trusted:
    capabilities: [context:prepare, context:search:personal, context:propose]
    max_sensitivity: personal
    allowed_repositories: [https://github.com/lestrrat-go/helium]
`)
	cfg, err := config.Load(path)
	require.NoError(t, err)

	t.Run("a named profile gets exactly its capabilities", func(t *testing.T) {
		caller := cfg.Caller("trusted")
		require.True(t, caller.Has(mecp.CapPropose))
		require.Equal(t, mecp.SensitivityPersonal, caller.SensitivityCeiling())
		require.True(t, caller.RepositoryAllowed("https://github.com/lestrrat-go/helium"))
		require.False(t, caller.RepositoryAllowed("https://github.com/example/billing"))
	})

	t.Run("an unknown client falls back to the default profile", func(t *testing.T) {
		caller := cfg.Caller("some-agent-nobody-configured")
		require.False(t, caller.Has(mecp.CapPropose))
		require.Equal(t, mecp.SensitivityProject, caller.SensitivityCeiling())
	})

	t.Run("global allowed roots apply to a profile that names none", func(t *testing.T) {
		require.Equal(t, []string{"/work"}, cfg.Caller("default").AllowedRoots)
	})

	t.Run("with no default profile an unknown client gets nothing", func(t *testing.T) {
		path := write(t, `
principal: lestrrat
database: /tmp/x.db
clients:
  only-this-one:
    capabilities: [context:prepare]
`)
		cfg, err := config.Load(path)
		require.NoError(t, err)

		caller := cfg.Caller("anything-else")
		require.Empty(t, caller.Capabilities)
		require.Error(t, caller.Validate(), "a caller with no capabilities must not be usable")
	})
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")

	cfg := config.Default()
	cfg.Principal = "lestrrat"
	require.NoError(t, cfg.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "configuration may name capabilities, so it is owner-only")

	reloaded, err := config.Load(path)
	require.NoError(t, err)
	require.Equal(t, "lestrrat", reloaded.Principal)
	require.Equal(t, cfg.Defaults.ContextTTL.Duration(), reloaded.Defaults.ContextTTL.Duration())
}
