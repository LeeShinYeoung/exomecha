package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The functions here work in the shared clone itself. The AI never works in it, so its config
// and hooks are exomecha's own.

func (g *Git) Sync(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sync(ctx)
}

// Rev returns the commit ref points at, with ok false when there is no such ref.
func (g *Git) Rev(ctx context.Context, ref string) (commit string, ok bool, err error) {
	out, err := g.run(ctx, g.repoDir(), "rev-parse", "--verify", "-q", ref+"^{commit}")
	if ok, err = yesNo(err); !ok {
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

func (g *Git) IsAncestor(ctx context.Context, ancestor, commit string) (bool, error) {
	_, err := g.run(ctx, g.repoDir(), "merge-base", "--is-ancestor", ancestor, commit)
	return yesNo(err)
}

// Show reads path as commit has it, with ok false when commit has no such file.
func (g *Git) Show(ctx context.Context, commit, path string) (content string, ok bool, err error) {
	// ls-tree prints "<mode> <type> <object>\t<path>", or nothing, still exiting 0, for a missing path.
	out, err := g.run(ctx, g.repoDir(), "ls-tree", commit, "--", path)
	entry := strings.Fields(out)
	if err != nil || len(entry) < 3 || entry[1] != "blob" {
		return "", false, err
	}
	// Reading the object ls-tree named keeps both calls on one commit even if a ref moves.
	content, err = g.run(ctx, g.repoDir(), "cat-file", "blob", entry[2])
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

// ConflictMarkers returns the files among files that still hold a conflict marker in commit.
func (g *Git) ConflictMarkers(ctx context.Context, commit string, files []string) ([]string, error) {
	var left []string
	for _, name := range files {
		// A file that is gone was deleted while resolving.
		content, ok, err := g.Show(ctx, commit, name)
		if err != nil {
			return nil, err
		}
		if ok && hasConflictMarker(content) {
			left = append(left, name)
		}
	}
	return left, nil
}

func hasConflictMarker(content string) bool {
	for line := range strings.Lines(content) {
		if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") {
			return true
		}
	}
	return false
}

// A transfer with a sandbox moves the whole repository in well under a minute. One that hangs,
// on an sbx that stopped answering, would otherwise hold mu and stall every worker.
var sandboxTransferTimeout = 5 * time.Minute

// SendTo pushes the base branch's commit as origin/<base>, and commit as branch, into the
// repository at url.
func (g *Git) SendTo(ctx context.Context, url, baseCommit, branch, commit string) error {
	ctx, cancel := context.WithTimeout(ctx, sandboxTransferTimeout)
	defer cancel()
	_, err := g.runSandbox(ctx, g.repoDir(), "push", "--quiet", url,
		baseCommit+":refs/remotes/origin/"+g.base,
		commit+":refs/heads/"+branch)
	return err
}

// FetchFrom fetches the HEAD of the repository at url into refs/exomecha/<name> and returns that commit.
func (g *Git) FetchFrom(ctx context.Context, url, name string) (string, error) {
	ref := "refs/exomecha/" + name
	g.mu.Lock()
	defer g.mu.Unlock()
	fetchCtx, cancel := context.WithTimeout(ctx, sandboxTransferTimeout)
	defer cancel()
	if _, err := g.runSandbox(fetchCtx, g.repoDir(), "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", url, "+HEAD:"+ref); err != nil {
		return "", err
	}
	commit, ok, err := g.Rev(ctx, ref)
	if err == nil && !ok {
		err = fmt.Errorf("fetch from %s left no %s", url, ref)
	}
	return commit, err
}

// DropRef deletes what FetchFrom kept for name.
func (g *Git) DropRef(ctx context.Context, name string) error {
	_, err := g.run(ctx, g.repoDir(), "update-ref", "-d", "refs/exomecha/"+name)
	return err
}

// PushCommit pushes commit to branch on GitHub. force replaces a branch that does not hold commit.
func (g *Git) PushCommit(ctx context.Context, commit, branch string, force bool) error {
	ref := commit + ":refs/heads/" + branch
	if force {
		ref = "+" + ref
	}
	_, err := g.runGitHub(ctx, g.repoDir(), "push", "--quiet", "origin", ref)
	return err
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
