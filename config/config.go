// Package config loads mecp's on-disk configuration and turns a client
// identifier into an authorized caller.
//
// Configuration validation fails closed: an unknown client profile receives
// the configured default profile, and when no default exists it receives no
// capabilities at all rather than a permissive fallback.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/lestrrat-ai/mecp"
)

// AppName is the directory name used under the user's config, data, and state
// directories.
const AppName = "mecp"

// DefaultClientID is the profile applied to a client that does not identify
// itself, and the fallback for an unrecognized one.
const DefaultClientID = "default"

// Duration is a time.Duration that reads from YAML as a string such as "15m".
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

func (d *Duration) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return err
	}
	s = strings.Trim(strings.TrimSpace(s), `"`)
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf(`failed to parse duration %q: %w`, s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the whole of mecp's configuration.
type Config struct {
	// Principal identifies the human whose context this store holds. Records
	// scoped to another principal are never returned.
	Principal string `yaml:"principal"`

	Database    string `yaml:"database"`
	EvidenceDir string `yaml:"evidence_dir,omitempty"`
	AuditLog    string `yaml:"audit_log,omitempty"`

	// Audit selects where audit events go: "jsonl", "sqlite", or "none".
	Audit string `yaml:"audit"`

	// AllowedRoots restricts which workspace roots any client may name. An
	// empty list allows any root.
	AllowedRoots []string `yaml:"allowed_roots,omitempty"`

	// RepositoryAliases maps alternate remote spellings onto one canonical
	// repository. Use it for mirrors, never to merge a fork into its upstream.
	RepositoryAliases map[string]string `yaml:"repository_aliases,omitempty"`

	Defaults   Defaults                 `yaml:"defaults"`
	Validation Validation               `yaml:"validation"`
	Clients    map[string]ClientProfile `yaml:"clients"`

	path string
}

// Defaults holds the response-shaping defaults.
type Defaults struct {
	TokenBudget           int      `yaml:"token_budget"`
	MaxEvidenceCharacters int      `yaml:"max_evidence_characters"`
	SearchLimit           int      `yaml:"search_limit"`
	ContextTTL            Duration `yaml:"context_ttl"`
	MaxCandidates         int      `yaml:"max_candidates"`
}

// Validation controls freshness checking.
type Validation struct {
	// TTL bounds how long a freshness result is reused.
	TTL Duration `yaml:"ttl"`
	// Git enables revision and content-hash validation against local
	// repositories. It shells out to git, so it is opt-in.
	Git bool `yaml:"git"`
}

// ClientProfile is what one agent host is allowed to do.
//
// A profile is selected by a command-line flag, so anyone who can edit the MCP
// host configuration can pick any profile. It shapes what each host sees; it is
// not a security control. See docs/design-deltas.md.
type ClientProfile struct {
	Capabilities        []mecp.Capability `yaml:"capabilities"`
	AllowedRepositories []string          `yaml:"allowed_repositories,omitempty"`
	AllowedRoots        []string          `yaml:"allowed_roots,omitempty"`
}

// Default returns the configuration a fresh installation gets. The default
// agent profile can prepare context, search, and read verbatim evidence. It
// cannot propose, so the agent-facing process opens the database read-only.
func Default() *Config {
	return &Config{
		Principal: "local-user",
		Database:  DefaultDatabasePath(),
		AuditLog:  DefaultAuditPath(),
		Audit:     "jsonl",
		Defaults: Defaults{
			TokenBudget:           mecp.DefaultTokenBudget,
			MaxEvidenceCharacters: 2000,
			SearchLimit:           8,
			ContextTTL:            Duration(time.Hour),
			MaxCandidates:         500,
		},
		Validation: Validation{TTL: Duration(15 * time.Minute)},
		Clients: map[string]ClientProfile{
			DefaultClientID: {
				Capabilities: []mecp.Capability{
					mecp.CapPrepare,
					mecp.CapSearch,
					mecp.CapEvidence,
				},
			},
		},
	}
}

// Load reads configuration from path. An empty path uses DefaultConfigPath,
// and a missing file at that default yields the built-in defaults so that the
// tool works before the user has written any configuration.
func Load(path string) (*Config, error) {
	explicit := path != ""
	if path == "" {
		path = DefaultConfigPath()
	}

	buf, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			cfg := Default()
			cfg.path = path
			return cfg, nil
		}
		return nil, fmt.Errorf(`failed to read configuration %s: %w`, path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(buf, cfg); err != nil {
		return nil, fmt.Errorf(`failed to parse configuration %s: %w`, path, err)
	}
	cfg.path = path

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Path reports where the configuration was read from.
func (c *Config) Path() string { return c.path }

// Save writes the configuration to path with owner-only permissions.
func (c *Config) Save(path string) error {
	if path == "" {
		path = c.path
	}
	if path == "" {
		path = DefaultConfigPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf(`failed to create configuration directory: %w`, err)
	}
	buf, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf(`failed to encode configuration: %w`, err)
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return fmt.Errorf(`failed to write configuration %s: %w`, path, err)
	}
	c.path = path
	return nil
}

// Validate rejects a configuration that would grant more than it names.
func (c *Config) Validate() error {
	if c.Principal == "" {
		return fmt.Errorf(`configuration must name a principal`)
	}
	if c.Database == "" {
		return fmt.Errorf(`configuration must name a database path`)
	}
	switch c.Audit {
	case "", "none", "jsonl", "sqlite":
	default:
		return fmt.Errorf(`unknown audit sink %q; use "jsonl", "sqlite", or "none"`, c.Audit)
	}
	for name, profile := range c.Clients {
		for _, cap := range profile.Capabilities {
			if !cap.Valid() {
				return fmt.Errorf(`client profile %q declares unknown capability %q`, name, cap)
			}
		}
	}
	return nil
}

// Caller resolves a client identifier into an authorized caller. An
// unrecognized client falls back to the default profile, and when no default
// profile exists the caller receives no capabilities, which every service
// operation then refuses.
func (c *Config) Caller(clientID string) mecp.Caller {
	if clientID == "" {
		clientID = DefaultClientID
	}
	profile, ok := c.Clients[clientID]
	if !ok {
		profile = c.Clients[DefaultClientID]
	}

	roots := profile.AllowedRoots
	if len(roots) == 0 {
		roots = c.AllowedRoots
	}

	return mecp.Caller{
		PrincipalID:         c.Principal,
		ClientID:            clientID,
		Capabilities:        profile.Capabilities,
		AllowedRepositories: profile.AllowedRepositories,
		AllowedRoots:        roots,
	}
}

// AdminCaller is the identity the administrative CLI uses. It is not reachable
// from any agent-facing transport.
func (c *Config) AdminCaller() mecp.Caller {
	return mecp.Caller{
		PrincipalID:  c.Principal,
		ClientID:     "contextctl",
		Capabilities: []mecp.Capability{mecp.CapAdmin},
	}
}

// DefaultConfigPath returns the platform configuration file location.
func DefaultConfigPath() string {
	return filepath.Join(baseDir("XDG_CONFIG_HOME", ".config"), AppName, "config.yaml")
}

// DefaultDatabasePath returns the platform database location.
func DefaultDatabasePath() string {
	return filepath.Join(baseDir("XDG_DATA_HOME", filepath.Join(".local", "share")), AppName, "context.db")
}

// DefaultAuditPath returns the platform audit log location.
func DefaultAuditPath() string {
	return filepath.Join(baseDir("XDG_STATE_HOME", filepath.Join(".local", "state")), AppName, "audit.jsonl")
}

func baseDir(envVar, fallback string) string {
	if dir := os.Getenv(envVar); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, fallback)
}
