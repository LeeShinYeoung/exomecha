package git

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// Worktrees sit on a detached commit, not a local branch, so no local branch can go stale or stay
// checked out in a leftover worktree. The branch name is only used to push.
type Worktree struct {
	Dir    string
	git    *Git
	branch string // empty means read-only: Push refuses
	fresh  bool   // branched off base just now, so Push overwrites an old remote branch of the same name
}

func (g *Git) sync(ctx context.Context) error {
	dir := g.repoDir()
	_, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(g.root, 0o755); err != nil {
			return err
		}
		_, err := g.runGitHub(ctx, g.root, "clone", "--no-checkout", g.url, dir)
		return err
	}
	if err != nil {
		return err
	}
	_, err = g.runGitHub(ctx, dir, "fetch", "--prune", "origin")
	return err
}

func (g *Git) open(ctx context.Context, ref string) (*Worktree, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.sync(ctx); err != nil {
		return nil, err
	}
	parent := filepath.Join(g.root, "worktrees")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(parent, "wt-")
	if err != nil {
		return nil, err
	}
	if _, err := g.run(ctx, g.repoDir(), "worktree", "add", "--detach", dir, "origin/"+ref); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &Worktree{Dir: dir, git: g}, nil
}

// Remove takes no context: it must still clean up after shutdown cancels the run.
func (w *Worktree) Remove() {
	g := w.git
	g.mu.Lock()
	defer g.mu.Unlock()
	ctx := context.Background()
	_, err := g.run(ctx, g.repoDir(), "worktree", "remove", "--force", w.Dir)
	if err == nil {
		return
	}
	slog.Warn("remove worktree failed, deleting the directory", "dir", w.Dir, "err", err)
	if err := os.RemoveAll(w.Dir); err != nil {
		slog.Warn("delete worktree directory failed", "dir", w.Dir, "err", err)
	}
	if _, err := g.run(ctx, g.repoDir(), "worktree", "prune"); err != nil {
		slog.Warn("prune worktrees failed", "err", err)
	}
}
