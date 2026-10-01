package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/LeeShinYeoung/coworker/internal/git"
)

var (
	// How long a finished script's output may stay open. A process the script left running holds it open.
	setupWaitDelay = 5 * time.Second
	setupShell     = "bash"
)

// Always the base branch's copy, even on a PR branch, so a fix to the script reaches every open Task.
//
// The script has two rules: it ends cleanly on its own, and it leaves the working copy as git had
// it. problem is set when it breaks either one, which only a person can fix.
func setup(ctx context.Context, wt *git.Worktree, baseRef string, task int) (ran bool, problem string, err error) {
	script, ok, err := wt.BaseFile(ctx, setupScript)
	if err != nil || !ok {
		return false, "", err
	}
	before, err := wt.Head(ctx)
	if err != nil {
		return false, "", err
	}
	// Asked before the script runs: another worker's fetch can move the base ref while it does.
	upToDate, err := wt.IsAncestor(ctx, baseRef, before)
	if err != nil {
		return false, "", err
	}

	runCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	// Plain exec, not the git runner: the script must not get the GitHub token.
	cmd := exec.CommandContext(runCtx, setupShell, "-eo", "pipefail", "-c", script)
	cmd.Dir = wt.Dir
	cmd.Env = append(os.Environ(), "COWORKER_TASK="+strconv.Itoa(task))
	cmd.WaitDelay = setupWaitDelay
	ownGroup(cmd)
	var out tailBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	start := time.Now()
	runErr := cmd.Run()
	// What the script started is kept only when the AI is going to run, since its tests may need
	// a daemon the script started on purpose. On every other way out, none of it may outlive the script.
	defer func() {
		if !ran {
			killGroup(cmd)
		}
	}()
	switch {
	case ctx.Err() != nil:
		return false, "", ctx.Err()
	case cmd.Process == nil:
		return false, "", fmt.Errorf("start %s: %w", setupScript, runErr)
	}

	after, changes := before, ""
	if runErr == nil {
		if after, err = wt.Head(ctx); err != nil {
			return false, "", err
		}
		if changes, err = wt.Changes(ctx); err != nil {
			return false, "", err
		}
	}
	var what string
	switch {
	case runErr != nil:
		what = fmt.Sprintf("did not end cleanly (%v). It must exit 0 within %d minutes and leave no process running.", runErr, int(setupTimeout.Minutes()))
	case after != before || changes != "":
		// Anything git can see, the AI can commit into the PR.
		what = "changed the working copy. Everything it creates must be ignored by git, and it must not edit tracked files or commit."
	default:
		slog.Info("ran setup script", "task", task, "took", time.Since(start).Round(time.Second))
		return true, "", nil
	}

	// The details stay in the log. The output can hold secrets, and the comment goes on the issue.
	slog.Warn("setup script rejected", "task", task, "why", what, "timed_out", runCtx.Err() != nil,
		"head_moved", after != before, "changes", changes, "output", out.String())
	fix := setupFix
	if !upToDate {
		// Fixing the base branch cannot help a branch that lacks what the script relies on, such as a newer .gitignore.
		fix = fmt.Sprintf(" The script is the base branch's copy, and this Task's branch is behind the base branch, so the branch may just be older than the script. If it is, merge the base branch into `%s`. If not, fix the script on the base branch. Then relabel this issue as below. You don't need to answer.", branchName(task))
	}
	return false, fmt.Sprintf("`%s` %s The details are in coworker's log.", setupScript, what) + fix, nil
}

// A script can print without limit.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*tailLimit {
		t.b = append(t.b[:0], t.b[len(t.b)-tailLimit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	b := t.b
	if len(b) > tailLimit {
		b = b[len(b)-tailLimit:]
	}
	// The cut can land in the middle of a character.
	return strings.TrimSpace(strings.ToValidUTF8(string(b), ""))
}
