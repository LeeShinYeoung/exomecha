package main

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/label"
)

func TestE2EConflict(t *testing.T) {
	e2e(t, true)
	const grace = 10 * time.Second
	w := newWorld(t, "conflict", project(), func(c *config) { c.ciGrace = grace })
	prd, pr := w.seedReview(shoutPRD, shoutDone)
	w.push("main", "", "refactor: print with printf", mainRefactor)
	before, mainHead := w.head(branchOf(prd)), w.head("main")
	w.start()

	resolver, reviewer := w.box(prd, "resolver"), w.box(prd, "reviewer")
	noReview := func() {
		if w.head(branchOf(prd)) == before && w.exists(reviewer) {
			t.Fatalf("reviewer took a PR that conflicts.\n%s", w.describe())
		}
	}
	w.waitFor("resolver", aStart, func() bool { noReview(); return w.started(resolver) })
	if args := w.claudeArgs(resolver); !strings.Contains(args, "--effort low") {
		t.Errorf("resolver runs claude as %q, want the light tier", args)
	}
	w.waitFor("the merge", aRun, func() bool { noReview(); return w.head(branchOf(prd)) != before })
	head := w.head(branchOf(prd))
	c := w.commit(head)
	if !slices.Contains(c.parents, mainHead) || len(c.parents) != 2 {
		t.Errorf("the branch head's parents = %v, want a merge of main %s", c.parents, mainHead)
	}
	if !strings.HasSuffix(c.message, "PRD: #"+strconv.Itoa(prd)) || strings.Contains(c.message, "\n#") {
		t.Errorf("merge message = %q, want it to end with the PRD and have no comment lines", c.message)
	}
	if c.author != "E2E Person <4242+e2e@users.noreply.github.com>" {
		t.Errorf("merge by %q", c.author)
	}
	if out, err := w.run(head, "! grep -rnE '^(<<<<<<<|>>>>>>>)' --exclude-dir=.git ."); err != nil {
		t.Errorf("conflict markers left:\n%s", out)
	}
	if err := w.testsPass(head); err != nil {
		t.Errorf("the merged branch's tests fail: %v", err)
	}
	w.wantStatus(prd, label.Review)

	w.holdAsTheyStart(grace+aStart+aRound, 1, reviewer)
	if got := w.startHead(reviewer); got != head {
		t.Errorf("reviewer started on %s, want the merge %s", got, head)
	}
	w.push("main", "", "refactor: greet through a function", mainFunction)
	w.waitFor("the review of a PR that now conflicts to be dropped", 2*aRound, func() bool {
		return !w.exists(reviewer) && !exists(w.runDir(reviewer))
	})
	w.wantStatus(prd, label.Review)
	if n := len(w.reviews(pr)); n != 0 {
		t.Errorf("the dropped review left %d reviews", n)
	}
	w.waitFor("resolver to take the new conflict", 2*aRound+aStart, func() bool { return w.started(resolver) })
	w.gh.CloseIssue(prd)
}

func TestE2EContradiction(t *testing.T) {
	e2e(t, false)
	w := newWorld(t, "contra", project(), func(c *config) { c.ciGrace = time.Hour })
	w.allow("sent to stuck")
	prd, _ := w.seedReview(shoutPRD, shoutDone)
	w.push("main", "", "docs: greet.sh takes no flags", mainNoFlags)
	before := w.head(branchOf(prd))
	w.start()

	builder := w.box(prd, "builder")
	noBuilder := func() {
		if w.exists(builder) {
			t.Fatalf("builder took a PRD in review.\n%s", w.describe())
		}
	}
	w.waitFor("resolver to ask which side wins", aStart+aRun, func() bool { noBuilder(); return w.status(prd) == label.Stuck })
	if q := w.lastComment(prd); !strings.Contains(q, "`"+string(label.Review)+"`") {
		t.Errorf("stuck comment = %q, want it to say to relabel it review", q)
	}
	w.gh.AddComment(prd, "Keep --shout. It is the one exception to the no-flags rule in CLAUDE.md: keep main's change that every other argument is part of the name, and keep `sh greet.sh --shout` printing `HELLO, WORLD!`.")
	w.relabel(prd, label.Stuck, label.Review)
	w.waitFor("the merge", aStart+aRun, func() bool { noBuilder(); return w.head(branchOf(prd)) != before })
	head := w.head(branchOf(prd))
	if !w.isAncestor(w.head("main"), head) {
		t.Errorf("the branch does not contain main")
	}
	if out, err := w.run(head, "sh greet.sh --shout"); out != "HELLO, WORLD!" {
		t.Errorf("sh greet.sh --shout = %q (%v), want the answer followed", out, err)
	}
	w.gh.CloseIssue(prd)
}

func TestE2EApprovedConflict(t *testing.T) {
	e2e(t, false)
	const grace = 10 * time.Second
	w := newWorld(t, "approvedconf", project(), func(c *config) { c.ciGrace = grace })
	a, prA := w.seedApproved(shoutPRD, shoutDone)
	b, _ := w.seedApproved(farewellPRD, farewellDone)
	w.push("main", "", "refactor: print with printf", mainRefactor)
	before := w.head(branchOf(a))
	w.start()

	quiet := func() {
		for _, role := range []string{"builder", "reviewer", "resolver"} {
			if w.exists(w.box(b, role)) {
				t.Fatalf("an approved PR with no conflict got a %s.\n%s", role, w.describe())
			}
		}
	}
	w.waitFor("the conflicting approved PRD back in review", 3*w.cfg.interval+aRound, func() bool { quiet(); return w.status(a) == label.Review })
	w.waitFor("the merge", aStart+aRun, func() bool { quiet(); return w.head(branchOf(a)) != before })
	w.waitFor("approved again", grace+aStart+aRun, func() bool { quiet(); return w.status(a) == label.Approved })
	if n := approvals(w.reviews(prA)); n != 2 {
		t.Errorf("%d approvals on the PR, want a second one after the merge", n)
	}
	w.wantStatus(b, label.Approved)
}
