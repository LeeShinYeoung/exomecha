package worker

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const approve = `{"verdict": "approve", "body": "fine"}`

func (e *env) reviewer() *reviewer { return &reviewer{e.deps, e.runs} }

// review runs one reviewer round for the Task and fails the test on an error.
func (e *env) review(n int) {
	e.t.Helper()
	if err := e.reviewer().Handle(e.t.Context(), n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) reviewerBox(n int) string { return e.runs.name(reviewerRole, n) }

// reviewTask sets up a Task in review under a PRD, with a pushed branch and an open,
// mergeable PR. With no CI runs set, its head commit is an hour old.
func (e *env) reviewTask() (task, pr int, sha string) {
	prd := e.gh.AddIssue("PRD", "why we decided this", label.PRD, label.Tasked)
	task = e.gh.AddIssue("Task", "task body", label.Task, string(label.Review))
	e.gh.AddSubIssue(prd, task)
	sha = gittest.Commit(e.t, e.origin, branchName(task), "main", map[string]string{"feature.txt": "x\n"})
	pr = e.gh.AddPull(branchName(task), "main")
	e.gh.SetMergeable(pr, new(true))
	return task, pr, sha
}

// reviewing is a Task in review with reviewer's run started for it.
func (e *env) reviewing() (task, pr int, sha, name string) {
	e.t.Helper()
	task, pr, sha = e.reviewTask()
	e.review(task)
	name = e.reviewerBox(task)
	e.wantState(name, running)
	return task, pr, sha, name
}

func (e *env) wantUntouched(task, pr int) {
	e.t.Helper()
	if e.sbx.exists(e.reviewerBox(task)) {
		e.t.Error("a review started")
	}
	e.wantStatus(task, label.Review)
	if p := e.gh.Pull(pr); p.Merged || len(p.Reviews) != 0 {
		e.t.Errorf("PR changed: merged=%v reviews=%d", p.Merged, len(p.Reviews))
	}
}

func TestReviewerPickSkipsConflictingPRs(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	mergeable, _, _ := e.reviewTask()
	computing, pr, _ := e.reviewTask()
	e.gh.SetMergeable(pr, nil)
	conflicting, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	picked, err := e.reviewer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "reviewer Pick", picked, []int{mergeable, computing})

	picked, err = e.resolver().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "resolver Pick", picked, []int{conflicting})
}

func TestReviewerPicksEveryTaskWithARun(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	conflicting, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.sbx.add(e.reviewerBox(conflicting))
	e.sbx.add(e.runs.name(builderRole, 99))

	picked, err := e.reviewer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{conflicting})
}

func TestReviewerStuckWhenPRClosedByHand(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, _ := e.reviewTask()
	e.gh.ClosePull(pr)

	e.review(task)

	if e.sbx.exists(e.reviewerBox(task)) {
		t.Error("a review started for a closed PR")
	}
	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "closed without being merged", label.Pending)
}

func TestReviewerWaitsForMergeable(t *testing.T) {
	for _, mergeable := range []*bool{nil, new(false)} {
		e := newEnv(t, map[string]string{"app.txt": "v1\n"})
		task, pr, _ := e.reviewTask()
		e.gh.SetMergeable(pr, mergeable)

		e.review(task)
		e.wantUntouched(task, pr)
	}
}

func TestReviewerWaitsForRunningCI(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, sha := e.reviewTask()
	e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "in_progress"})

	e.review(task)
	e.wantUntouched(task, pr)
}

func TestReviewerRequeuesOnFailedCI(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, sha := e.reviewTask()
	e.gh.SetRuns(sha,
		githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure", URL: "http://ci/test"},
		githubtest.Run{Name: "lint", Status: "completed", Conclusion: "success", URL: "http://ci/lint"},
	)

	e.review(task)

	if e.sbx.exists(e.reviewerBox(task)) {
		t.Error("a review started on failed CI")
	}
	e.wantStatus(task, label.Queued)
	reviews := e.gh.Pull(pr).Reviews
	if len(reviews) != 1 {
		t.Fatalf("reviews = %+v, want one", reviews)
	}
	if reviews[0].Event != "COMMENT" || !strings.HasPrefix(reviews[0].Body, github.Marker) {
		t.Errorf("review = %+v, want a COMMENT review starting with the marker", reviews[0])
	}
	wantContains(t, "review", reviews[0].Body, "test: failure http://ci/test")
	wantNotContains(t, "review", reviews[0].Body, "lint")
}

