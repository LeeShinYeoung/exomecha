package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const approvedLine = "exomecha's reviewer approved this PR."

func TestE2EHappyPath(t *testing.T) {
	e2e(t, true)
	const grace = 10 * time.Second
	w := newWorld(t, "happy", project(), func(c *config) { c.ciGrace = grace })
	w.start()

	prd := w.filePRD(shoutPRD, string(label.Ready))
	builder := w.box(prd, "builder")
	w.waitFor("the builder to start claude", aStart, func() bool { return w.started(builder) })
	w.wantStatus(prd, label.Working)
	w.waitFor("claude's init line", aRun, func() bool { return w.model(builder) != "" })
	if m := w.model(builder); !strings.Contains(m, "opus") {
		t.Errorf("claude's init line names model %q", m)
	}

	w.waitFor("the PR", aRun, func() bool { _, ok := w.openPR(prd); return ok })
	w.waitFor("the builder's sandbox and folder to go", aRound, func() bool { return !w.exists(builder) && !exists(w.runDir(builder)) })
	w.wantStatus(prd, label.Review)
	pr, _ := w.openPR(prd)
	if !strings.HasPrefix(pr.Body, "Closes #"+strconv.Itoa(prd)) {
		t.Errorf("PR body = %q, want it to close the PRD", pr.Body)
	}
	head := w.head(branchOf(prd))
	c := w.commit(head)
	const identity = "E2E Person <4242+e2e@users.noreply.github.com>"
	if c.author != identity || c.committer != identity {
		t.Errorf("commit by %q, committed by %q, want %q for both", c.author, c.committer, identity)
	}
	if !strings.HasSuffix(c.message, "PRD: #"+strconv.Itoa(prd)) {
		t.Errorf("commit message %q does not end with the PRD", c.message)
	}
	if err := w.testsPass(head); err != nil {
		t.Errorf("the PR's tests fail: %v", err)
	}

	reviewer := w.box(prd, "reviewer")
	w.waitFor("the reviewer to start claude", grace+aStart, func() bool { return w.started(reviewer) })
	w.waitFor("the review", aRun, func() bool { return w.status(prd) != label.Review })
	w.wantStatus(prd, label.Approved)
	p := w.gh.Pull(pr.Number)
	if n := len(p.Reviews); n != 1 || p.Reviews[0].Event != "COMMENT" || !strings.HasPrefix(p.Reviews[0].Body, github.Marker+"\n"+approvedLine) {
		t.Errorf("reviews = %+v, want one approval", p.Reviews)
	}
	if !p.Open || p.Merged {
		t.Errorf("PR open=%v merged=%v, want it left for a person", p.Open, p.Merged)
	}
}

func TestE2EStuckAndBack(t *testing.T) {
	e2e(t, false)
	w := newWorld(t, "stuck", project(), func(c *config) { c.ciGrace = time.Hour })
	w.allow("sent to stuck")
	prd := w.filePRD(officialPRD, string(label.Ready), string(label.Light))
	w.start()

	w.waitFor("builder to ask for the missing decision", aStart+aRun, func() bool { return w.status(prd) == label.Stuck })
	if q := w.lastComment(prd); !strings.HasPrefix(q, github.Marker) || !strings.Contains(q, "`"+string(label.Ready)+"`") {
		t.Errorf("stuck comment = %q, want exomecha's question that says to relabel it ready", q)
	}
	if n := len(w.prsOf(prd)); n != 0 {
		t.Errorf("a stuck PRD opened %d PRs", n)
	}

	// exomecha labels the PRD stuck before it removes the run, and the next run must not be confused with it.
	box := w.box(prd, "builder")
	w.waitFor("the asking run to be removed", aRound, func() bool { return !exists(w.runDir(box)) })
	w.gh.AddComment(prd, "The official greeting is Ahoy. `sh greet.sh` should print `Ahoy, world!`.")
	w.relabel(prd, label.Stuck, label.Ready)
	w.waitFor("builder to start again", aRound+aStart, func() bool { return w.started(box) })
	if in := w.input(box); !strings.Contains(in, "Ahoy") || !strings.Contains(in, "(person)") || !strings.Contains(in, "(exomecha)") {
		t.Errorf("the second run's input lacks the question or the answer:\n%s", in)
	}
	w.waitFor("the PR", aRun, func() bool { _, ok := w.openPR(prd); return ok })
	if out, err := w.run(w.head(branchOf(prd)), "sh greet.sh"); out != "Ahoy, world!" {
		t.Errorf("sh greet.sh on the PR = %q (%v), want the answer used", out, err)
	}
}

func TestE2EBlockedHost(t *testing.T) {
	e2e(t, false)
	w := newWorld(t, "host", project(), func(c *config) { c.ciGrace = time.Hour })
	w.allow("sent to stuck")
	prd := w.filePRD(pagePRD, string(label.Ready), string(label.Light))
	w.start()

	w.waitFor("builder to report the blocked host", aStart+aRun, func() bool { return w.status(prd) == label.Stuck })
	if c := w.lastComment(prd); !strings.Contains(c, "example.com") {
		t.Errorf("stuck comment = %q, want it to name example.com", c)
	}
	w.push("main", "", "chore: allow example.com", map[string]string{".exomecha/allowed-hosts": "example.com\n"})
	w.relabel(prd, label.Stuck, label.Ready)
	w.waitFor("the PR", aStart+aRun, func() bool { _, ok := w.openPR(prd); return ok })
	if got := w.file(branchOf(prd), "page.sha256"); !regexp.MustCompile(`^[0-9a-f]{64}\n?$`).MatchString(got) {
		t.Errorf("page.sha256 = %q, want a SHA-256", got)
	}
}

func TestE2ERestart(t *testing.T) {
	e2e(t, true)
	w := newWorld(t, "restart", project(), func(c *config) { c.ciGrace = time.Hour })
	prd := w.filePRD(shoutPRD, string(label.Ready), string(label.Light))
	w.start()

	builder := w.box(prd, "builder")
	w.waitFor("claude's output", aStart+aRound, func() bool { return w.streamSize(builder) > 0 })
	w.stop()
	w.wantStatus(prd, label.Working)
	if !w.running(builder) {
		t.Errorf("stopping exomecha stopped the builder's sandbox.\n%s", w.describe())
	}
	first := w.startedAt(builder)
	w.start()
	restarted := false
	w.waitFor("the PR", aRun, func() bool {
		if at := w.startedAt(builder); !at.IsZero() && !at.Equal(first) {
			restarted = true
		}
		_, ok := w.openPR(prd)
		return ok
	})
	if restarted {
		t.Error("builder started claude again after the restart instead of collecting the run")
	}
	w.gh.CloseIssue(prd)
}
