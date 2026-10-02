package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const resolverDone = `{"result": "resolved", "reason": "kept both lines"}`

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

func (e *env) resolver() *resolver { return &resolver{e.deps, e.runs} }

// resolve runs one resolver round for the Task and fails the test on an error.
func (e *env) resolve(n int) {
	e.t.Helper()
	if err := e.resolver().Handle(e.t.Context(), n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) resolverBox(n int) string { return e.runs.name(n, resolverRole) }

// commitMerge commits the merge in progress the way the AI does, with git's merge message.
func commitMerge(t *testing.T, repo string) {
	t.Helper()
	gittest.Run(t, repo, "add", "-A")
	gittest.Run(t, repo, "commit", "-q", "--no-edit")
}

func resolveWith(t *testing.T, content string) func(string) {
	return func(repo string) {
		writeFile(t, repo, "app.txt", content)
		commitMerge(t, repo)
	}
}

// conflicted is a Task whose PR conflicts on app.txt, with resolver's sandbox started for it.
func (e *env) conflicted() (task int, name, head string) {
	e.t.Helper()
	task, _, head = e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.resolve(task)
	name = e.resolverBox(task)
	e.wantState(name, running)
	return task, name, head
}

func TestResolverPicks(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	conflicting, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	mergeable, _, _ := e.reviewTask()
	e.sbx.add(e.resolverBox(mergeable))
	e.sbx.add(e.runs.name(99, builderRole))

	picked, err := e.resolver().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{conflicting, mergeable})
}

func TestResolverPushesResolvedMerge(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name, _ := e.conflicted()
	repo := e.sbx.repo(name)
	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); err != nil {
		t.Errorf("no merge in progress in the sandbox: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "app.txt")); !strings.Contains(string(b), "<<<<<<<") {
		t.Errorf("app.txt has no conflict markers: %q", b)
	}
	input := e.readCtl(name, "input.md")
	wantContains(t, "claude input", input, "task body", "## Conflicted files (merging `origin/main`)", "- app.txt")
	wantNotContains(t, "claude input", input, "## Setup")

	e.play(name, resolveWith(t, "main\nfeature\n"), resolverDone)
	e.resolve(task)

	branch := branchName(task)
	if n := len(gittest.Parents(t, e.origin, branch)); n != 2 {
		t.Errorf("%s head has %d parents, want a merge commit", branch, n)
	}
	if got := gittest.ReadFile(t, e.origin, branch, "app.txt"); got != "main\nfeature" {
		t.Errorf("app.txt = %q", got)
	}
	e.wantAuthor(branch)
	e.wantStatus(task, label.Review)
	e.wantRemoved(name)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("resolved merge left %d comments", n)
	}
}

func TestResolverStuckWhenMarkersRemain(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name, head := e.conflicted()
	e.play(name, func(repo string) { commitMerge(t, repo) }, `{"result": "resolved", "reason": "done"}`)
	e.resolve(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "conflict markers remain in: app.txt", label.Review)
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
	e.wantRemoved(name)
}

func TestResolverStuck(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name, head := e.conflicted()
	e.play(name, nil, `{"result": "stuck", "reason": "choose between A and B"}`)
	e.resolve(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "choose between A and B", label.Review)
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
	e.wantRemoved(name)
}

func TestResolverStuckWhenMergeNotCommitted(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name, head := e.conflicted()
	e.play(name, func(repo string) { writeFile(t, repo, "app.txt", "main\nfeature\n") }, resolverDone)
	e.resolve(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "the merge was not committed", label.Review)
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
}

func TestResolverStuckWhenTheResultLacksTheBase(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name, _ := e.conflicted()
	e.play(name, func(repo string) {
		gittest.Run(t, repo, "merge", "--abort")
		writeFile(t, repo, "app.txt", "feature only\n")
		gittest.CommitAll(t, repo, "keep mine")
	}, resolverDone)
	e.resolve(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "does not include the base branch", label.Review)
}

