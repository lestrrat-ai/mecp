// Package source holds the adapters that connect mecp to the material records
// are made from: files on disk, Git repositories, and portable JSONL exports.
package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lestrrat-ai/mecp"
)

// ErrUnverifiable means a source cannot be checked from the local machine, for
// example a conversation locator. It becomes ValidationUnverified rather than a
// failure, because "I could not check" is not the same claim as "this is false".
var ErrUnverifiable = errors.New("source cannot be verified locally")

// GitResolver validates file and revision evidence against the local
// workspace. It shells out to git rather than linking a Git implementation,
// which keeps the dependency surface small at the cost of requiring git on the
// PATH.
type GitResolver struct {
	gitPath string
	timeout time.Duration
	// gitEnabled allows the policies that shell out. File existence and content
	// hashing never do, so they run either way.
	gitEnabled bool
	// extraRoots are directories a source may live in besides the workspace.
	// A record extracted from an instruction file points at that file wherever
	// it is, and validating it from inside some repository must not be refused
	// just because the document sits elsewhere.
	extraRoots []string
}

// GitOption configures NewGitResolver.
type GitOption func(*GitResolver)

// WithGitPath overrides the git executable.
func WithGitPath(path string) GitOption { return func(r *GitResolver) { r.gitPath = path } }

// WithGitTimeout bounds how long a single git invocation may take.
func WithGitTimeout(d time.Duration) GitOption { return func(r *GitResolver) { r.timeout = d } }

// WithAllowedRoots names directories, besides the workspace, whose files may be
// validated. Pass the same document roots the reader was given.
func WithAllowedRoots(roots []string) GitOption {
	return func(r *GitResolver) { r.extraRoots = append(r.extraRoots, roots...) }
}

// WithGitEnabled allows the policies that run git. With it off, those report
// that they could not check rather than failing, and the file and hash checks
// carry on unaffected.
func WithGitEnabled(v bool) GitOption { return func(r *GitResolver) { r.gitEnabled = v } }

// NewGitResolver returns a resolver backed by the git executable.
func NewGitResolver(options ...GitOption) *GitResolver {
	r := &GitResolver{gitPath: "git", timeout: 10 * time.Second, gitEnabled: true}
	for _, opt := range options {
		opt(r)
	}
	return r
}

// Exists reports whether the artifact a source points at is still present.
func (r *GitResolver) Exists(ctx context.Context, src mecp.Source, ws mecp.Workspace) (bool, error) {
	switch src.Type {
	case mecp.SourceFile, mecp.SourceADR, mecp.SourceNote:
		path, err := r.resolvePath(src.Locator, ws)
		if err != nil {
			return false, err
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, fmt.Errorf(`failed to stat %s: %w`, path, err)
		}
		return !info.IsDir(), nil

	case mecp.SourceCommit:
		if !r.gitEnabled {
			return false, ErrUnverifiable
		}
		root, err := workspaceRoot(ws)
		if err != nil {
			return false, err
		}
		rev := src.Revision
		if rev == "" {
			rev = strings.TrimPrefix(src.Locator, "commit://")
		}
		if rev == "" {
			return false, ErrUnverifiable
		}
		_, err = r.git(ctx, root, "cat-file", "-e", rev+"^{commit}")
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return false, nil
			}
			return false, err
		}
		return true, nil

	default:
		return false, ErrUnverifiable
	}
}

// ContentHash returns the current hash of a file source in the same
// "sha256:<hex>" form stored on the source.
func (r *GitResolver) ContentHash(_ context.Context, src mecp.Source, ws mecp.Workspace) (string, error) {
	path, err := r.resolvePath(src.Locator, ws)
	if err != nil {
		return "", err
	}
	return HashFile(path)
}

// RevisionApplies reports whether the source's revision is an ancestor of, or
// equal to, the workspace revision. A decision recorded against a commit that
// is not in the current history describes a different line of development.
func (r *GitResolver) RevisionApplies(ctx context.Context, src mecp.Source, ws mecp.Workspace) (bool, error) {
	if !r.gitEnabled || src.Revision == "" || ws.Revision == "" {
		return false, ErrUnverifiable
	}
	root, err := workspaceRoot(ws)
	if err != nil {
		return false, err
	}
	if _, err := r.git(ctx, root, "merge-base", "--is-ancestor", src.Revision, ws.Revision); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *GitResolver) git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.gitPath, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolvePath turns a source locator into an absolute path inside the
// workspace root, refusing anything that escapes it. Without this check, a
// crafted locator could hash or read files anywhere the process can reach.
func (r *GitResolver) resolvePath(locator string, ws mecp.Workspace) (string, error) {
	path := strings.TrimPrefix(locator, "file://")
	if path == "" {
		return "", ErrUnverifiable
	}

	// A document the user named in configuration is already vetted, so an
	// absolute path inside one of those roots is readable wherever the caller
	// happens to be working.
	if filepath.IsAbs(path) {
		for _, root := range r.extraRoots {
			if contained, err := ContainedPath(expandHome(root), path); err == nil {
				return contained, nil
			}
		}
	}

	root, err := workspaceRoot(ws)
	if err != nil {
		if filepath.IsAbs(path) {
			return path, nil
		}
		return "", err
	}

	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return ContainedPath(root, path)
}

// ContainedPath verifies that path resolves inside root, following symlinks so
// that a link out of the workspace is caught rather than followed.
func ContainedPath(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// The file itself may not exist yet, so resolve the deepest existing
	// ancestor instead of failing outright.
	checked := abs
	for {
		if resolved, err := filepath.EvalSymlinks(checked); err == nil {
			abs = filepath.Join(resolved, strings.TrimPrefix(abs, checked))
			break
		}
		parent := filepath.Dir(checked)
		if parent == checked {
			break
		}
		checked = parent
	}

	if abs != absRoot && !strings.HasPrefix(abs, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf(`path %s escapes the workspace root %s`, path, root)
	}
	return abs, nil
}

// HashFile returns the "sha256:<hex>" digest of a file's contents.
func HashFile(path string) (string, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf(`failed to read %s: %w`, path, err)
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func workspaceRoot(ws mecp.Workspace) (string, error) {
	root := strings.TrimPrefix(ws.RootURI, "file://")
	if root == "" {
		return "", fmt.Errorf(`workspace supplied no root: %w`, ErrUnverifiable)
	}
	return root, nil
}

var _ mecp.SourceResolver = (*GitResolver)(nil)
