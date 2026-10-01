package git

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func (g *Git) Base() string { return g.base }

func (g *Git) BaseRef() string { return "origin/" + g.base }

func (g *Git) OpenBase(ctx context.Context) (*Worktree, error) {
	return g.open(ctx, g.base)
}

func (g *Git) NewBranch(ctx context.Context, branch string) (*Worktree, error) {
	w, err := g.open(ctx, g.base)
	if err != nil {
		return nil, err
	}
	w.branch = branch
	w.fresh = true
	return w, nil
}

func (g *Git) OpenBranch(ctx context.Context, branch string) (*Worktree, error) {
	w, err := g.open(ctx, branch)
	if err != nil {
		return nil, err
	}
	w.branch = branch
	return w, nil
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

// StartMerge merges the base branch but stops before the merge commit, so the AI makes that
// commit itself and sees whatever the project's commit checks report.
func (w *Worktree) StartMerge(ctx context.Context) (conflicts []string, err error) {
	_, mergeErr := w.git.run(ctx, w.Dir, "merge", "--no-commit", "--no-ff", w.git.BaseRef())
	if mergeErr == nil {
		return nil, nil
	}
	// -z keeps non-ASCII file names as they are instead of quoting them.
	out, err := w.git.run(ctx, w.Dir, "diff", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	for name := range strings.SplitSeq(out, "\x00") {
		if name != "" {
			conflicts = append(conflicts, name)
		}
	}
	if len(conflicts) == 0 {
		return nil, mergeErr
	}
	return conflicts, nil
}

// MergeInProgress is false once the merge is committed, or when there was nothing to merge.
func (w *Worktree) MergeInProgress(ctx context.Context) (bool, error) {
	_, err := w.git.run(ctx, w.Dir, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	return yesNo(err)
}

func (w *Worktree) ConflictMarkers(files []string) ([]string, error) {
	var left []string
	for _, name := range files {
		has, err := hasConflictMarker(filepath.Join(w.Dir, name))
		if err != nil {
			return nil, err
		}
		if has {
			left = append(left, name)
		}
	}
	return left, nil
}

func hasConflictMarker(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil // deleted while resolving
	}
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(string(b)) {
		if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") {
			return true, nil
		}
	}
	return false, nil
}
