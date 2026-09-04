package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/config"
	"github.com/lestrrat-ai/mecp/sqlite"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

// writeTestConfig writes a config.yaml whose configured database is
// deliberately a different file from the one each test points --database at,
// so a record landing in the configured database (instead of the one named on
// the command line) is easy to catch.
func writeTestConfig(t *testing.T, path, configuredDatabase string) {
	t.Helper()
	cfg := config.Default()
	cfg.Principal = "test-user"
	cfg.Database = configuredDatabase
	cfg.Audit = "none"
	require.NoError(t, cfg.Save(path))
}

// statementsIn reads back every record's statement from the database at path,
// which is how these tests confirm which file a command actually wrote to.
func statementsIn(t *testing.T, path string) []string {
	t.Helper()
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()

	recs, err := store.QueryRecords(t.Context(), mecp.RecordQuery{})
	require.NoError(t, err)

	statements := make([]string, len(recs))
	for i, rec := range recs {
		statements[i] = rec.Statement
	}
	return statements
}

// TestDatabaseFlagPosition covers the bug: --database (and --config) are
// documented as global flags, shown under GLOBAL OPTIONS in "mecp --help" at
// every command depth, but were previously only wired into each leaf
// subcommand's own flag set. A value given before the subcommand landed on
// the root command's flag set instead, which the subcommand never consulted,
// so it was silently ignored and the configured database was used instead.
func TestDatabaseFlagPosition(t *testing.T) {
	t.Run("before the subcommand", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		configuredDB := filepath.Join(dir, "configured.db")
		scratchDB := filepath.Join(dir, "scratch.db")
		writeTestConfig(t, cfgPath, configuredDB)

		err := rootCommand().Run(t.Context(), []string{
			"mecp", "--config", cfgPath, "--database", scratchDB,
			"record", "add", "recorded before the subcommand",
		})
		require.NoError(t, err)

		require.NoFileExists(t, configuredDB,
			"the configured database must not have been touched")
		require.Contains(t, statementsIn(t, scratchDB), "recorded before the subcommand")
	})

	t.Run("after the subcommand", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		configuredDB := filepath.Join(dir, "configured.db")
		scratchDB := filepath.Join(dir, "scratch.db")
		writeTestConfig(t, cfgPath, configuredDB)

		err := rootCommand().Run(t.Context(), []string{
			"mecp", "--config", cfgPath,
			"record", "add", "--database", scratchDB, "recorded after the subcommand",
		})
		require.NoError(t, err)

		require.NoFileExists(t, configuredDB,
			"the configured database must not have been touched")
		require.Contains(t, statementsIn(t, scratchDB), "recorded after the subcommand")
	})

	t.Run("MECP_DATABASE environment variable", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		configuredDB := filepath.Join(dir, "configured.db")
		scratchDB := filepath.Join(dir, "scratch.db")
		writeTestConfig(t, cfgPath, configuredDB)

		t.Setenv("MECP_DATABASE", scratchDB)

		err := rootCommand().Run(t.Context(), []string{
			"mecp", "--config", cfgPath,
			"record", "add", "recorded via environment variable",
		})
		require.NoError(t, err)

		require.NoFileExists(t, configuredDB,
			"the configured database must not have been touched")
		require.Contains(t, statementsIn(t, scratchDB), "recorded via environment variable")
	})

	t.Run("MECP_CONFIG environment variable", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		configuredDB := filepath.Join(dir, "configured.db")
		writeTestConfig(t, cfgPath, configuredDB)

		t.Setenv("MECP_CONFIG", cfgPath)

		err := rootCommand().Run(t.Context(), []string{
			"mecp", "record", "add", "recorded via MECP_CONFIG",
		})
		require.NoError(t, err)

		require.Contains(t, statementsIn(t, configuredDB), "recorded via MECP_CONFIG")
	})

	t.Run("no database given falls back to the configured path", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		configuredDB := filepath.Join(dir, "configured.db")
		writeTestConfig(t, cfgPath, configuredDB)

		err := rootCommand().Run(t.Context(), []string{
			"mecp", "--config", cfgPath, "record", "add", "recorded with no override",
		})
		require.NoError(t, err)

		require.Contains(t, statementsIn(t, configuredDB), "recorded with no override")
	})
}

// TestGlobalFlagsHelpShowsInheritance confirms the help text a subcommand
// prints matches what it accepts: --database and --config are billed as
// GLOBAL OPTIONS even for a command several levels below root, because they
// are the root command's own (non-local) flags rather than copies redeclared
// on each subcommand.
func TestGlobalFlagsHelpShowsInheritance(t *testing.T) {
	root := rootCommand()
	var out strings.Builder
	root.Writer = &out

	// Running "--help" is what actually wires up parent pointers between
	// commands (they are literal struct nesting until Run walks the tree), so
	// this both renders the help text and puts the command tree into the
	// state VisiblePersistentFlags relies on.
	require.NoError(t, root.Run(t.Context(), []string{"mecp", "record", "list", "--help"}))
	require.Contains(t, out.String(), "GLOBAL OPTIONS:")
	require.Contains(t, out.String(), "--database")
	require.Contains(t, out.String(), "--config")

	leaf := root.Command("record").Command("list")
	require.NotNil(t, leaf)

	// A subcommand must not redeclare them locally: a same-named local flag
	// would shadow the shared, persistent instance and reintroduce the bug.
	for _, f := range leaf.Flags {
		for _, n := range f.Names() {
			require.NotEqual(t, "database", n, "record list must not redeclare --database locally")
			require.NotEqual(t, "config", n, "record list must not redeclare --config locally")
		}
	}
}

// TestGlobalFlagsAreNotLocal guards the mechanism the fix relies on: if
// globalFlags ever gained a Local flag (or Local: true on config/database),
// urfave/cli would stop treating them as inherited and the original bug would
// come back silently.
func TestGlobalFlagsAreNotLocal(t *testing.T) {
	for _, f := range globalFlags() {
		local, ok := f.(interface{ IsLocal() bool })
		require.True(t, ok, "%v must implement LocalFlag", f.Names())
		require.False(t, local.IsLocal(), "%v must not be Local, or it would stop being inherited", f.Names())
	}
}

// TestNoSubcommandRedeclaresGlobalFlags walks every command in the real tree
// and fails if any non-root command redeclares "database" or "config" among
// its own Flags. That is exactly the shape of the original bug: a same-named
// local flag shadows the inherited, shared one instead of sharing it, so a
// value given at a different position is silently lost.
func TestNoSubcommandRedeclaresGlobalFlags(t *testing.T) {
	root := rootCommand()
	err := root.Walk(func(cmd *cli.Command) error {
		if cmd == root {
			return nil
		}
		for _, f := range cmd.Flags {
			for _, n := range f.Names() {
				require.NotEqual(t, "database", n, "%s must not redeclare --database locally", cmd.FullName())
				require.NotEqual(t, "config", n, "%s must not redeclare --config locally", cmd.FullName())
			}
		}
		return nil
	})
	require.NoError(t, err)
}
