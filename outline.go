package mecp

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Distiller turns a Markdown instruction document into candidate records.
//
// It is deliberately mechanical. Splitting prose into rules is judgement, and a
// parser that guessed would produce records nobody could trust; instead this
// takes the structure the document already has. A bullet is a rule. A table row
// with a trigger is a rule. A heading is the subject those rules belong to.
// Prose paragraphs are counted and skipped rather than guessed at, so the caller
// knows what was left behind.
//
// Every produced record carries the original text verbatim as evidence, so the
// reviewer can see what the parser changed. Nothing is activated: the output is
// meant to be edited and then imported.
type Distiller struct {
	// Principal owns the produced records.
	Principal string
	// Authority is what the produced records claim. It defaults to
	// sourced_import, because a parser cannot know whether the reader wrote the
	// document it is reading.
	Authority Authority
	// Scope is applied to every record from this document. One document
	// normally covers one area, so a uniform scope is usually right, and
	// guessing per-rule scope from prose is not.
	Scope Scope
	// Now supplies timestamps. Zero means time.Now.
	Now time.Time
}

// DistillResult is what one document produced, including what was skipped.
type DistillResult struct {
	Records []*Record
	// Lines gives the line each record came from, in the same order.
	Lines []int
	// Sections is how many headings held at least one rule.
	Sections int
	// SkippedParagraphs counts prose the parser did not try to interpret.
	SkippedParagraphs int
	// SkippedLines names, by line number, the prose it skipped, so a reviewer
	// can check whether anything important was left behind.
	SkippedLines []int
}

// NewDistiller returns a distiller with conservative defaults.
func NewDistiller(principal string) *Distiller {
	return &Distiller{Principal: principal, Authority: AuthorityImport}
}

