package worker

import (
	"context"
	"fmt"
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
const setupTimeout = 10 * time.Minute

// setup runs the base branch's setup script in the sandbox's repository, always the base branch's
// copy, even on a PR branch, so a fix to the script reaches every open PRD. note tells claude it
// ran, and is "" when the base branch has no script. The problem is set when the script breaks one
// of its rules, which only a person can fix: it exits 0 in time, and it leaves the working copy as
// git had it. Its output is in setup.log, which a stuck issue keeps. What it leaves running stays,
// since the AI's tests may need it, and goes away with the sandbox.
func (r *runs) setup(ctx context.Context, ro role, n int, s spec) (note string, p *problem, err error) {
	content, ok, err := r.Git.Show(ctx, s.base, setupScript)
	if err != nil || !ok {
		return "", nil, err
	}
	name := r.name(ro, n)
	if err := os.WriteFile(filepath.Join(r.ctl(name), "setup.sh"), []byte(content), 0o644); err != nil {
		return "", nil, err
	}
	upToDate, err := r.Git.IsAncestor(ctx, s.base, s.head)
	if err != nil {
		return "", nil, err
	}
	// The output goes to a file: a service the script starts would hold a pipe open, and the exec
	// would never return. The exit status comes back on stdout, apart from sbx's own failures.
	script := fmt.Sprintf(`timeout %d bash -eo pipefail -c "$(cat "$1/setup.sh")" > "$1/setup.log" 2>&1 < /dev/null; echo $?`,
		int(setupTimeout.Seconds()))
	// The timeout inside the sandbox stops the script. This one only covers sbx itself hanging.
	runCtx, cancel := context.WithTimeout(ctx, setupTimeout+time.Minute)
	defer cancel()
	env := []string{"EXOMECHA_PRD=" + strconv.Itoa(n)}
	out, err := r.Sandboxes.Exec(runCtx, name, repoDir, env, "sh", "-c", script, "sh", r.ctl(name))
	if err != nil {
		return "", nil, fmt.Errorf("run %s: %w", setupScript, err)
	}
	_, code, err := splitStatus(string(out))
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", setupScript, err)
	}

	head, changes := s.head, ""
	if code == 0 {
		out, err := r.git(ctx, name, repoDir, "rev-parse", "HEAD")
		if err != nil {
			return "", nil, err
		}
		head = strings.TrimSpace(out)
		if changes, err = r.changes(ctx, name); err != nil {
			return "", nil, err
		}
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
		if s.merge {
			return setupNote + " It ran before the merge started.\n\n", nil, nil
		}
		return setupNote + " It ran before you started.\n\n", nil, nil
	}

	var outputs map[string]string
	if changes != "" {
		outputs = map[string]string{"git-status.log": changes}
	}
	fix := setupFix
	if !upToDate {
		// Fixing the base branch cannot help a branch that lacks what the script relies on, such as a newer .gitignore.
		fix = fmt.Sprintf(" The script is the base branch's copy, and this PRD's branch is behind the base branch, so the branch may just be older than the script. If it is, merge the base branch into `%s`. If not, fix the script on the base branch. Then relabel this issue as below. You don't need to answer.", branchName(n))
	}
	return "", &problem{why: fmt.Sprintf("`%s` %s", setupScript, what) + fix, outputs: outputs}, nil
}
