package examples_test

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lestrrat-ai/mecp/source"
)

// Example_source_import_files shows how curated Markdown and YAML files become
// records. An importer never grants explicit_user authority on its own: the
// most it will claim is that the material came from a source it read.
func Example_source_import_files() {
	dir, err := os.MkdirTemp("", ".tmp-import-files-*")
	if err != nil {
		fmt.Printf("failed to create a temporary directory: %s\n", err)
		return
	}
	defer os.RemoveAll(dir)

	// A YAML file may hold one record, a list, or a "records:" mapping.
	yamlDoc := `
kind: decision
subject: release conformance testing
statement: The conformance suite runs against a controlled commit before a release.
rationale: Reproducibility comes from choosing the commit at release time.
scope:
  repository: git@github.com:lestrrat-go/helium.git
  task_kinds: [release]
tags: [conformance, release]
`
	if err := os.WriteFile(filepath.Join(dir, "conformance.yaml"), []byte(yamlDoc), 0o600); err != nil {
		fmt.Printf("failed to write the YAML record: %s\n", err)
		return
	}

	// A Markdown file carries front matter, a body statement, and an optional
	// "## Rationale" section.
	markdownDoc := `---
kind: constraint
subject: untrusted stylesheets
scope:
  repository: https://github.com/lestrrat-go/helium
---

Untrusted XSLT stylesheets must never be executed during parsing.

## Rationale

Stylesheet execution is a remote code execution primitive.
`
	if err := os.WriteFile(filepath.Join(dir, "stylesheets.md"), []byte(markdownDoc), 0o600); err != nil {
		fmt.Printf("failed to write the Markdown record: %s\n", err)
		return
	}

	importer := source.NewFileImporter("local-user")
	importer.Now = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	records, err := importer.ImportPath(dir)
	if err != nil {
		fmt.Printf("failed to import the directory: %s\n", err)
		return
	}

	for _, rec := range records {
		fmt.Printf("%s %s (%s) scope=%s sources=%d\n",
			rec.Kind, rec.Subject, rec.Authority, rec.Scope.SpecificityLabel(), len(rec.Sources))
	}
	// Output:
	// decision release conformance testing (sourced_import) scope=repository_and_task_kind sources=1
	// constraint untrusted stylesheets (sourced_import) scope=repository sources=1
}
