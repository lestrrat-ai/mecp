package source

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/lestrrat-ai/mecp"
)

// FileImporter reads records from YAML and Markdown files.
//
// Two shapes are accepted. A YAML file holds one record, a list of records, or
// a mapping with a "records" key. A Markdown file holds YAML front matter
// between "---" fences, with the body as the statement and an optional
// "## Rationale" section.
//
// Imported records default to sourced_import authority. An adapter never
// assigns explicit_user authority on its own: that is a judgement only the
// user can make.
type FileImporter struct {
	// DefaultAuthority is applied to records that do not declare one.
	DefaultAuthority mecp.Authority
	// DefaultSensitivity is applied to records that do not declare one.
	DefaultSensitivity mecp.Sensitivity
	// Principal is written into scopes that do not name one, so imported files
	// cannot silently become another principal's context.
	Principal string
	// Now supplies timestamps. Zero means time.Now.
	Now time.Time
}

// NewFileImporter returns an importer with the conservative defaults.
func NewFileImporter(principal string) *FileImporter {
	return &FileImporter{
		DefaultAuthority:   mecp.AuthorityImport,
		DefaultSensitivity: mecp.SensitivityProject,
		Principal:          principal,
	}
}

// ImportPath reads one file, or every .yaml, .yml, and .md file under a
// directory. Files are visited in lexical order so that repeated imports
// produce the same result.
func (imp *FileImporter) ImportPath(path string) ([]*mecp.Record, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf(`failed to read %s: %w`, path, err)
	}
	if !info.IsDir() {
		return imp.ImportFile(path)
	}

	var out []*mecp.Record
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != path {
				return fs.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".yaml", ".yml":
		case ".md":
			// A directory of records normally also holds ordinary prose: a
			// README, notes, an ADR that is referenced rather than imported.
			// Front matter is what marks a Markdown file as a record, so a file
			// without it is skipped here. Naming the file explicitly still
			// imports it.
			ok, err := hasFrontMatter(p)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		default:
			return nil
		}
		recs, err := imp.ImportFile(p)
		if err != nil {
			return err
		}
		out = append(out, recs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ImportFile reads one file.
func (imp *FileImporter) ImportFile(path string) ([]*mecp.Record, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(`failed to read %s: %w`, path, err)
	}

	var recs []*mecp.Record
	if strings.EqualFold(filepath.Ext(path), ".md") {
		rec, err := parseMarkdown(buf)
		if err != nil {
			return nil, fmt.Errorf(`failed to parse %s: %w`, path, err)
		}
		recs = []*mecp.Record{rec}
	} else {
		recs, err = parseYAML(buf)
		if err != nil {
			return nil, fmt.Errorf(`failed to parse %s: %w`, path, err)
		}
	}

	hash, err := HashFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	now := imp.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	for _, rec := range recs {
		imp.applyDefaults(rec, abs, hash, now)
		if err := rec.Validate(); err != nil {
			return nil, fmt.Errorf(`%s: %w`, path, err)
		}
	}
	return recs, nil
}

func (imp *FileImporter) applyDefaults(rec *mecp.Record, path, hash string, now time.Time) {
	if rec.ID == "" {
		rec.ID = mecp.NewID("rec")
	}
	if rec.Kind == "" {
		rec.Kind = mecp.KindProjectFact
	}
	if rec.Authority == "" {
		rec.Authority = imp.DefaultAuthority
	}
	if rec.Sensitivity == "" {
		rec.Sensitivity = imp.DefaultSensitivity
	}
	if rec.Subject == "" {
		rec.Subject = deriveSubject(rec.Statement)
	}
	if rec.Scope.User == "" && imp.Principal != "" {
		rec.Scope.User = imp.Principal
	}

	// Every imported record carries the file it came from, so a later
	// content-hash validation can tell whether the file has since changed.
	if !hasFileSource(rec, path) {
		rec.Sources = append(rec.Sources, mecp.Source{
			ID:          mecp.NewID("src"),
			Type:        mecp.SourceFile,
			Locator:     "file://" + path,
			ContentHash: hash,
			CapturedAt:  now,
		})
	}
	rec.Normalize(now)
}

