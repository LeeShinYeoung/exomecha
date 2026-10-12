package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const setupScript = "setup.sh"

// sandboxRules is what the person chose for the sandbox when /exomecha:setup asked, such as
// which checks not to run there.
const sandboxRules = "sandbox.md"

const setupFix = " Fix the script, or make it again with /exomecha:setup, then relabel this issue as below. You don't need to answer."

// A script still running after this long is stopped and the PRD goes to stuck. Without a limit,
// a script that never ends would hold the PRD with nothing to show for it.
const setupTimeout = 10 * time.Minute

// setup runs the project's setup script in the run's sandbox, where claude starts. note tells
// claude it ran, and is "" when the project has no script. The problem is set when the script
// breaks one of its rules, which only a person can fix: it exits 0 in time, and it leaves every
// repository's working copy as git had it. Its output is in setup.log, which a stuck issue keeps.
// What it leaves running stays, since the AI's tests may need it, and goes away with the sandbox.
func (r *runs) setup(ctx context.Context, ro role, n int, repos []repoRun) (note string, p *problem, err error) {
	path := filepath.Join(r.SetupDir, setupScript)
	content, ok, err := readSetupFile(path)
	if err != nil || !ok {
		return "", nil, err
	}
	name := r.name(ro, n)
	if err := os.WriteFile(filepath.Join(r.ctl(name), "setup.sh"), []byte(content), 0o644); err != nil {
		return "", nil, err
	}
	// The output goes to a file: a service the script starts would hold a pipe open, and the exec
	// would never return. The exit status comes back on stdout, apart from sbx's own failures.
	script := fmt.Sprintf(`timeout %d bash -eo pipefail -c "$(cat "$1/setup.sh")" > "$1/setup.log" 2>&1 < /dev/null; echo $?`,
		int(setupTimeout.Seconds()))
	// The timeout inside the sandbox stops the script. This one only covers sbx itself hanging.
	runCtx, cancel := context.WithTimeout(ctx, setupTimeout+time.Minute)
	defer cancel()
	env := append([]string{"EXOMECHA_PRD=" + strconv.Itoa(n), "EXOMECHA_REPOS=" + reposEnv(repos)}, r.secretEnv(ro)...)
	out, err := r.Sandboxes.Exec(runCtx, name, repoDir, env, "sh", "-c", script, "sh", r.ctl(name))
	if err != nil {
		return "", nil, fmt.Errorf("run %s: %w", path, err)
	}
	_, code, err := splitStatus(string(out))
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", path, err)
	}

	var changed []string // git status of each repository the script changed
	if code == 0 {
		for _, rr := range repos {
			out, err := r.git(ctx, name, rr.Dir, "rev-parse", "HEAD")
			if err != nil {
				return "", nil, err
			}
			changes, err := r.changes(ctx, name, rr.Dir)
			if err != nil {
				return "", nil, err
			}
			if strings.TrimSpace(out) == rr.Head && changes == "" {
				continue
			}
			if f := rr.folder(); f != "" {
				changes = f + "/:\n" + changes
			}
			changed = append(changed, changes)
		}
	}
	minutes := int(setupTimeout.Minutes())
	var what string
	switch {
	case code == 124: // what timeout exits with when it stopped the script
		what = fmt.Sprintf("did not end cleanly (it ran past %d minutes). It must exit 0 within %d minutes.", minutes, minutes)
	case code != 0:
		what = fmt.Sprintf("did not end cleanly (exit status %d). It must exit 0 within %d minutes.", code, minutes)
	case len(changed) > 0:
		// Anything git can see, the AI can commit into the PR.
		what = "changed the working copy. Everything it creates must be ignored by git, and it must not edit tracked files or commit."
	default:
		note := "## Setup\n\nexomecha ran the project's setup script (`" + filepath.Join(r.ctl(name), setupScript) + "`) in this directory."
		if slices.ContainsFunc(repos, func(rr repoRun) bool { return rr.Merge }) {
			return note + " It ran before the merge started.\n\n", nil, nil
		}
		return note + " It ran before you started.\n\n", nil, nil
	}

	var outputs map[string]string
	if status := strings.TrimSpace(strings.Join(changed, "\n")); status != "" {
		outputs = map[string]string{"git-status.log": status}
	}
	return "", &problem{why: fmt.Sprintf("`%s` %s", path, what) + setupFix, outputs: outputs}, nil
}

// rules is the input section with the project's sandbox rules, or "" when there are none.
func (r *runs) rules() (string, error) {
	content, ok, err := readSetupFile(filepath.Join(r.SetupDir, sandboxRules))
	if err != nil || !ok || strings.TrimSpace(content) == "" {
		return "", err
	}
	return "## Sandbox rules\n\nThe project sets these rules for this sandbox. " +
		"Follow them where they differ from CLAUDE.md.\n\n" + strings.TrimSpace(content) + "\n\n", nil
}

func readSetupFile(path string) (content string, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	return string(b), err == nil, err
}

// reposEnv lists the run's repositories for the setup script, one line each: owner/name, then
// where it is in the sandbox.
func reposEnv(repos []repoRun) string {
	var lines []string
	for _, rr := range repos {
		lines = append(lines, rr.Repo+" "+rr.Dir)
	}
	return strings.Join(lines, "\n")
}
