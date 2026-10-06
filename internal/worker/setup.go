package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const setupScript = ".exomecha/setup.sh"

const setupFix = " Fix the script on the base branch, then relabel this issue as below. You don't need to answer."

const setupNote = "## Setup\n\nexomecha ran the project's setup script in this directory (`" + setupScript + "`, the base branch's copy)."

// A script still running after this long is stopped and the PRD goes to stuck. Without a limit,
// a script that never ends would hold the PRD with nothing to show for it.
var setupTimeout = 10 * time.Minute

// setup runs the base branch's setup script in the sandbox's repository, always the base branch's
// copy, even on a PR branch, so a fix to the script reaches every open PRD. note tells claude it
// ran, and is "" when the base branch has no script. problem is set when the script breaks one of
// its rules, which only a person can fix: it exits 0 in time, and it leaves the working copy as git
// had it. What it leaves running stays, since the AI's tests may need it, and goes away with the
// sandbox.
func (r *runs) setup(ctx context.Context, ro role, n int, s spec) (note, problem string, err error) {
	content, ok, err := r.Git.Show(ctx, s.base, setupScript)
	if err != nil || !ok {
		return "", "", err
	}
	name := r.name(ro, n)
	if err := os.WriteFile(filepath.Join(r.ctl(name), "setup.sh"), []byte(content), 0o644); err != nil {
		return "", "", err
	}
	upToDate, err := r.Git.IsAncestor(ctx, s.base, s.head)
	if err != nil {
		return "", "", err
	}
	// The output goes to a file: a service the script starts would hold a pipe open, and the exec
	// would never return. The exit status comes back on stdout, apart from sbx's own failures.
	script := fmt.Sprintf(`timeout %d bash -eo pipefail -c "$(cat "$1/setup.sh")" > "$1/setup.log" 2>&1 < /dev/null; echo $?`,
		int(setupTimeout.Seconds()))
	// The timeout inside the sandbox stops the script. This one only covers sbx itself hanging.
	runCtx, cancel := context.WithTimeout(ctx, setupTimeout+time.Minute)
	defer cancel()
	start := time.Now()
	env := []string{"EXOMECHA_PRD=" + strconv.Itoa(n)}
	out, err := r.Sandboxes.Exec(runCtx, name, repoDir, env, "sh", "-c", script, "sh", r.ctl(name))
	if err != nil {
		return "", "", fmt.Errorf("run %s: %w", setupScript, err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return "", "", fmt.Errorf("read the exit status of %s: %q", setupScript, out)
	}

	head, changes := s.head, ""
	if code == 0 {
		out, err := r.git(ctx, name, repoDir, "rev-parse", "HEAD")
		if err != nil {
			return "", "", err
		}
		head = strings.TrimSpace(out)
		if changes, err = r.git(ctx, name, repoDir, "status", "--porcelain"); err != nil {
			return "", "", err
		}
		// Not TrimSpace: a line for an unstaged change starts with a space.
		changes = strings.TrimRight(changes, "\n")
	}
	minutes := int(setupTimeout.Minutes())
	var what string
	switch {
	case code == 124: // what timeout exits with when it stopped the script
		what = fmt.Sprintf("did not end cleanly (it ran past %d minutes). It must exit 0 within %d minutes.", minutes, minutes)
	case code != 0:
		what = fmt.Sprintf("did not end cleanly (exit status %d). It must exit 0 within %d minutes.", code, minutes)
	case head != s.head || changes != "":
		// Anything git can see, the AI can commit into the PR.
		what = "changed the working copy. Everything it creates must be ignored by git, and it must not edit tracked files or commit."
	default:
		slog.Info("ran setup script", "prd", n, "took", time.Since(start).Round(time.Second))
		if s.merge {
			return setupNote + " It ran before the merge started.\n\n", "", nil
		}
		return setupNote + " It ran before you started.\n\n", "", nil
	}

	// The details stay in the log. The output can hold secrets, and the comment goes on the issue.
	slog.Warn("setup script rejected", "prd", n, "why", what, "exit", code,
		"head_moved", head != s.head, "changes", changes, "output", r.tail(name, "setup.log"))
	fix := setupFix
	if !upToDate {
		// Fixing the base branch cannot help a branch that lacks what the script relies on, such as a newer .gitignore.
		fix = fmt.Sprintf(" The script is the base branch's copy, and this PRD's branch is behind the base branch, so the branch may just be older than the script. If it is, merge the base branch into `%s`. If not, fix the script on the base branch. Then relabel this issue as below. You don't need to answer.", branchName(n))
	}
	return "", fmt.Sprintf("`%s` %s The details are in exomecha's log.", setupScript, what) + fix, nil
}
