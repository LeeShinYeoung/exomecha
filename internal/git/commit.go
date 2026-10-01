package git

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

func (w *Worktree) Changes(ctx context.Context) (string, error) {
	out, err := w.git.run(ctx, w.Dir, "status", "--porcelain")
	// Not TrimSpace: a line for an unstaged change starts with a space.
	return strings.TrimRight(out, "\n"), err
}

func (w *Worktree) HasChanges(ctx context.Context) (bool, error) {
	out, err := w.Changes(ctx)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

func (w *Worktree) Head(ctx context.Context) (string, error) {
	out, err := w.git.run(ctx, w.Dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

func (w *Worktree) IsAncestor(ctx context.Context, ancestor, commit string) (bool, error) {
	_, err := w.git.run(ctx, w.Dir, "merge-base", "--is-ancestor", ancestor, commit)
	return yesNo(err)
}

// yesNo reads a git command that answers with its exit code: 0 for yes, 1 for no.
func yesNo(err error) (bool, error) {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	default:
		return false, err
	}
}

func (w *Worktree) Push(ctx context.Context) error {
	if w.branch == "" {
		return errors.New("read-only worktree has no branch to push")
	}
	ref := "HEAD:refs/heads/" + w.branch
	if w.fresh {
		ref = "+" + ref
	}
	_, err := w.git.run(ctx, w.Dir, "push", "origin", ref)
	return err
}