func TestResolverCommitsCleanMergeThroughClaude(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, _, _ := e.conflictTask(map[string]string{"other.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.resolve(task)
	name := e.resolverBox(task)
	wantContains(t, "claude input", e.readCtl(name, "input.md"), "None. The merge only needs to be committed.")
	e.play(name, func(repo string) { commitMerge(t, repo) }, `{"result": "resolved", "reason": "nothing to resolve"}`)
	e.resolve(task)

	if n := len(gittest.Parents(t, e.origin, branchName(task))); n != 2 {
		t.Errorf("branch head has %d parents, want a merge commit", n)
	}
	e.wantStatus(task, label.Review)
}

// A project setting can make git refuse the merge with no conflict, the same way on every try.
func TestResolverStuckWhenTheMergeFailsWithoutAConflict(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, map[string]string{"app.txt": "v1\n", setupScript: "git config merge.verifySignatures true\n"})
	task, _, head := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	e.resolve(task)

	e.wantStatus(task, label.Stuck)
	comment := e.lastComment(task)
	wantStuckComment(t, comment, "Merging `main` into this Task's branch in the sandbox failed without a conflict", label.Review)
	wantNotContains(t, "stuck comment", comment, "GPG signature")
	wantContains(t, "log", log.String(), "does not have a GPG signature")
	e.wantRemoved(e.resolverBox(task))
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
}

func TestResolverRetriesWhenSbxFailsTheMerge(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	name := e.resolverBox(task)
	e.sbx.failExec = "git merge"

	if err := e.resolver().Handle(t.Context(), task); err == nil {
		t.Fatal("Handle returned nil for an sbx error")
	}

	e.wantStatus(task, label.Review)
	e.wantRemoved(name)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("an sbx error left %d comments", n)
	}
	e.sbx.failExec = ""
	e.clock.advance(minBackoff)
	e.resolve(task)
	e.wantState(name, running)
}

func TestResolverNothingToMerge(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "task body", label.Task, string(label.Review))
	head := gittest.Commit(t, e.origin, branchName(task), "main", map[string]string{"app.txt": "feature\n"})
	pr := e.gh.AddPull(branchName(task), "main")
	e.gh.SetMergeable(pr, new(false))

	e.resolve(task)

	if e.sbx.exists(e.resolverBox(task)) {
		t.Error("a sandbox started with nothing to merge")
	}
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
	e.wantStatus(task, label.Review)
}

func TestResolverRemovesTheSandboxWhenTheTaskLeavesReview(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name, _ := e.conflicted()
	e.relabel(task, label.Review, label.Queued)

	e.resolve(task)

	e.wantRemoved(name)
}

func TestResolverThreeFailuresGoStuck(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name, _ := e.conflicted()
	for i := 1; i <= 3; i++ {
		e.writeCtl(name, "stream.jsonl", "{}\n")
		e.resolve(task)
		e.clock.advance(stallAfter)
		e.resolve(task)
		e.wantRemoved(name)
		if i == 3 {
			break
		}
		e.wantStatus(task, label.Review)
		e.clock.advance(maxBackoff)
		e.resolve(task)
		e.wantState(name, running)
	}

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "failed 3 times in a row", label.Review)
	wantContains(t, "stuck comment", e.lastComment(task), "did not change for 15 minutes")
}

func TestResolverWaitsForAFreeSlot(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.SandboxLimit = 1
	e.sbx.add(e.runs.name(99, builderRole))
	task, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	e.resolve(task)

	if e.sbx.exists(e.resolverBox(task)) {
		t.Error("started a sandbox over the limit")
	}
	e.wantStatus(task, label.Review)
}

func TestResolverStartsOverWhenTheBranchChangedDuringTheRun(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name, _ := e.conflicted()
	pushed := gittest.Commit(t, e.origin, branchName(task), "", map[string]string{"other.txt": "a person's push\n"})
	e.play(name, resolveWith(t, "main\nfeature\n"), resolverDone)

	e.resolve(task)

	e.wantRemoved(name)
	e.wantStatus(task, label.Review)
	if got := gittest.Head(e.origin, branchName(task)); got != pushed {
		t.Errorf("branch = %s, want the person's push %s left alone", got, pushed)
	}
	e.clock.advance(time.Minute)
	if got := e.runs.fail(name); got != 1 {
		t.Errorf("a branch change counted as a failed run")
	}
}

func TestResolverStuckWhenNoCommitIsCheckedOut(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name, _ := e.conflicted()
	e.play(name, func(repo string) {
		gittest.Run(t, repo, "merge", "--abort")
		gittest.Run(t, repo, "checkout", "-q", "--orphan", "empty")
	}, resolverDone)

	e.resolve(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "no commit checked out", label.Review)
}
