package git

import (
	"context"
	"strings"
)

func (g *Git) Base() string { return g.base }

func (g *Git) BaseRef() string { return "origin/" + g.base }

func (g *Git) OpenBase(ctx context.Context) (*Worktree, error) {
	return g.open(ctx, g.base)
}

func (g *Git) OpenBranch(ctx context.Context, branch string) (*Worktree, error) {
	return g.open(ctx, branch)
}

func (w *Worktree) BaseFile(ctx context.Context, path string) (content string, ok bool, err error) {
	return w.git.baseFile(ctx, w.Dir, path)
}

// Reads the base branch as of the last fetch. Callers open a worktree first, which fetches.
func (g *Git) BaseFile(ctx context.Context, path string) (content string, ok bool, err error) {
	return g.baseFile(ctx, g.repoDir(), path)
}

func (g *Git) baseFile(ctx context.Context, dir, path string) (content string, ok bool, err error) {
	// ls-tree prints "<mode> <type> <object>\t<path>", or nothing, still exiting 0, for a missing path.
	out, err := g.run(ctx, dir, "ls-tree", g.BaseRef(), "--", path)
	entry := strings.Fields(out)
	if err != nil || len(entry) < 3 || entry[1] != "blob" {
		return "", false, err
	}
	// Reading the object ls-tree named keeps both calls on one commit even if the ref moves.
	content, err = g.run(ctx, dir, "cat-file", "blob", entry[2])
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}