func hasFileSource(rec *mecp.Record, path string) bool {
	for _, src := range rec.Sources {
		if strings.TrimPrefix(src.Locator, "file://") == path {
			return true
		}
	}
	return false
}

// recordFile is the YAML envelope accepted by ImportFile.
type recordFile struct {
	Records []*mecp.Record `yaml:"records"`
}

func parseYAML(buf []byte) ([]*mecp.Record, error) {
	var envelope recordFile
	if err := yaml.Unmarshal(buf, &envelope); err == nil && len(envelope.Records) > 0 {
		return envelope.Records, nil
	}

	var list []*mecp.Record
	if err := yaml.Unmarshal(buf, &list); err == nil && len(list) > 0 {
		return list, nil
	}

	var single mecp.Record
	if err := yaml.Unmarshal(buf, &single); err != nil {
		return nil, err
	}
	if single.Statement == "" {
		return nil, fmt.Errorf(`no records found; expected a record, a list of records, or a "records:" key`)
	}
	return []*mecp.Record{&single}, nil
}

// parseMarkdown reads YAML front matter plus a body. The body up to an
// "## Rationale" heading is the statement; anything under that heading is the
// rationale.
func parseMarkdown(buf []byte) (*mecp.Record, error) {
	front, body, err := splitFrontMatter(buf)
	if err != nil {
		return nil, err
	}

	var rec mecp.Record
	if len(front) > 0 {
		if err := yaml.Unmarshal(front, &rec); err != nil {
			return nil, fmt.Errorf(`failed to parse front matter: %w`, err)
		}
	}

	statement, rationale := splitRationale(string(body))
	if rec.Statement == "" {
		rec.Statement = statement
	}
	if rec.Rationale == "" {
		rec.Rationale = rationale
	}
	if rec.Statement == "" {
		return nil, fmt.Errorf(`file has no statement: put it in the body or in a "statement" front-matter key`)
	}
	return &rec, nil
}

var frontMatterFence = []byte("---")

func splitFrontMatter(buf []byte) ([]byte, []byte, error) {
	trimmed := bytes.TrimLeft(buf, " \t\r\n")
	if !bytes.HasPrefix(trimmed, frontMatterFence) {
		return nil, buf, nil
	}

	rest := trimmed[len(frontMatterFence):]
	rest = bytes.TrimLeft(rest, "\r")
	if !bytes.HasPrefix(rest, []byte("\n")) {
		return nil, buf, nil
	}
	rest = rest[1:]

	idx := bytes.Index(rest, []byte("\n---"))
	if idx < 0 {
		return nil, nil, fmt.Errorf(`front matter is not closed with "---"`)
	}
	front := rest[:idx]
	body := rest[idx+len("\n---"):]
	if i := bytes.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = nil
	}
	return front, body, nil
}

func splitRationale(body string) (string, string) {
	lines := strings.Split(body, "\n")
	var statement, rationale []string
	inRationale := false
	for _, line := range lines {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "## rationale") {
			inRationale = true
			continue
		}
		if inRationale {
			rationale = append(rationale, line)
			continue
		}
		statement = append(statement, line)
	}
	return strings.TrimSpace(strings.Join(statement, "\n")), strings.TrimSpace(strings.Join(rationale, "\n"))
}

func deriveSubject(statement string) string {
	statement = strings.TrimSpace(statement)
	if idx := strings.IndexAny(statement, ".;\n"); idx > 0 {
		statement = statement[:idx]
	}
	words := strings.Fields(statement)
	if len(words) > 12 {
		words = words[:12]
	}
	return strings.Join(words, " ")
}

// hasFrontMatter reports whether a Markdown file opens with a YAML front-matter
// fence.
func hasFrontMatter(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf(`failed to read %s: %w`, path, err)
	}
	defer f.Close()

	var head [8]byte
	n, err := f.Read(head[:])
	if err != nil && n == 0 {
		return false, nil
	}
	trimmed := bytes.TrimLeft(head[:n], " \t\r\n")
	return bytes.HasPrefix(trimmed, frontMatterFence), nil
}
