package main

import (
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const setupScript = "git config core.hooksPath .githooks\nmkdir -p .cache\necho ran > .cache/setup-ran\n" +
	"echo .cache/ >> \"$(git rev-parse --git-path info/exclude)\"\n" +
	"docker run -d --restart unless-stopped --name e2e-svc busybox sleep 100000\n"

func TestE2ESetupAndCommitChecks(t *testing.T) {
	e2e(t, false)
	files := project(map[string]string{
		".githooks/commit-msg": "#!/bin/sh\nhead -n 1 \"$1\" | grep -q ' \\[e2e\\]$' && exit 0\n" +
			"echo 'commit-msg: the first line must end with \" [e2e]\"' >&2\nexit 1\n",
	})
	files["CLAUDE.md"] += "- The commit-msg hook in .githooks checks every commit message.\n"
	w := newWorld(t, "setup", files, func(c *config) { c.ciGrace = time.Hour })
	w.writeSetup("setup.sh", setupScript)
	prd := w.filePRD(shoutPRD, string(label.Ready), string(label.Light))
	w.start()

	builder := w.box(prd, "builder")
	w.waitFor("builder", aStart, func() bool { return w.started(builder) })
	if out, err := w.execIn(builder, "cat", ".cache/setup-ran"); out != "ran" {
		t.Errorf("the setup script's file in the sandbox = %q (%v)", out, err)
	}
	if out, err := w.exec(builder, "docker", "inspect", "-f", "{{.State.Running}}", "e2e-svc"); strings.TrimSpace(out) != "true" {
		t.Errorf("the service the setup script started: %q (%v)", out, err)
	}
	if !exists(w.ctl(builder, "setup.log")) {
		t.Error("no setup.log for the run")
	}
	w.waitFor("the PR", aRun, func() bool { _, ok := w.openPR(prd); return ok })
	head := w.head(branchOf(prd))
	if w.file(head, ".cache/setup-ran") != "" {
		t.Error("the PR holds a file the setup script made")
	}
	c := w.commit(head)
	if first, _, _ := strings.Cut(c.message, "\n"); !strings.HasSuffix(first, " [e2e]") || !strings.HasSuffix(c.message, "PRD: #"+itoa(prd)) {
		t.Errorf("builder's commit message = %q, want it past the hook and ending with the PRD", c.message)
	}

	w.push("main", "", "refactor: print with printf [e2e]", mainRefactor)
	w.waitFor("resolver's merge", aStart+aRun, func() bool { return w.head(branchOf(prd)) != head })
	m := w.commit(w.head(branchOf(prd)))
	if first, _, _ := strings.Cut(m.message, "\n"); len(m.parents) != 2 || !strings.HasSuffix(first, " [e2e]") {
		t.Errorf("resolver's commit %q with parents %v, want a merge past the hook", m.message, m.parents)
	}
	w.gh.CloseIssue(prd)
}

// Not haiku: claude sometimes reports auto permission mode for it, and then the start goes ahead.
// TestCheckStream covers a model claude does not run in auto permission mode.
func TestE2EStartRefusesUnknownModel(t *testing.T) {
	e2e(t, false)
	gh := githubtest.New(t)
	cfg := defaults()
	// The start check asks GitHub for every repository, so without a fake one it would reach the real GitHub.
	cfg.prd = target{owner: gh.Owner, name: gh.Repo, apiURL: gh.URL}
	cfg.repos = []target{cfg.prd}
	cfg.token = "e2e-token"
	cfg.light.Builder = agent.Model{Name: "opsu", Effort: "low"}
	err := serve(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "models.light.builder") || !strings.Contains(err.Error(), "opsu") || strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("start with light builder opsu: %v, want only a refusal naming models.light.builder and opsu", err)
	}
}
