package worker

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const approve = `{"verdict": "approve", "body": "fine"}`

func (e *env) reviewer() *reviewer { return &reviewer{e.deps, e.runs} }

func (e *env) review(n int) {
	e.t.Helper()
	if err := e.reviewer().Handle(e.t.Context(), n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) reviewerBox(n int) string { return e.runs.name(reviewerRole, n) }

// reviewPRD sets up a PRD in review, with a pushed branch and an open, mergeable PR. With no
// CI runs set, its head commit is an hour old.
func (e *env) reviewPRD() (prd, pr int, sha string) {
	prd = e.addPRD("PRD", "PRD body", label.PRD, string(label.Review))
	sha = gittest.Commit(e.t, e.origin, branchName(prd), "main", map[string]string{"feature.txt": "x\n"})
	pr = e.gh.AddPull(branchName(prd), "main")
	e.gh.SetMergeable(pr, new(true))
	return prd, pr, sha
}

// reviewing is a PRD in review with reviewer's run started for it.
func (e *env) reviewing() (prd, pr int, sha, name string) {
	e.t.Helper()
	prd, pr, sha = e.reviewPRD()
	e.review(prd)
	name = e.reviewerBox(prd)
	e.wantState(name, running)
	return prd, pr, sha, name
}

func (e *env) wantUntouched(prd, pr int) {
	e.t.Helper()
	if e.sbx.exists(e.reviewerBox(prd)) {
		e.t.Error("a review started")
	}
	e.wantStatus(prd, label.Review)
	if p := e.gh.Pull(pr); p.Merged || len(p.Reviews) != 0 {
		e.t.Errorf("PR changed: merged=%v reviews=%d", p.Merged, len(p.Reviews))
	}
}

func TestReviewerPickSkipsConflictingPRs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	mergeable, _, _ := e.reviewPRD()
	computing, pr, _ := e.reviewPRD()
	e.gh.SetMergeable(pr, nil)
	conflicting, _, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.approvedPRD()
	approvedConflicting, apr := e.approvedPRD()
	e.gh.SetMergeable(apr, new(false))
	approvedMerged, mpr := e.approvedPRD()
	e.gh.MergePull(mpr)
	approvedClosed, cpr := e.approvedPRD()
	e.gh.ClosePull(cpr)

	picked, err := e.reviewer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "reviewer Pick", picked, []int{mergeable, computing, approvedMerged, approvedClosed})

	picked, err = e.resolver().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "resolver Pick", picked, []int{conflicting, approvedConflicting})
}

func TestReviewerPicksEveryPRDWithARun(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	conflicting, _, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.sbx.add(e.reviewerBox(conflicting))
	e.sbx.add(e.runs.name(builderRole, 99))

	picked, err := e.reviewer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{conflicting})
}

func TestReviewerStuckWhenPRClosedByHand(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _ := e.reviewPRD()
	e.gh.ClosePull(pr)

	e.review(prd)

	if e.sbx.exists(e.reviewerBox(prd)) {
		t.Error("a review started for a closed PR")
	}
	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "closed without being merged", label.Ready)
}

func TestReviewerWaitsForMergeable(t *testing.T) {
	t.Parallel()
	for _, mergeable := range []*bool{nil, new(false)} {
		e := newEnv(t, map[string]string{"app.txt": "v1\n"})
		prd, pr, _ := e.reviewPRD()
		e.gh.SetMergeable(pr, mergeable)

		e.review(prd)
		e.wantUntouched(prd, pr)
	}
}

func TestReviewerWaitsForRunningCI(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, sha := e.reviewPRD()
	e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "in_progress"})

	e.review(prd)
	e.wantUntouched(prd, pr)
}

func TestReviewerRequeuesOnFailedCI(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, sha := e.reviewPRD()
	e.gh.SetRuns(sha,
		githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure", URL: "http://ci/test"},
		githubtest.Run{Name: "lint", Status: "completed", Conclusion: "success", URL: "http://ci/lint"},
	)

	e.review(prd)

	if e.sbx.exists(e.reviewerBox(prd)) {
		t.Error("a review started on failed CI")
	}
	e.wantStatus(prd, label.Ready)
	reviews := e.gh.Pull(pr).Reviews
	if len(reviews) != 1 {
		t.Fatalf("reviews = %+v, want one", reviews)
	}
	if n, _, _ := github.ParseNote(reviews[0].Body); reviews[0].Event != "COMMENT" || n != (github.Note{Worker: "reviewer", Kind: github.KindCI}) {
		t.Errorf("review = %+v, want a COMMENT review with reviewer's CI note", reviews[0])
	}
	wantContains(t, "review", reviews[0].Body, "test: failure http://ci/test")
	wantNotContains(t, "review", reviews[0].Body, "lint")
}

