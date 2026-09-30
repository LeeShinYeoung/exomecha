package worker

import (
	"testing"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
	"github.com/LeeShinYeoung/coworker/internal/label"
)

// conflictTask sets up a Task in review whose branch and main both changed since the branch
// started, and whose PR GitHub reports as conflicting. It returns the branch head.
func (e *env) conflictTask(feature, main map[string]string) (task, pr int, head string) {
	task = e.gh.AddIssue("Task", "task body", label.Task, string(label.Review))
	head = gittest.Commit(e.t, e.origin, branchName(task), "main", feature)
	gittest.Commit(e.t, e.origin, "main", "", main)
	pr = e.gh.AddPull(branchName(task), "main")
	e.gh.SetMergeable(pr, new(false))
	return task, pr, head
}

// commitMerge commits the merge in progress the way the AI does, with git's merge message.
func commitMerge(t *testing.T, dir string) {
	t.Helper()
	gittest.Run(t, dir, "add", "-A")
	gittest.Run(t, dir, "commit", "-q", "--no-edit")
}

func TestResolverPicksConflictingPRsOnly(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	conflicting, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.reviewTask() // a Task in review whose PR is mergeable

	picked, err := (&resolver{e.deps}).Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{conflicting})
}

func TestResolverPushesResolvedMerge(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.claude.reply = func(req agent.Request) (string, error) {
		writeFile(t, req.Dir, "app.txt", "main\nfeature\n")
		commitMerge(t, req.Dir)
		return `{"result": "resolved", "reason": "kept both lines"}`, nil
	}

	if err := (&resolver{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	branch := branchName(task)
	if n := len(gittest.Parents(t, e.origin, branch)); n != 2 {
		t.Errorf("%s head has %d parents, want a merge commit", branch, n)
	}
	if got := gittest.ReadFile(t, e.origin, branch, "app.txt"); got != "main\nfeature" {
		t.Errorf("app.txt = %q", got)
	}
	e.wantStatus(task, label.Review)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("resolved merge left %d comments", n)
	}
	wantContains(t, "claude input", e.claude.lastInput(), "task body", "- app.txt")
}

func TestResolverStuckWhenMarkersRemain(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, _, head := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.claude.reply = func(req agent.Request) (string, error) {
		commitMerge(t, req.Dir)
		return `{"result": "resolved", "reason": "done"}`, nil
	}

	if err := (&resolver{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "conflict markers remain in: app.txt", label.Review)
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
}

func TestResolverStuck(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, _, head := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.claude.answer(`{"result": "stuck", "reason": "choose between A and B"}`)

	if err := (&resolver{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "choose between A and B", label.Review)
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
}

func TestResolverStuckWhenMergeNotCommitted(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, _, head := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.claude.reply = func(req agent.Request) (string, error) {
		writeFile(t, req.Dir, "app.txt", "main\nfeature\n")
		return `{"result": "resolved", "reason": "kept both lines"}`, nil
	}

	if err := (&resolver{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "the merge was not committed", label.Review)
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
}

func TestResolverCommitsCleanMergeThroughClaude(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, _, _ := e.conflictTask(map[string]string{"other.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.claude.reply = func(req agent.Request) (string, error) {
		commitMerge(t, req.Dir)
		return `{"result": "resolved", "reason": "nothing to resolve"}`, nil
	}

	if err := (&resolver{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	wantContains(t, "claude input", e.claude.lastInput(), "None. The merge only needs to be committed.")
	if n := len(gittest.Parents(t, e.origin, branchName(task))); n != 2 {
		t.Errorf("branch head has %d parents, want a merge commit", n)
	}
	e.wantStatus(task, label.Review)
}

func TestResolverNothingToMerge(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "task body", label.Task, string(label.Review))
	head := gittest.Commit(t, e.origin, branchName(task), "main", map[string]string{"app.txt": "feature\n"})
	pr := e.gh.AddPull(branchName(task), "main")
	e.gh.SetMergeable(pr, new(false))

	if err := (&resolver{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	if n := e.claude.callCount(); n != 0 {
		t.Errorf("claude called %d times with nothing to merge", n)
	}
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
	e.wantStatus(task, label.Review)
}
