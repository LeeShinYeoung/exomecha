package git

import "context"

func (g *Git) Base() string { return g.base }

func (g *Git) BaseRef() string { return "origin/" + g.base }

func (g *Git) OpenBase(ctx context.Context) (*Worktree, error) {
	return g.open(ctx, g.base)
}

func (g *Git) OpenBranch(ctx context.Context, branch string) (*Worktree, error) {
	return g.open(ctx, branch)
}

// Reads the base branch as of the last fetch. Sync and opening a worktree both fetch.
func (g *Git) BaseFile(ctx context.Context, path string) (content string, ok bool, err error) {
	return g.Show(ctx, g.BaseRef(), path)
}
