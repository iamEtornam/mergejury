package packet

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git shells out to the git binary. We deliberately do not embed a git
// library: worktrees and the user's local git config behave better through
// the binary.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// LocalDiff returns the unified diff of the working tree against baseRef.
func LocalDiff(ctx context.Context, repoDir, baseRef string) (string, error) {
	return Git(ctx, repoDir, "diff", "--no-color", "--no-ext-diff", baseRef, "--")
}

// Worktrees manages one read-only-by-intent worktree per adapter under a
// run-scoped temp directory.
type Worktrees struct {
	RepoDir string
	BaseDir string
	paths   map[string]string
}

func NewWorktrees(ctx context.Context, repoDir string) (*Worktrees, error) {
	// Recover from crashed runs before adding new trees.
	_, _ = Git(ctx, repoDir, "worktree", "prune")
	base, err := os.MkdirTemp("", "revu-worktrees-*")
	if err != nil {
		return nil, err
	}
	return &Worktrees{RepoDir: repoDir, BaseDir: base, paths: map[string]string{}}, nil
}

// Add creates a detached worktree at sha for the named adapter.
func (w *Worktrees) Add(ctx context.Context, adapterID, sha string) (string, error) {
	dest := filepath.Join(w.BaseDir, adapterID)
	if _, err := Git(ctx, w.RepoDir, "worktree", "add", "--detach", dest, sha); err != nil {
		return "", err
	}
	w.paths[adapterID] = dest
	return dest, nil
}

// AssertClean fails loudly if an adapter dirtied its worktree: that is a bug
// in the adapter's permission config, not something to clean up silently.
func (w *Worktrees) AssertClean(ctx context.Context, adapterID string) error {
	dir, ok := w.paths[adapterID]
	if !ok {
		return nil
	}
	out, err := Git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("adapter %s dirtied its worktree (permissions misconfigured):\n%s", adapterID, out)
	}
	return nil
}

// Cleanup removes all worktrees. Call it deferred so it runs even on panic.
func (w *Worktrees) Cleanup() {
	ctx := context.Background()
	for _, dir := range w.paths {
		_, _ = Git(ctx, w.RepoDir, "worktree", "remove", "--force", dir)
	}
	_ = os.RemoveAll(w.BaseDir)
	_, _ = Git(ctx, w.RepoDir, "worktree", "prune")
}

// EnsureCommit fetches sha from origin if it is not already present locally.
func EnsureCommit(ctx context.Context, repoDir, sha string) error {
	if _, err := Git(ctx, repoDir, "cat-file", "-e", sha+"^{commit}"); err == nil {
		return nil
	}
	if _, err := Git(ctx, repoDir, "fetch", "origin", sha); err != nil {
		return fmt.Errorf("fetch %s: %w", sha, err)
	}
	return nil
}

// FileAtSHA returns a file's content at the given SHA (or from the working
// tree when sha is empty).
func FileAtSHA(ctx context.Context, repoDir, sha, path string) (string, error) {
	if sha == "" {
		b, err := os.ReadFile(filepath.Join(repoDir, path))
		return string(b), err
	}
	return Git(ctx, repoDir, "show", sha+":"+path)
}