// Distill reads one Markdown document from disk.
func (d *Distiller) Distill(path string) (*DistillResult, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(`failed to read %s: %w`, path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return d.DistillContent(string(buf), abs, HashContent(string(buf))), nil
}

// DistillContent reads a document already in memory, which is what the
// extraction coverage check uses so that reading the file twice is unnecessary.
func (d *Distiller) DistillContent(content, path, hash string) *DistillResult {
	now := d.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	p := &distillParser{
		distiller: d,
		locator:   "file://" + path,
		hash:      hash,
		now:       now,
		docTag:    tagFromFilename(path),
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		p.line++
		p.consume(scanner.Text())
	}
	p.flushBullet()

	return p.result()
}

type distillParser struct {
	distiller *Distiller
	locator   string
	hash      string
	now       time.Time
	docTag    string

	line     int
	headings []string
	inFence  bool
	inTable  bool

	// pending accumulates a bullet across its continuation lines.
	pending     []string
	pendingLine int

	records      []*Record
	recordLines  []int
	sectionsSeen map[string]struct{}
	skippedPara  int
	skippedLines []int
	// paragraphOpen stops a multi-line prose block being counted once per line.
	paragraphOpen bool
}

var (
	headingPattern = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	// Ordered items count as rules too: a numbered procedure is a sequence of
	// instructions, not prose.
	bulletPattern   = regexp.MustCompile(`^([-*+]|\d+\.)\s+(.*)$`)
	checkboxPattern = regexp.MustCompile(`^\[[ xX]\]\s*`)
	fencePattern    = regexp.MustCompile("^```|^~~~")
	separatorRow    = regexp.MustCompile(`^\|[\s:|-]+\|?$`)
)

func (p *distillParser) consume(raw string) {
	if fencePattern.MatchString(strings.TrimSpace(raw)) {
		p.flushBullet()
		p.inFence = !p.inFence
		return
	}
	if p.inFence {
		return
	}

	trimmed := strings.TrimSpace(raw)

	if trimmed == "" {
		p.flushBullet()
		p.paragraphOpen = false
		p.inTable = false
		return
	}

	if m := headingPattern.FindStringSubmatch(trimmed); m != nil {
		p.flushBullet()
		p.paragraphOpen = false
		p.inTable = false
		p.setHeading(len(m[1]), strings.TrimSpace(m[2]))
		return
	}

	if strings.HasPrefix(trimmed, "|") {
		p.flushBullet()
		p.paragraphOpen = false
		p.consumeTableRow(trimmed)
		return
	}
	p.inTable = false

	if m := bulletPattern.FindStringSubmatch(trimmed); m != nil {
		// An indented bullet is a sub-point of the one above it, so it joins
		// rather than starting a new rule.
		if isIndented(raw) && len(p.pending) > 0 {
			p.pending = append(p.pending, strings.TrimSpace(m[2]))
			return
		}
		p.flushBullet()
		p.pending = []string{strings.TrimSpace(m[2])}
		p.pendingLine = p.line
		return
	}

	// A continuation of the bullet above.
	if len(p.pending) > 0 && isIndented(raw) {
		p.pending = append(p.pending, trimmed)
		return
	}

	p.flushBullet()
	if !p.paragraphOpen {
		p.skippedPara++
		p.skippedLines = append(p.skippedLines, p.line)
		p.paragraphOpen = true
	}
}

func isIndented(raw string) bool {
	return strings.HasPrefix(raw, "  ") || strings.HasPrefix(raw, "\t")
}

func (p *distillParser) setHeading(level int, text string) {
	for len(p.headings) < level {
		p.headings = append(p.headings, "")
	}
	p.headings = p.headings[:level]
	p.headings[level-1] = text
}

// subject is the nearest non-empty heading, which is what the rules under it are
// about.
func (p *distillParser) subject() string {
	for i := len(p.headings) - 1; i >= 0; i-- {
		if p.headings[i] != "" {
			return strings.ToLower(p.headings[i])
		}
	}
	return "general"
}

func (p *distillParser) flushBullet() {
	if len(p.pending) == 0 {
		return
	}
	text := strings.Join(p.pending, " ")
	p.pending = nil
	p.add(text, p.pendingLine)
}

// consumeTableRow reads a two-or-more column row as "subject | rule". The first
// column of your tables is consistently what the row is about and the last is
// what to do about it, which is exactly a subject and a statement.
func (p *distillParser) consumeTableRow(row string) {
	if separatorRow.MatchString(row) {
		p.inTable = true
		return
	}
	// The row before the separator is the header, so the first row of a table is
	// skipped until the separator has been seen.
	if !p.inTable {
		return
	}

	cells := splitTableRow(row)
	if len(cells) < 2 {
		return
	}
	subject := strings.TrimSpace(cells[0])
	statement := strings.TrimSpace(strings.Join(cells[1:], " — "))
	if subject == "" || statement == "" {
		return
	}
	p.addWithSubject(strings.ToLower(subject), subject+": "+statement, p.line)
}

func splitTableRow(row string) []string {
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	parts := strings.Split(row, "|")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

func (p *distillParser) add(text string, line int) {
	p.addWithSubject(p.subject(), text, line)
}

func (p *distillParser) addWithSubject(subject, text string, line int) {
	statement := normalizeStatement(text)
	if statement == "" {
		return
	}

	if p.sectionsSeen == nil {
		p.sectionsSeen = make(map[string]struct{})
	}
	p.sectionsSeen[subject] = struct{}{}

	rec := &Record{
		ID:               NewID("rec"),
		Kind:             inferKind(text),
		Subject:          subject,
		Statement:        statement,
		Scope:            p.distiller.Scope.Clone(),
		Authority:        p.distiller.Authority,
		ValidationPolicy: ValidateFileAndHash,
		Tags:             tagsFor(p.docTag),
		Sources: []Source{{
			ID:          NewID("src"),
			Type:        SourceFile,
			Locator:     p.locator,
			ContentHash: p.hash,
			// The original wording, kept verbatim so a reviewer can see what
			// the parser changed. Untrusted text: it is evidence, not an
			// instruction.
			ExactExcerpt: fmt.Sprintf("line %d: %s", line, text),
			CapturedAt:   p.now,
		}},
	}
	if rec.Scope.User == "" {
		rec.Scope.User = p.distiller.Principal
	}
	rec.Normalize(p.now)
	p.records = append(p.records, rec)
	p.recordLines = append(p.recordLines, line)
}

func (p *distillParser) result() *DistillResult {
	return &DistillResult{
		Records:           p.records,
		Lines:             p.recordLines,
		Sections:          len(p.sectionsSeen),
		SkippedParagraphs: p.skippedPara,
		SkippedLines:      p.skippedLines,
	}
}

var (
	markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	boldOrItalic = regexp.MustCompile(`\*\*([^*]*)\*\*|__([^_]*)__`)
)

// normalizeStatement turns one bullet into a sentence. Markdown decoration goes;
// backticks stay, because they carry meaning in a rule about code.
func normalizeStatement(text string) string {
	s := checkboxPattern.ReplaceAllString(strings.TrimSpace(text), "")
	s = markdownLink.ReplaceAllString(s, "$1")
	s = boldOrItalic.ReplaceAllString(s, "$1$2")
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	if !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, ":") && !strings.HasSuffix(s, "?") {
		s += "."
	}
	return s
}

// shoutedWords mark a rule the author meant as absolute. Instruction documents
// shout these deliberately, so the capitals are signal rather than noise, and
// matching them case-sensitively keeps ordinary prose from being promoted.
var shoutedWords = []string{"NEVER", "ALWAYS", "MUST", "BANNED", "MANDATORY", "ONLY", "No exceptions"}

// commandingOpeners are the ways a rule states an absolute without shouting.
// They are matched only at the start, where they govern the whole sentence,
// rather than anywhere in it where they usually describe rather than instruct.
var commandingOpeners = []string{"never ", "do not ", "don't ", "only use ", "only ", "avoid "}

func inferKind(text string) RecordKind {
	for _, w := range shoutedWords {
		if strings.Contains(text, w) {
			return KindConstraint
		}
	}
	opening := strings.ToLower(strings.TrimSpace(checkboxPattern.ReplaceAllString(strings.TrimSpace(text), "")))
	for _, w := range commandingOpeners {
		if strings.HasPrefix(opening, w) {
			return KindConstraint
		}
	}
	return KindPreference
}

func tagFromFilename(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	base = strings.ToLower(base)
	if base == "claude" || base == "agents" {
		return "agent-instructions"
	}
	return base
}

func tagsFor(docTag string) []string {
	if docTag == "" {
		return nil
	}
	return []string{docTag}
}
