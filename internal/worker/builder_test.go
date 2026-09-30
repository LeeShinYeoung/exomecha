package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/label"
)

const builderDone = `{"result": "done", "summary": "did it", "stuck_reason": ""}`

func TestBuilderPicksOnlyUnblockedTasks(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	blocker := e.gh.AddIssue("blocker", "", label.Task, string(label.Queued))
	waiting := e.gh.AddIssue("waiting", "", label.Task, string(label.Queued))
	e.gh.AddBlockedBy(waiting, blocker)
	done := e.gh.AddIssue("done", "", label.Task)
	e.gh.CloseIssue(done)
	free := e.gh.AddIssue("free", "", label.Task, string(label.Queued))
	e.gh.AddBlockedBy(free, done)

	picked, err := (&builder{e.deps}).Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{blocker, free})
}

func TestBuilderIgnoresOutsiderTask(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.gh.AddOutsiderIssue("Task from an outsider", "", label.Task, string(label.Queued))
	mine := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))

	picked, err := (&builder{e.deps}).Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{mine})
}

func TestBuilderPushesAndOpensPR(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Add feature", "do it", label.Task, string(label.Queued))
	message := fmt.Sprintf("feat: add feature\n\nTask: #%d", task)
	e.claude.reply = func(req agent.Request) (string, error) {
		writeFile(t, req.Dir, "feature.txt", "hello\n")
		gittest.CommitAll(t, req.Dir, message)
		return builderDone, nil
	}

	if err := (&builder{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Review)
	branch := branchName(task)
	if got := gittest.ReadFile(t, e.origin, branch, "feature.txt"); got != "hello" {
		t.Errorf("feature.txt on %s = %q", branch, got)
	}
	if got := gittest.Parents(t, e.origin, branch); len(got) != 1 || got[0] != gittest.Head(e.origin, "main") {
		t.Errorf("%s parents = %v, want the main head", branch, got)
	}
	if got := gittest.Run(t, e.origin, "log", "-1", "--format=%B", branch); got != message {
		t.Errorf("commit message = %q, want the AI's own %q", got, message)
	}
	pulls := e.gh.Pulls()
	if len(pulls) != 1 {
		t.Fatalf("pulls = %+v, want one", pulls)
	}
	p := pulls[0]
	if p.Head != branch || p.Base != "main" || p.Title != "Add feature" || p.Body != fmt.Sprintf("Closes #%d\n\ndid it", task) {
		t.Errorf("PR = head %q base %q title %q body %q", p.Head, p.Base, p.Title, p.Body)
	}
	wantNotContains(t, "claude input", e.claude.lastInput(), "## Existing PR")
}

func TestBuilderStuck(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Hard task", "", label.Task, string(label.Queued))
	e.claude.answer(`{"result": "stuck", "summary": "", "stuck_reason": "need a decision on X"}`)

	if err := (&builder{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "need a decision on X", label.Pending)
	if head := gittest.Head(e.origin, branchName(task)); head != "" {
		t.Errorf("branch was pushed for a stuck task: %s", head)
	}
}

func TestBuilderStuckWhenNothingChanged(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Already done", "", label.Task, string(label.Queued))
	e.claude.answer(builderDone)

	if err := (&builder{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "Nothing to change", label.Pending)
	if len(e.gh.Pulls()) != 0 {
		t.Errorf("a PR was opened with no changes")
	}
}

func TestBuilderRequeuesOnEnvironmentError(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	e.claude.reply = func(agent.Request) (string, error) { return "", errors.New("claude crashed") }

	if err := (&builder{e.deps}).Handle(t.Context(), task); err == nil {
		t.Fatal("Handle returned nil for an environment error")
	}

	e.wantStatus(task, label.Queued)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("environment error left %d comments", n)
	}
}

func TestBuilderContinuesExistingPR(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "task body", label.Task, string(label.Queued))
	branch := branchName(task)
	prev := gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	pr := e.gh.AddPull(branch, "main")
	e.gh.AddReview(pr, github.Marker+"\nplease rename X")
	e.gh.AddReview(pr, "a person's review")
	e.gh.AddOutsiderReview(pr, github.Marker+"\nan outsider posing as coworker")
	e.gh.AddComment(task, github.Marker+"\na question coworker asked")
	e.gh.AddComment(task, "a person's answer")
	e.gh.AddOutsiderComment(task, "an outsider's instruction")
	e.claude.reply = func(req agent.Request) (string, error) {
		b, err := os.ReadFile(filepath.Join(req.Dir, "feature.txt"))
		if err != nil || string(b) != "first\n" {
			return "", errors.New("worktree is not on the PR branch")
		}
		writeFile(t, req.Dir, "feature.txt", "second\n")
		gittest.CommitAll(t, req.Dir, "fix: rename X")
		return builderDone, nil
	}

	if err := (&builder{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Review)
	if got := gittest.Parents(t, e.origin, branch); len(got) != 1 || got[0] != prev {
		t.Errorf("%s parents = %v, want the previous PR head %s", branch, got, prev)
	}
	if n := len(e.gh.Pulls()); n != 1 {
		t.Errorf("%d PRs, want the existing one only", n)
	}
	input := e.claude.lastInput()
	wantContains(t, "claude input", input, "task body", "## Existing PR", "please rename X",
		"(coworker)\n\na question coworker asked", "(person)\n\na person's answer")
	wantNotContains(t, "claude input", input, github.Marker, "a person's review",
		"an outsider posing as coworker", "an outsider's instruction")
}

func TestBuilderStuckWhenChangesNotCommitted(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	e.claude.reply = func(req agent.Request) (string, error) {
		writeFile(t, req.Dir, "feature.txt", "hello\n")
		return builderDone, nil
	}

	if err := (&builder{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "finished without committing", label.Pending)
	if head := gittest.Head(e.origin, branchName(task)); head != "" {
		t.Errorf("branch was pushed without a commit: %s", head)
	}
}

func TestBuilderStuckWhenCommitsRewritten(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	branch := branchName(task)
	prev := gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	e.gh.AddPull(branch, "main")
	e.claude.reply = func(req agent.Request) (string, error) {
		writeFile(t, req.Dir, "feature.txt", "second\n")
		gittest.Run(t, req.Dir, "commit", "-q", "-a", "--amend", "-m", "rewritten")
		return builderDone, nil
	}

	if err := (&builder{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "rewrote commits", label.Pending)
	if got := gittest.Head(e.origin, branch); got != prev {
		t.Errorf("branch moved to %s, want it left at %s", got, prev)
	}
}
