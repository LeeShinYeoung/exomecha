package main

import (
	"regexp"
	"slices"
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
	const grace = 30 * time.Second
	w := newWorld(t, "happy", project(), func(c *config) { c.ciGrace = grace })
	w.start()
	if got := w.gh.Labels(); len(got) != 10 {
		t.Errorf("labels after the start = %q, want ten", got)
	}

	prd := w.filePRD(shoutPRD, string(label.Ready))
	builder := w.box(prd, "builder")
	w.waitFor("the builder to start claude", aStart, func() bool { return w.started(builder) })
	w.wantStatus(prd, label.Working)
	if args := w.claudeArgs(builder); !strings.Contains(args, "--model opus") || !strings.Contains(args, "--effort high") {
		t.Errorf("a PRD with no tier label runs claude as %q, want the normal tier", args)
	}
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
	committed, err := time.Parse(time.RFC3339, w.git(w.origin, "show", "-s", "--format=%cI", head))
	if err != nil {
		t.Fatal(err)
	}
	if at := w.startedAt(reviewer); at.Before(committed.Add(grace - 3*time.Second)) {
		t.Errorf("the review started %v after the commit, before the CI grace of %v", at.Sub(committed), grace)
	}
	if args := w.claudeArgs(reviewer); !strings.Contains(args, "--effort xhigh") {
		t.Errorf("reviewer runs claude as %q, want the heavy tier", args)
	}
	w.waitFor("the review", aRun, func() bool { return w.status(prd) != label.Review })
	w.wantStatus(prd, label.Approved)
	p := w.gh.Pull(pr.Number)
	if n := len(p.Reviews); n != 1 || p.Reviews[0].Event != "COMMENT" || !strings.HasPrefix(p.Reviews[0].Body, github.Marker+"\n"+approvedLine) {
		t.Errorf("reviews = %+v, want one approval", p.Reviews)
	}
	if !p.Open || p.Merged {
		t.Errorf("PR open=%v merged=%v, want it left for a person", p.Open, p.Merged)
	}
	w.waitFor("the reviewer's sandbox and folder to go", 2*aRound, func() bool { return !w.exists(reviewer) && !exists(w.runDir(reviewer)) })
	w.holds("no second review", 3*w.cfg.interval, func() bool { return !w.exists(reviewer) })

	w.gh.MergePull(pr.Number)
	if got := w.commit("main").parents; len(got) != 2 || got[1] != head {
		t.Errorf("main's parents = %v, want the merge of %s", got, head)
	}
	if i := w.gh.Issue(prd); i.Open || i.StateReason != "completed" || !i.HasLabel(string(label.Approved)) {
		t.Errorf("PRD open=%v reason=%q labels=%v, want closed with status:approved kept", i.Open, i.StateReason, i.Labels)
	}
	w.holds("no sandbox after the merge", 3*w.cfg.interval, func() bool { return len(w.boxes()) == 0 })
	if left := dirEntries(w.root + "/sandboxes"); len(left) != 0 {
		t.Errorf("run folders left: %v", left)
	}
}

func TestE2EParallelAndBlocked(t *testing.T) {
	e2e(t, false)
	w := newWorld(t, "parallel", project(), func(c *config) { c.ciGrace = time.Hour })
	light := string(label.Light)
	a := w.filePRD(shoutPRD, string(label.Ready), light)
	b := w.filePRD(farewellPRD, string(label.Ready), light)
	c := w.filePRD(helpPRD, string(label.Ready), light)
	w.gh.AddBlockedBy(c, a)
	peak := w.watchPeaks(a, b, c)
	w.start()

	blocked := w.box(c, "builder")
	w.waitFor("both free PRDs' PRs", aStart+aRun, func() bool {
		if w.status(c) != label.Ready || w.exists(blocked) {
			t.Fatalf("PRD #%d started while its blocker #%d was open.\n%s", c, a, w.describe())
		}
		_, okA := w.openPR(a)
		_, okB := w.openPR(b)
		return okA && okB
	})
	peak.mu.Lock()
	working := peak.working
	peak.mu.Unlock()
	if working < 2 {
		t.Errorf("at most %d PRDs worked at once, want the two free ones together", working)
	}
	w.holds("the blocked PRD waiting", 2*w.cfg.interval, func() bool { return w.status(c) == label.Ready && !w.exists(blocked) })

	prA, _ := w.openPR(a)
	w.gh.MergePull(prA.Number)
	merged := w.head("main")
	w.waitFor("the blocked PRD to start once its blocker closed", 3*w.cfg.interval+aStart, func() bool { return w.started(blocked) })
	if got := w.startBase(blocked); got != merged {
		t.Errorf("the blocked PRD was built on %s, want main with its blocker merged, %s", got, merged)
	}
	w.gh.CloseIssue(c)
	w.gh.CloseIssue(b)
}

func TestE2EIntake(t *testing.T) {
	e2e(t, false)
	w := newWorld(t, "intake", project(), func(c *config) { c.ciGrace = time.Hour })
	untouched := map[int][]string{}
	for _, labels := range [][]string{{string(label.Draft)}, nil, {string(label.Draft), string(label.Ready)}, {string(label.Stuck)}} {
		n := w.filePRD(farewellPRD, labels...)
		untouched[n] = w.gh.Issue(n).Labels
	}
	plain := w.filePRD(shoutPRD, string(label.Ready))
	heavy := w.filePRD(helpPRD, string(label.Ready), string(label.Light), string(label.Heavy))
	w.start()

	w.holdAsTheyStart(aStart, 2, w.box(plain, "builder"), w.box(heavy, "builder"))
	for n, effort := range map[int]string{plain: "--effort high", heavy: "--effort xhigh"} {
		if args := w.claudeArgs(w.box(n, "builder")); !strings.Contains(args, effort) {
			t.Errorf("PRD #%d runs claude as %q, want %s", n, args, effort)
		}
	}
	w.holds("the PRDs no worker may take", 3*w.cfg.interval, func() bool {
		for n, labels := range untouched {
			if !slices.Equal(w.gh.Issue(n).Labels, labels) || w.exists(w.box(n, "builder")) {
				return false
			}
		}
		return true
	})

	box := w.box(plain, "builder")
	first := w.startedAt(box)
	w.relabel(plain, label.Working, label.Ready)
	w.waitFor("the relabeled PRD to start over", aRound+aStart, func() bool {
		at := w.startedAt(box)
		return !at.IsZero() && at.After(first)
	})
	w.wantStatus(plain, label.Working)
	w.gh.CloseIssue(plain)
	w.gh.CloseIssue(heavy)
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
	const grace = 15 * time.Second
	w := newWorld(t, "restart", project(), func(c *config) { c.ciGrace = grace })
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

	reviewer := w.box(prd, "reviewer")
	w.waitFor("the review to start", grace+aStart, func() bool { return w.started(reviewer) })
	w.stop()
	w.waitFor("the review to end while exomecha is stopped", aRun, func() bool { return w.ended(reviewer) })
	first = w.startedAt(reviewer)
	w.start()
	w.waitFor("the review to be collected", 2*aRound, func() bool {
		if at := w.startedAt(reviewer); !at.IsZero() && !at.Equal(first) {
			restarted = true
		}
		return w.status(prd) != label.Review
	})
	if restarted {
		t.Error("reviewer started claude again after the restart instead of collecting the run")
	}
	w.waitFor("the reviewer's sandbox and folder to go", aRound, func() bool { return !w.exists(reviewer) && !exists(w.runDir(reviewer)) })
	if got := w.gh.Labels(); len(got) != 10 {
		t.Errorf("labels after a restart = %q, want the same ten", got)
	}
}
