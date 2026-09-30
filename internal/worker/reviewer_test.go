package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/github/githubtest"
	"github.com/LeeShinYeoung/coworker/internal/label"
)

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

func (e *env) wantUntouched(task, pr int) {
	e.t.Helper()
	if n := e.claude.callCount(); n != 0 {
		e.t.Errorf("claude called %d times, want none", n)
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

	picked, err := (&reviewer{e.deps}).Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "reviewer Pick", picked, []int{mergeable, computing})

	picked, err = (&resolver{e.deps}).Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "resolver Pick", picked, []int{conflicting})
}

func TestReviewerStuckWhenPRClosedByHand(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, _ := e.reviewTask()
	e.gh.ClosePull(pr)

	if err := (&reviewer{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	if n := e.claude.callCount(); n != 0 {
		t.Errorf("claude called %d times for a closed PR", n)
	}
	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "closed without being merged", label.Pending)
}

func TestReviewerWaitsForMergeable(t *testing.T) {
	for _, mergeable := range []*bool{nil, new(false)} {
		e := newEnv(t, map[string]string{"app.txt": "v1\n"})
		task, pr, _ := e.reviewTask()
		e.gh.SetMergeable(pr, mergeable)

		if err := (&reviewer{e.deps}).Handle(t.Context(), task); err != nil {
			t.Fatal(err)
		}
		e.wantUntouched(task, pr)
	}
}

func TestReviewerWaitsForRunningCI(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, sha := e.reviewTask()
	e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "in_progress"})

	if err := (&reviewer{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	e.wantUntouched(task, pr)
}

func TestReviewerRequeuesOnFailedCI(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, sha := e.reviewTask()
	e.gh.SetRuns(sha,
		githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure", URL: "http://ci/test"},
		githubtest.Run{Name: "lint", Status: "completed", Conclusion: "success", URL: "http://ci/lint"},
	)

	if err := (&reviewer{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	if n := e.claude.callCount(); n != 0 {
		t.Errorf("claude called %d times on failed CI", n)
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
	r := &reviewer{e.deps}

	if err := r.Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	e.wantUntouched(task, pr)

	e.gh.SetCommitTime(sha, time.Now().Add(-10*time.Minute))
	e.claude.answer(`{"verdict": "approve", "body": "fine"}`)
	if err := r.Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if !e.gh.Pull(pr).Merged {
		t.Error("PR not merged once the wait for CI was over")
	}
}

func TestReviewerApproveMerges(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, sha := e.reviewTask()
	e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "completed", Conclusion: "success"})
	e.claude.answer(`{"verdict": "approve", "body": "fine"}`)

	if err := (&reviewer{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	p := e.gh.Pull(pr)
	if !p.Merged || p.MergeMethod != "merge" || p.MergedSHA != sha {
		t.Errorf("PR merged=%v method=%q sha=%q, want merged with merge at %s", p.Merged, p.MergeMethod, p.MergedSHA, sha)
	}
	if got := e.gh.Issue(task); got.Open || got.StateReason != "completed" {
		t.Errorf("task open=%v reason=%q, want closed as completed", got.Open, got.StateReason)
	}
	e.wantStatus(task, label.None)
	wantContains(t, "claude input", e.claude.lastInput(), "task body", "why we decided this", "origin/main")
}

func TestReviewerChangesRequeues(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, _ := e.reviewTask()
	e.claude.answer(`{"verdict": "changes", "body": "rename the function"}`)

	if err := (&reviewer{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Queued)
	p := e.gh.Pull(pr)
	if p.Merged {
		t.Error("PR merged despite requested changes")
	}
	if len(p.Reviews) != 1 || p.Reviews[0].Body != github.Marker+"\nrename the function" {
		t.Errorf("reviews = %+v", p.Reviews)
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
	r := &reviewer{e.deps}

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
