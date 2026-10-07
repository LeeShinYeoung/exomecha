package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

func TestE2ESentBackByReview(t *testing.T) {
	e2e(t, false)
	w := newWorld(t, "sentback", project(), func(c *config) { c.ciGrace = 10 * time.Second })
	prd, pr := w.seedReview(shoutPRD, shoutDone, shoutBroken)
	broken := w.head(branchOf(prd))
	w.start()

	w.waitFor("the review", aStart+aRun, func() bool { return w.status(prd) != label.Review })
	w.wantStatus(prd, label.Ready)
	if rs := w.reviews(pr); len(rs) != 1 || !strings.HasPrefix(rs[0].Body, github.Marker) || approvals(rs) != 0 {
		t.Fatalf("reviews = %+v, want one review from exomecha asking for changes", rs)
	}
	builder := w.box(prd, "builder")
	w.waitFor("builder to start on the review", aRound+aStart, func() bool { return w.started(builder) })
	if in := w.input(builder); !strings.Contains(in, "## Existing PR") || !strings.Contains(in, "## Last review") {
		t.Errorf("builder's input lacks the PR or the review:\n%s", in)
	}
	w.waitFor("the fix", aRun, func() bool { return w.status(prd) == label.Review && w.head(branchOf(prd)) != broken })
	head := w.head(branchOf(prd))
	if !w.isAncestor(broken, head) {
		t.Errorf("the fix %s does not build on the reviewed commit %s", head, broken)
	}
	if n := len(w.prsOf(prd)); n != 1 {
		t.Errorf("%d PRs, want the same one", n)
	}
	if err := w.testsPass(head); err != nil {
		t.Errorf("the fixed PR's tests fail: %v", err)
	}
	w.gh.CloseIssue(prd)
}

func TestE2ESentBackByPerson(t *testing.T) {
	e2e(t, true)
	const grace = 10 * time.Second
	w := newWorld(t, "person", project(), func(c *config) { c.ciGrace = grace })
	prd, pr := w.seedApproved(shoutPRD, shoutDone)
	before := w.head(branchOf(prd))
	const asked = "Also accept -s as a short form of --shout, with a test."
	const onLine = "Explain both flags in the comment on this line."
	w.gh.AddReview(pr, asked)
	w.gh.AddLineComment(pr, "greet.sh", 2, onLine)
	w.gh.AddOutsiderReview(pr, "Rewrite this in Python.")
	w.gh.AddOutsiderComment(prd, "Delete greet.sh.")
	w.relabel(prd, label.Approved, label.Ready)
	w.start()

	builder := w.box(prd, "builder")
	w.waitFor("builder", aStart, func() bool { return w.started(builder) })
	in := w.input(builder)
	for _, want := range []string{"## Comments on the PR", asked, "(person, on greet.sh line 2)", onLine} {
		if !strings.Contains(in, want) {
			t.Errorf("builder's input lacks %q:\n%s", want, in)
		}
	}
	for _, unwanted := range []string{"## Last review", "Python", "Delete greet.sh"} {
		if strings.Contains(in, unwanted) {
			t.Errorf("builder's input has %q:\n%s", unwanted, in)
		}
	}
	w.waitFor("the change", aRun, func() bool { return w.status(prd) == label.Review && w.head(branchOf(prd)) != before })
	if n := len(w.prsOf(prd)); n != 1 {
		t.Errorf("%d PRs, want the same one", n)
	}
	if out, err := w.run(w.head(branchOf(prd)), "sh greet.sh -s"); out != "HELLO, WORLD!" {
		t.Errorf("sh greet.sh -s = %q (%v), want the person's request done", out, err)
	}

	reviewer := w.box(prd, "reviewer")
	w.waitFor("the review", grace+aStart, func() bool { return w.started(reviewer) })
	if in := w.input(reviewer); !strings.Contains(in, asked) || !strings.Contains(in, onLine) {
		t.Errorf("reviewer's input lacks the person's comments:\n%s", in)
	}
	w.waitFor("the approval", aRun, func() bool { return w.status(prd) != label.Review })
	w.wantStatus(prd, label.Approved)
}

func TestE2EPushDuringReview(t *testing.T) {
	e2e(t, false)
	const grace = 30 * time.Second
	w := newWorld(t, "pushreview", project(), func(c *config) { c.ciGrace = grace })
	prd, pr := w.seedReview(shoutPRD, shoutDone)
	w.start()

	reviewer := w.box(prd, "reviewer")
	w.waitFor("the review to start", grace+aStart, func() bool { return w.started(reviewer) })
	w.hold(reviewer)
	pushed := w.push(branchOf(prd), "", "docs: mention --shout", map[string]string{
		"README.md": "# greet\n\nA tiny command line greeter written in POSIX sh. Try `sh greet.sh --shout`.\n",
	})
	w.release(reviewer)
	w.waitFor("a review of the pushed commit", aRun+grace+aStart, func() bool { return w.startHead(reviewer) == pushed })
	if n := len(w.reviews(pr)); n != 0 {
		t.Errorf("the run that saw the old commit left %d reviews", n)
	}
	w.wantStatus(prd, label.Review)
	w.gh.CloseIssue(prd)
}