func TestReviewerSendsAPersonThePRAfterTheLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.deps.ReviewLimit = 2
	prd, pr, sha := e.reviewPRD()
	e.gh.AddReview(pr, github.Marker+"\nrename the function")
	e.gh.AddReview(pr, github.Marker+"\nCI failed.")
	e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure", URL: "http://ci/test"})

	e.review(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), fmt.Sprintf("sent PR #%d back to builder 2 times, the limit", pr), label.Ready)
	reviews := e.gh.Pull(pr).Reviews
	if len(reviews) != 3 {
		t.Fatalf("%d reviews, want the third one left on the PR too", len(reviews))
	}
	wantContains(t, "the third review", reviews[2].Body, "test: failure http://ci/test")
}

// An approval is a person's turn, so the send-backs before it do not count.
func TestReviewerCountsSendBacksSinceTheApproval(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.deps.ReviewLimit = 1
	prd, pr, _ := e.reviewPRD()
	e.gh.AddReview(pr, github.Marker+"\nrename the function")
	if err := e.deps.GitHub.AddApproval(t.Context(), pr, "fine"); err != nil {
		t.Fatal(err)
	}
	e.review(prd)
	name := e.reviewerBox(prd)
	e.play(name, nil, `{"verdict": "changes", "body": "a person asked for more"}`)

	e.review(prd)

	e.wantStatus(prd, label.Ready)
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("%d comments on the PRD, want none", n)
	}
}

func TestReviewerWaitsForCIToAppear(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, sha := e.reviewPRD()
	e.gh.SetCommitTime(sha, time.Now().Add(-time.Minute))

	e.review(prd)
	e.wantUntouched(prd, pr)

	e.gh.SetCommitTime(sha, time.Now().Add(-10*time.Minute))
	e.review(prd)
	e.wantState(e.reviewerBox(prd), running)
}

func TestReviewerWaitsForTheGivenCIGrace(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.deps.CIGrace = 30 * time.Minute
	prd, pr, sha := e.reviewPRD()
	e.gh.SetCommitTime(sha, time.Now().Add(-10*time.Minute))

	e.review(prd)
	e.wantUntouched(prd, pr)

	e.gh.SetCommitTime(sha, time.Now().Add(-time.Hour))
	e.review(prd)
	e.wantState(e.reviewerBox(prd), running)
}

func TestReviewerApproveLeavesThePRToAPerson(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, sha := e.reviewPRD()
	e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "completed", Conclusion: "success"})
	e.review(prd)
	name := e.reviewerBox(prd)
	repo := e.sbx.repo(name)
	if got := gittest.Run(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != branchName(prd) {
		t.Errorf("sandbox repository is on %q, want the PR branch", got)
	}
	if got := gittest.Run(t, repo, "rev-parse", "HEAD"); got != sha {
		t.Errorf("sandbox repository is at %s, want the PR head %s", got, sha)
	}
	if got := gittest.Run(t, repo, "rev-parse", "origin/main"); got != gittest.Head(e.origin, "main") {
		t.Errorf("origin/main in the sandbox = %s", got)
	}
	wantContains(t, "claude input", e.readCtl(name, "input.md"), "PRD body", "git diff origin/main...HEAD")

	e.play(name, nil, approve)
	e.review(prd)

	e.wantApproved(prd, pr)
	e.wantRemoved(name)
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("%d comments, want none", n)
	}
}

// wantApproved checks that the PRD waits for a person, with exomecha's approval as the PR's last
// review and the PR not merged.
func (e *env) wantApproved(prd, pr int) {
	e.t.Helper()
	e.wantStatus(prd, label.Approved)
	p := e.gh.Pull(pr)
	if p.Merged || !p.Open {
		e.t.Errorf("PR merged=%v open=%v, want it open for a person", p.Merged, p.Open)
	}
	if len(p.Reviews) == 0 {
		e.t.Fatal("no review on the PR")
	}
	last := p.Reviews[len(p.Reviews)-1]
	n, text, _ := github.ParseNote(last.Body)
	if last.Event != "COMMENT" || n != (github.Note{Worker: "reviewer", Kind: github.KindApproved}) ||
		!strings.HasPrefix(text, "exomecha's reviewer approved this PR.") || !strings.HasSuffix(text, "fine") {
		e.t.Errorf("last review = %+v, want exomecha's approval with the AI's summary", last)
	}
}