func TestReviewerWaitsForCIToAppear(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, sha := e.reviewTask()
	e.gh.SetCommitTime(sha, time.Now().Add(-time.Minute))

	e.review(task)
	e.wantUntouched(task, pr)

	e.gh.SetCommitTime(sha, time.Now().Add(-10*time.Minute))
	e.review(task)
	e.wantState(e.reviewerBox(task), running)
}

func TestReviewerApproveMerges(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, sha := e.reviewTask()
	e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "completed", Conclusion: "success"})
	e.review(task)
	name := e.reviewerBox(task)
	repo := e.sbx.repo(name)
	if got := gittest.Run(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != branchName(task) {
		t.Errorf("sandbox repository is on %q, want the PR branch", got)
	}
	if got := gittest.Run(t, repo, "rev-parse", "HEAD"); got != sha {
		t.Errorf("sandbox repository is at %s, want the PR head %s", got, sha)
	}
	if got := gittest.Run(t, repo, "rev-parse", "origin/main"); got != gittest.Head(e.origin, "main") {
		t.Errorf("origin/main in the sandbox = %s", got)
	}
	wantContains(t, "claude input", e.readCtl(name, "input.md"), "task body", "why we decided this", "git diff origin/main...HEAD")

	e.play(name, nil, approve)
	e.review(task)

	p := e.gh.Pull(pr)
	if !p.Merged || p.MergeMethod != "merge" || p.MergedSHA != sha {
		t.Errorf("PR merged=%v method=%q sha=%q, want merged with merge at the reviewed head %s", p.Merged, p.MergeMethod, p.MergedSHA, sha)
	}
	e.wantFinished(task)
	e.wantRemoved(name)
}

// wantFinished checks that the Task is closed as completed, with no status and no comment.
func (e *env) wantFinished(task int) {
	e.t.Helper()
	if got := e.gh.Issue(task); got.Open || got.StateReason != "completed" {
		e.t.Errorf("task open=%v reason=%q, want closed as completed", got.Open, got.StateReason)
	}
	e.wantStatus(task, label.None)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		e.t.Errorf("%d comments, want none", n)
	}
}

func TestReviewerChangesRequeues(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, _, name := e.reviewing()
	e.play(name, nil, `{"verdict": "changes", "body": "rename the function"}`)

	e.review(task)

	e.wantStatus(task, label.Queued)
	p := e.gh.Pull(pr)
	if p.Merged {
		t.Error("PR merged despite requested changes")
	}
	if len(p.Reviews) != 1 || p.Reviews[0].Body != github.Marker+"\nrename the function" {
		t.Errorf("reviews = %+v", p.Reviews)
	}
	e.wantRemoved(name)
}

// The review was of a head that is no longer the PR's. The next round reviews the new head.
func TestReviewerDiscardsWhenThePRMoved(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, _, name := e.reviewing()
	pushed := gittest.Commit(t, e.origin, branchName(task), "", map[string]string{"other.txt": "a person's push\n"})
	e.play(name, nil, approve)

	e.review(task)

	e.wantRemoved(name)
	e.wantStatus(task, label.Review)
	if p := e.gh.Pull(pr); p.Merged || len(p.Reviews) != 0 {
		t.Errorf("PR merged=%v reviews=%d, want the review thrown away", p.Merged, len(p.Reviews))
	}
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("a push during the review left %d comments", n)
	}
	e.review(task)
	if got := gittest.Run(t, e.sbx.repo(name), "rev-parse", "HEAD"); got != pushed {
		t.Errorf("the next review is of %s, want the new head %s", got, pushed)
	}
}