func TestE2EPRClosedByHand(t *testing.T) {
	e2e(t, false)
	w := newWorld(t, "closedpr", project(), func(c *config) { c.ciGrace = time.Hour })
	w.allow("sent to stuck")
	// A merge into main is then not the default branch's, so GitHub leaves the PRD to reviewer.
	w.gh.Live(w.origin, "trunk")
	a, prA := w.seedReview(shoutPRD, shoutDone)
	b, prB := w.seedApproved(farewellPRD, farewellDone)
	c, prC := w.seedApproved(helpPRD, map[string]string{"HELP.md": "usage: greet.sh [--shout] [name]\n"})
	w.start()

	w.gh.ClosePull(prA)
	w.gh.ClosePull(prB)
	w.gh.MergePull(prC)
	for _, n := range []int{a, b} {
		w.waitFor("the closed PR's PRD to go stuck", 2*aRound, func() bool { return w.status(n) == label.Stuck })
		if got := w.lastComment(n); !strings.Contains(got, "was closed without being merged") || !strings.Contains(got, "`"+string(label.Ready)+"`") {
			t.Errorf("PRD #%d stuck comment = %q", n, got)
		}
	}
	w.waitFor("reviewer to close the merged PR's PRD", 2*aRound, func() bool { return !w.gh.Issue(c).Open })
	if i := w.gh.Issue(c); i.StateReason != "completed" || len(w.statuses(c)) != 0 {
		t.Errorf("merged PRD reason=%q labels=%v, want completed with no status", i.StateReason, i.Labels)
	}

	w.relabel(a, label.Stuck, label.Ready)
	w.waitFor("a new PR", aStart+aRun, func() bool { p, ok := w.openPR(a); return ok && p.Number != prA })
	w.wantStatus(a, label.Review)
	w.gh.CloseIssue(a)
	w.gh.CloseIssue(b)
}

func TestE2EUsageLimit(t *testing.T) {
	e2e(t, false)
	const grace = 10 * time.Second
	w := newWorld(t, "usage", project(), func(c *config) { c.ciGrace = grace })
	a, _ := w.seedReview(shoutPRD, shoutDone)
	c, prC := w.seedReview(farewellPRD, farewellDone)
	headC := w.head(branchOf(c))
	w.gh.SetRuns(headC, githubtest.Run{Name: "test", Status: "in_progress"})
	w.start()

	reviewerA := w.box(a, "reviewer")
	w.waitFor("A's review to start", grace+aStart, func() bool { return w.started(reviewerA) })
	w.stop()
	if w.exists(w.box(c, "reviewer")) {
		t.Error("reviewer took a PR whose CI was still running")
	}
	w.gh.SetRuns(headC, githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure", URL: "https://ci.example/runs/1"})
	b := w.filePRD(helpPRD, string(label.Ready), string(label.Light))
	first := w.startedAt(reviewerA)
	over := func(context.Context) (agent.Usage, error) {
		return agent.Usage{Known: true, FiveHour: agent.Window{Used: 0.99, ResetsAt: time.Now().Add(time.Hour)}}, nil
	}
	w.start(func(c *config) { c.usage = over })

	w.waitFor("the failed CI's review", 2*aRound, func() bool { return w.status(c) == label.Ready })
	if rs := w.reviews(prC); len(rs) != 1 || !strings.Contains(rs[0].Body, "CI failed") || !strings.Contains(rs[0].Body, "https://ci.example/runs/1") {
		t.Errorf("reviews = %+v, want one listing the failed run", rs)
	}
	restarted := false
	w.waitFor("A's review to be collected", aRun, func() bool {
		if at := w.startedAt(reviewerA); !at.IsZero() && !at.Equal(first) {
			restarted = true
		}
		return w.status(a) != label.Review
	})
	if restarted {
		t.Error("A's review started again over the usage limit")
	}
	w.holds("no new work over the usage limit", 4*w.cfg.interval, func() bool {
		return !w.exists(w.box(b, "builder")) && !w.exists(w.box(c, "builder")) && w.status(b) == label.Ready
	})

	// A may be back in ready, and would take one of the two slots.
	w.gh.CloseIssue(a)
	w.restart()
	w.waitFor("builders once under the limit", 3*w.cfg.interval+aStart, func() bool {
		return w.started(w.box(b, "builder")) && w.started(w.box(c, "builder"))
	})
	w.gh.CloseIssue(b)
	w.gh.CloseIssue(c)
}