func TestReviewerReadsAPersonsCommentsOnThePR(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _ := e.reviewPRD()
	if err := e.deps.GitHub.AddReview(t.Context(), pr, github.Note{Worker: "reviewer", Kind: github.KindChanges}, "add a log line"); err != nil {
		t.Fatal(err)
	}
	e.gh.AddLineComment(pr, "feature.txt", 1, "keep it short")
	e.gh.AddPullComment(pr, "also log it")
	e.gh.AddOutsiderReview(pr, "an outsider's review")

	e.review(prd)

	input := e.readCtl(e.reviewerBox(prd), "input.md")
	onPR := fmt.Sprintf("PR #%d", pr)
	wantContains(t, "claude input", input, "## Record", "git diff origin/main...HEAD",
		"(reviewer changes, "+onPR+")\n\nadd a log line",
		"(person, "+onPR+", on feature.txt line 1)\n\nkeep it short", "(person, "+onPR+")\n\nalso log it")
	wantNotContains(t, "claude input", input, "an outsider's review")
}

// approvedPRD is a PRD whose PR exomecha approved, waiting for a person.
func (e *env) approvedPRD() (prd, pr int) {
	prd, pr, _ = e.reviewPRD()
	e.gh.Relabel(prd, string(label.Review), string(label.Approved))
	return prd, pr
}

func TestReviewerLeavesAnApprovedPRDWhileItsPRIsOpen(t *testing.T) {
	t.Parallel()
	for _, mergeable := range []*bool{new(true), nil, new(false)} {
		e := newEnv(t, map[string]string{"app.txt": "v1\n"})
		prd, pr := e.approvedPRD()
		e.gh.SetMergeable(pr, mergeable)

		e.review(prd)

		if e.sbx.exists(e.reviewerBox(prd)) {
			t.Error("a review started for an approved PRD")
		}
		e.wantStatus(prd, label.Approved)
		if p := e.gh.Pull(pr); len(p.Reviews) != 0 || len(e.gh.Issue(prd).Comments) != 0 {
			t.Errorf("reviews=%d comments=%d, want none", len(p.Reviews), len(e.gh.Issue(prd).Comments))
		}
	}
}

// A PR whose base is not the default branch leaves its PRD open when a person merges it.
func TestReviewerFinishesAnApprovedPRDOnceAPersonMerges(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr := e.approvedPRD()
	e.gh.MergePull(pr)

	e.review(prd)

	e.wantFinished(prd)
}

// Closing the PRD failed after a person merged. The next try closes it.
func TestReviewerFinishesAnApprovedPRDWhoseCloseFailed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr := e.approvedPRD()
	e.gh.MergePull(pr)
	e.gh.Fail(closeRoute, http.StatusBadGateway)

	if err := e.reviewer().Handle(t.Context(), prd); err == nil {
		t.Fatal("Handle returned nil with the close failing")
	}
	e.wantStatus(prd, label.Approved)

	e.gh.Fail(closeRoute, 0)
	e.review(prd)
	e.wantFinished(prd)
}

func TestReviewerStuckWhenAnApprovedPRIsClosedByHand(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr := e.approvedPRD()
	e.gh.ClosePull(pr)

	e.review(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "closed without being merged", label.Ready)
}

func (e *env) wantFinished(prd int) {
	e.t.Helper()
	if got := e.gh.Issue(prd); got.Open || got.StateReason != "completed" {
		e.t.Errorf("PRD open=%v reason=%q, want closed as completed", got.Open, got.StateReason)
	}
	e.wantStatus(prd, label.None)
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		e.t.Errorf("%d comments, want none", n)
	}
}

func TestReviewerChangesRequeues(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _, name := e.reviewing()
	e.play(name, nil, `{"verdict": "changes", "body": "rename the function"}`)

	e.review(prd)

	e.wantStatus(prd, label.Ready)
	p := e.gh.Pull(pr)
	if p.Merged {
		t.Error("PR merged despite requested changes")
	}
	if len(p.Reviews) != 1 || p.Reviews[0].Body != github.Noted(github.Note{Worker: "reviewer", Kind: github.KindChanges}, "rename the function") {
		t.Errorf("reviews = %+v", p.Reviews)
	}
	e.wantRemoved(name)
}

// The review was of a head that is no longer the PR's. The next round reviews the new head.
func TestReviewerDiscardsWhenThePRMoved(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _, name := e.reviewing()
	pushed := gittest.Commit(t, e.origin, branchName(prd), "", map[string]string{"other.txt": "a person's push\n"})
	e.play(name, nil, approve)

	e.review(prd)

	e.wantRemoved(name)
	e.wantStatus(prd, label.Review)
	if p := e.gh.Pull(pr); p.Merged || len(p.Reviews) != 0 {
		t.Errorf("PR merged=%v reviews=%d, want the review thrown away", p.Merged, len(p.Reviews))
	}
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("a push during the review left %d comments", n)
	}
	e.review(prd)
	if got := gittest.Run(t, e.sbx.repo(name), "rev-parse", "HEAD"); got != pushed {
		t.Errorf("the next review is of %s, want the new head %s", got, pushed)
	}
}

func TestReviewerFailedRunGoesStuckBackToReview(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _, name := e.reviewing()
	e.writeCtl(name, "stream.jsonl", `{"type":"system","subtype":"init","permissionMode":"auto"}`+"\n"+
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}`+"\n")
	e.writeCtl(name, "exit", "1\n")

	e.review(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "The AI's run failed because claude ended with an error", label.Review)
	e.wantRemoved(name)
	e.wantKept(name, "stream.jsonl")
	if e.gh.Pull(pr).Merged {
		t.Error("PR merged after a failed review")
	}
}

func TestReviewerUnknownVerdictFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _, name := e.reviewing()
	e.play(name, nil, `{"verdict": "maybe", "body": ""}`)

	e.review(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "The AI's run failed because "+badResult, label.Review)
	e.wantKept(name, "stream.jsonl")
	if e.gh.Pull(pr).Merged {
		t.Error("PR merged on an unknown verdict")
	}
}

// approving is a PRD whose review approved its PR, with the run finished and not yet collected.
func (e *env) approving() (prd, pr int, name string) {
	e.t.Helper()
	prd, pr, _, name = e.reviewing()
	e.play(name, nil, approve)
	return prd, pr, name
}

func (e *env) wantResultKept(prd, pr int, name string) {
	e.t.Helper()
	e.wantState(name, finished)
	if e.sbx.exists(name) {
		e.t.Error("the finished review still holds its sandbox")
	}
	e.wantStatus(prd, label.Review)
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		e.t.Errorf("%d comments, want none", n)
	}
}

const closeRoute = "PATCH /repos/{owner}/{repo}/issues/{n}"

// The approval is on the PR and the status change failed. The next try moves the PRD without
// another review.
func TestReviewerApprovalRetriesTheStatusWithoutANewReview(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, name := e.approving()
	e.gh.Fail(labelsRoute, http.StatusBadGateway)

	if err := e.reviewer().Handle(t.Context(), prd); err == nil {
		t.Fatal("Handle returned nil with the status change failing")
	}
	e.wantResultKept(prd, pr, name)

	e.gh.Fail(labelsRoute, 0)
	e.review(prd)
	if e.sbx.exists(name) {
		t.Error("a new review started")
	}
	e.wantApproved(prd, pr)
	e.wantRemoved(name)
}

// A finished review gives its slot back at once, and a new exomecha still collects it from the
// run's directory.
func TestReviewerFinishesARunAfterRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _, name := e.reviewing()
	e.play(name, nil, approve)
	e.gh.SetMergeable(pr, nil)
	e.review(prd)
	if e.sbx.exists(name) {
		t.Fatal("the finished review still holds its sandbox")
	}

	e.newRuns()
	e.gh.SetMergeable(pr, new(true))
	picked, err := e.reviewer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{prd})
	e.review(prd)

	e.wantApproved(prd, pr)
	e.wantRemoved(name)
}

// Over the usage limit, no review starts, and a review already going is still collected.
func TestReviewerHarvestsWhileTheGateIsClosed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	running, pr, _, name := e.reviewing()
	f := newFakeUsage(e.clock)
	f.usage.FiveHour.Used = 0.99
	e.runs.gate = f.gate
	waiting, _, _ := e.reviewPRD()

	e.review(waiting)
	e.play(name, nil, approve)
	e.review(running)

	if e.sbx.exists(e.reviewerBox(waiting)) {
		t.Error("a review started while the gate was closed")
	}
	if e.statuses(running)[0] != string(label.Approved) || len(e.gh.Pull(pr).Reviews) != 1 {
		t.Error("the review already going was not collected")
	}
}
