package main

import (
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/github"
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
	w := newWorld(t, "person", project(), func(c *config) { c.ciGrace = time.Hour })
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
	w.gh.CloseIssue(prd)
}
