package git

import "context"

func (g *Git) Base() string { return g.base }

func (g *Git) BaseRef() string { return "origin/" + g.base }

// Reads the base branch as of the last Sync.
func (g *Git) BaseFile(ctx context.Context, path string) (content string, ok bool, err error) {
	return g.Show(ctx, g.BaseRef(), path)
}