func TestReviewerFailedRunGoesStuckBackToReview(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, _, name := e.reviewing()
	e.writeCtl(name, "stream.jsonl", `{"type":"system","subtype":"init","permissionMode":"auto"}`+"\n"+
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}`+"\n")
	e.writeCtl(name, "exit", "1\n")

	e.review(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "The AI's run failed because claude ended with an error", label.Review)
	e.wantRemoved(name)
	e.wantKept(name, "stream.jsonl")
	if e.gh.Pull(pr).Merged {
		t.Error("PR merged after a failed review")
	}
}

func TestReviewerUnknownVerdictFails(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, _, name := e.reviewing()
	e.play(name, nil, `{"verdict": "maybe", "body": ""}`)

	e.review(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "The AI's run failed because "+badResult, label.Review)
	e.wantKept(name, "stream.jsonl")
	if e.gh.Pull(pr).Merged {
		t.Error("PR merged on an unknown verdict")
	}
}

const mergeRoute = "PUT /repos/{owner}/{repo}/pulls/{n}/merge"

// approved is a Task whose review approved its PR, with the run finished and not yet collected.
func (e *env) approved() (task, pr int, name string) {
	e.t.Helper()
	task, pr, _, name = e.reviewing()
	e.play(name, nil, approve)
	return task, pr, name
}

// wantResultKept checks that the finished review is still there to collect, with no new run started.
func (e *env) wantResultKept(task, pr int, name string) {
	e.t.Helper()
	e.wantState(name, finished)
	if e.sbx.exists(name) {
		e.t.Error("the finished review still holds its sandbox")
	}
	e.wantStatus(task, label.Review)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		e.t.Errorf("%d comments, want none", n)
	}
	if e.gh.Pull(pr).Merged {
		e.t.Error("PR merged")
	}
}

func TestReviewerMergeRefusedByARuleGoesStuck(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, name := e.approved()
	e.gh.RefuseMerge(pr, "At least 1 approving review is required by reviewers with write access.", new(true))

	e.review(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "At least 1 approving review is required by reviewers with write access.", label.Review)
	e.wantRemoved(name)
	if e.gh.Pull(pr).Merged {
		t.Error("PR merged")
	}
}

// GitHub refuses a PR that turned out to conflict the same way. That is resolver's to fix.
func TestReviewerMergeRefusedOnAConflictLeavesTheTaskToResolver(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, name := e.approved()
	e.gh.RefuseMerge(pr, "Pull Request is not mergeable", new(false))

	e.review(task)
	e.wantResultKept(task, pr, name)
	picked, err := e.resolver().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "resolver Pick", picked, []int{task})

	e.review(task)
	e.wantRemoved(name)
	e.wantStatus(task, label.Review)
}

func TestReviewerMergeRefusedWhileGitHubChecksKeepsTheResult(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, name := e.approved()
	e.gh.RefuseMerge(pr, "Pull Request is not mergeable", nil)

	if err := e.reviewer().Handle(t.Context(), task); err == nil {
		t.Fatal("Handle returned nil while GitHub was still checking the PR")
	}
	e.wantResultKept(task, pr, name)

	e.gh.SetMergeable(pr, new(true))
	e.review(task)
	if !e.gh.Pull(pr).Merged {
		t.Error("PR not merged from the kept result")
	}
}

const closeRoute = "PATCH /repos/{owner}/{repo}/issues/{n}"

// The merge went through and closing the Task did not. The Task stays in review with its PR
// merged, and the next try closes it.
func TestReviewerFinishesATaskWhoseCloseFailedAfterTheMerge(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, name := e.approved()
	e.gh.Fail(closeRoute, http.StatusBadGateway)

	if err := e.reviewer().Handle(t.Context(), task); err == nil {
		t.Fatal("Handle returned nil with the close failing")
	}
	if !e.gh.Pull(pr).Merged {
		t.Fatal("PR not merged")
	}
	e.wantStatus(task, label.Review)

	e.gh.Fail(closeRoute, 0)
	e.review(task)
	e.wantFinished(task)
	e.wantRemoved(name)
}

// The Task closed and its status stayed. No worker takes a closed Task, and the run goes.
func TestReviewerLeavesATaskWhoseStatusClearFailedAfterTheMerge(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, _, name := e.approved()
	e.gh.Fail(labelsRoute, http.StatusBadGateway)

	if err := e.reviewer().Handle(t.Context(), task); err == nil {
		t.Fatal("Handle returned nil with the status clear failing")
	}
	e.gh.Fail(labelsRoute, 0)
	picked, err := e.reviewer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{task})
	e.review(task)

	if got := e.gh.Issue(task); got.Open || got.StateReason != "completed" || len(got.Comments) != 0 {
		t.Errorf("task open=%v reason=%q comments=%d, want closed as completed with no comment", got.Open, got.StateReason, len(got.Comments))
	}
	e.wantRemoved(name)
}

// Another merge into the base branch went in at the same moment. The next try merges.
func TestReviewerMergeThatRacedAnotherKeepsTheResult(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, name := e.approved()
	e.gh.RefuseMerge(pr, "Base branch was modified. Review and try the merge again.", new(true))

	if err := e.reviewer().Handle(t.Context(), task); err == nil {
		t.Fatal("Handle returned nil for a merge that raced another")
	}
	e.wantResultKept(task, pr, name)

	e.review(task)
	if !e.gh.Pull(pr).Merged {
		t.Error("PR not merged from the kept result")
	}
}

// Something was pushed between the check and the merge. The next round reviews the new head.
func TestReviewerMergeOfAMovedHeadStartsOver(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, name := e.approved()
	e.gh.Fail(mergeRoute, http.StatusConflict)

	e.review(task)

	e.wantRemoved(name)
	e.wantStatus(task, label.Review)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("%d comments, want none", n)
	}
	e.gh.Fail(mergeRoute, 0)
	e.review(task)
	e.wantState(name, running)
	if e.gh.Pull(pr).Merged {
		t.Error("PR merged without a new review")
	}
}

func TestReviewerMergeErrorRetriesWithoutANewReview(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, name := e.approved()
	e.gh.Fail(mergeRoute, http.StatusBadGateway)

	if err := e.reviewer().Handle(t.Context(), task); err == nil {
		t.Fatal("Handle returned nil for a failed merge")
	}
	e.wantResultKept(task, pr, name)

	e.gh.Fail(mergeRoute, 0)
	e.review(task)
	if !e.gh.Pull(pr).Merged {
		t.Error("PR not merged from the kept result")
	}
	if e.sbx.exists(name) {
		t.Error("a new review started")
	}
	e.wantRemoved(name)
}

// A finished review gives its slot back at once, and a new exomecha still collects it from the
// run's directory.
func TestReviewerFinishesARunAfterRestart(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, _, name := e.reviewing()
	e.play(name, nil, approve)
	e.gh.SetMergeable(pr, nil)
	e.review(task)
	if e.sbx.exists(name) {
		t.Fatal("the finished review still holds its sandbox")
	}

	e.runs = newRuns(e.deps, nil)
	e.runs.now = e.clock.now
	e.gh.SetMergeable(pr, new(true))
	picked, err := e.reviewer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{task})
	e.review(task)

	if !e.gh.Pull(pr).Merged {
		t.Error("PR not merged")
	}
	e.wantRemoved(name)
}

// Over the usage limit, no review starts, and a review already going is still collected.
func TestReviewerHarvestsWhileTheGateIsClosed(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	running, pr, _, name := e.reviewing()
	e.runs.gate = &Gate{
		Check: func(context.Context) (agent.Usage, error) {
			return agent.Usage{Known: true, FiveHour: agent.Window{Used: 0.99}}, nil
		},
		FiveHour: 0.9,
		Weekly:   0.8,
	}
	waiting, _, _ := e.reviewTask()

	e.review(waiting)
	e.play(name, nil, approve)
	e.review(running)

	if e.sbx.exists(e.reviewerBox(waiting)) {
		t.Error("a review started while the gate was closed")
	}
	if !e.gh.Pull(pr).Merged {
		t.Error("the review already going was not collected")
	}
}

func TestReviewerSweepClosesFinishedPRDs(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	problem := e.gh.AddIssue("problem", "", label.Problem)
	prd1 := e.gh.AddIssue("PRD 1", "", label.PRD, label.Tasked)
	prd2 := e.gh.AddIssue("PRD 2", "", label.PRD, label.Tasked)
	empty := e.gh.AddIssue("PRD without tasks", "", label.PRD, label.Tasked)
	e.gh.AddSubIssue(problem, prd1)
	e.gh.AddSubIssue(problem, prd2)
	t1 := e.gh.AddIssue("task 1", "", label.Task)
	t2 := e.gh.AddIssue("task 2", "", label.Task, string(label.Review))
	e.gh.AddSubIssue(prd1, t1)
	e.gh.AddSubIssue(prd2, t2)
	e.gh.CloseIssue(t1)
	r := e.reviewer()

	if err := r.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.gh.Issue(prd1).Open {
		t.Error("PRD 1 is open with all its tasks closed")
	}
	if !e.gh.Issue(prd2).Open || !e.gh.Issue(problem).Open || !e.gh.Issue(empty).Open {
		t.Error("closed a PRD or problem that still has open work, or a PRD with no tasks")
	}

	e.gh.CloseIssue(t2)
	if err := r.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.gh.Issue(prd2).Open || e.gh.Issue(problem).Open {
		t.Error("PRD 2 or the problem is still open after every task closed")
	}
}
