package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lestrrat-ai/mecp"
)

// maxDocumentBytes bounds an instruction file. Anything larger is not a rules
// document, and reading it whole into memory on a caller's say-so is not
// something this should do.
const maxDocumentBytes = 4 << 20

// DocumentReader reads instruction documents from the filesystem, and only from
// inside the roots it was given. It stores nothing; the name says read because
// reading is all it does.
//
// The restriction is the point. ExtractRules reports whether a quote appears in
// a named file, and that answer is enough to read a file back a piece at a time
// by guessing. Confining reads to roots the user chose keeps that from being a
// way to search the whole disk. A store with no roots reads nothing.
type DocumentReader struct {
	roots []string
}

// NewDocumentReader returns a reader confined to roots. Each root is resolved
// once, so a symlink swapped in later cannot widen what is readable.
func NewDocumentReader(roots []string) *DocumentReader {
	resolved := make([]string, 0, len(roots))
	for _, root := range roots {
		root = expandHome(strings.TrimSpace(root))
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
		resolved = append(resolved, abs)
	}
	return &DocumentReader{roots: resolved}
}

// Roots reports which directories the reader may read from.
func (r *DocumentReader) Roots() []string { return append([]string(nil), r.roots...) }

// Read returns a document, refusing any path outside the configured roots.
func (r *DocumentReader) Read(_ context.Context, path string) (*mecp.Document, error) {
	if len(r.roots) == 0 {
		return nil, fmt.Errorf(`no document roots are configured, so no instruction file may be read`)
	}

	abs, err := r.resolve(path)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(`document %s does not exist`, path)
		}
		return nil, fmt.Errorf(`failed to read %s: %w`, path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf(`%s is a directory, not a document`, path)
	}
	if info.Size() > maxDocumentBytes {
		return nil, fmt.Errorf(`%s is %d bytes, larger than the %d-byte limit for an instruction document`,
			path, info.Size(), maxDocumentBytes)
	}

	buf, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf(`failed to read %s: %w`, path, err)
	}

	content := string(buf)
	return &mecp.Document{Path: abs, Content: content, ContentHash: mecp.HashContent(content)}, nil
}

// resolve turns a caller-supplied path into an absolute one inside a root, or
// refuses it. Symlinks are followed before the check, so a link inside a root
// pointing out of it is caught rather than followed.
func (r *DocumentReader) resolve(path string) (string, error) {
	path = expandHome(strings.TrimPrefix(strings.TrimSpace(path), "file://"))
	if path == "" {
		return "", fmt.Errorf(`no document path was given`)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf(`failed to resolve %s: %w`, path, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}

	for _, root := range r.roots {
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return abs, nil
		}
	}
	return "", fmt.Errorf(`%s is outside every configured document root (%s)`,
		path, strings.Join(r.roots, ", "))
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

var _ mecp.DocumentReader = (*DocumentReader)(nil)
