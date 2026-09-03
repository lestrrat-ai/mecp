package source_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lestrrat-ai/mecp"
	"github.com/lestrrat-ai/mecp/source"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestFileImporter(t *testing.T) {
	t.Run("reads a single YAML record", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "decision.yaml", `
kind: decision
subject: release conformance testing
statement: The conformance suite runs against a controlled commit before a release.
rationale: Reproducibility comes from choosing the commit at release time.
scope:
  repository: git@github.com:lestrrat-go/helium.git
  task_kinds: [release]
tags: [conformance, release]
`)

		recs, err := source.NewFileImporter("local-user").ImportFile(path)
		require.NoError(t, err)
		require.Len(t, recs, 1)

		rec := recs[0]
		require.Equal(t, mecp.KindDecision, rec.Kind)
		require.Equal(t, "https://github.com/lestrrat-go/helium", rec.Scope.Repository)
		require.Equal(t, []mecp.TaskKind{mecp.TaskRelease}, rec.Scope.TaskKinds)
		require.Equal(t, "local-user", rec.Scope.User)
	})

	t.Run("an imported record never claims explicit user authority by itself", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "note.yaml", "statement: Something the importer found.\n")

		recs, err := source.NewFileImporter("local-user").ImportFile(path)
		require.NoError(t, err)
		require.Equal(t, mecp.AuthorityImport, recs[0].Authority)
	})

	t.Run("records the file it came from with a content hash", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "note.yaml", "statement: A fact worth keeping.\n")

		recs, err := source.NewFileImporter("local-user").ImportFile(path)
		require.NoError(t, err)
		require.Len(t, recs[0].Sources, 1)

		src := recs[0].Sources[0]
		require.Equal(t, mecp.SourceFile, src.Type)
		require.Contains(t, src.Locator, "note.yaml")

		want, err := source.HashFile(path)
		require.NoError(t, err)
		require.Equal(t, want, src.ContentHash)
	})

	t.Run("reads a list and a records: envelope", func(t *testing.T) {
		dir := t.TempDir()
		list := writeFile(t, dir, "list.yaml", `
- statement: First fact.
  kind: project_fact
- statement: Second fact.
  kind: project_fact
`)
		envelope := writeFile(t, dir, "envelope.yaml", `
records:
  - statement: Third fact.
    kind: project_fact
`)

		imp := source.NewFileImporter("local-user")
		fromList, err := imp.ImportFile(list)
		require.NoError(t, err)
		require.Len(t, fromList, 2)

		fromEnvelope, err := imp.ImportFile(envelope)
		require.NoError(t, err)
		require.Len(t, fromEnvelope, 1)
	})

	t.Run("reads Markdown front matter with a body", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "constraint.md", `---
kind: constraint
subject: untrusted stylesheets
scope:
  repository: https://github.com/lestrrat-go/helium
---

Untrusted XSLT stylesheets must never be executed during parsing.

## Rationale

Stylesheet execution is a remote code execution primitive.
`)

		recs, err := source.NewFileImporter("local-user").ImportFile(path)
		require.NoError(t, err)
		require.Len(t, recs, 1)
		require.Equal(t, mecp.KindConstraint, recs[0].Kind)
		require.Equal(t, "Untrusted XSLT stylesheets must never be executed during parsing.", recs[0].Statement)
		require.Equal(t, "Stylesheet execution is a remote code execution primitive.", recs[0].Rationale)
	})

	t.Run("walks a directory in a stable order and skips other files", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "a.yaml", "statement: First.\nsubject: a\n")
		writeFile(t, dir, "b.md", "---\nsubject: b\n---\n\nSecond.\n")
		writeFile(t, dir, "README.txt", "not a record")
		writeFile(t, dir, "nested/c.yaml", "statement: Third.\nsubject: c\n")

		recs, err := source.NewFileImporter("local-user").ImportPath(dir)
		require.NoError(t, err)
		require.Len(t, recs, 3)

		var subjects []string
		for _, rec := range recs {
			subjects = append(subjects, rec.Subject)
		}
		require.Equal(t, []string{"a", "b", "c"}, subjects)
	})

	t.Run("prose Markdown in a records directory is not a record", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "README.md", "# Seed records\n\nHow to use this directory.\n")
		writeFile(t, dir, "real.yaml", "statement: A real record.\nsubject: real\n")

		recs, err := source.NewFileImporter("local-user").ImportPath(dir)
		require.NoError(t, err)
		require.Len(t, recs, 1)
		require.Equal(t, "real", recs[0].Subject)
	})

	t.Run("naming a front-matterless Markdown file explicitly still imports it", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "note.md", "A statement with no front matter.\n")

		recs, err := source.NewFileImporter("local-user").ImportFile(path)
		require.NoError(t, err)
		require.Len(t, recs, 1)
		require.Equal(t, "A statement with no front matter.", recs[0].Statement)
	})

	t.Run("reports a file it cannot parse", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "broken.md", "---\nkind: decision\nno closing fence\n")

		_, err := source.NewFileImporter("local-user").ImportFile(path)
		require.Error(t, err)
		require.Contains(t, err.Error(), "front matter")
	})

	t.Run("rejects a record with an unknown kind", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "bad.yaml", "kind: gossip\nstatement: Something.\n")

		_, err := source.NewFileImporter("local-user").ImportFile(path)
		require.Error(t, err)
		require.Contains(t, err.Error(), "gossip")
	})
}

func TestContainedPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "docs/adr.md", "content")

	t.Run("accepts a path inside the root", func(t *testing.T) {
		got, err := source.ContainedPath(root, filepath.Join(root, "docs/adr.md"))
		require.NoError(t, err)
		require.Contains(t, got, "adr.md")
	})

	t.Run("rejects a traversal out of the root", func(t *testing.T) {
		_, err := source.ContainedPath(root, filepath.Join(root, "..", "..", "etc", "passwd"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "escapes")
	})

	t.Run("rejects a symlink pointing out of the root", func(t *testing.T) {
		outside := t.TempDir()
		writeFile(t, outside, "secret.txt", "secret")
		link := filepath.Join(root, "escape")
		require.NoError(t, os.Symlink(outside, link))

		_, err := source.ContainedPath(root, filepath.Join(link, "secret.txt"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "escapes")
	})
}

// testTime is the fixed clock the source tests validate against.
func testTime() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }
