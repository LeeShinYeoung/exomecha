package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const resolverDone = `{"result": "resolved", "reason": "kept both lines"}`

// conflictPRD sets up a PRD in review whose branch and main both changed since the branch
// started, and whose PR GitHub reports as conflicting. It returns the branch head.
func (e *env) conflictPRD(feature, main map[string]string, labels ...string) (prd, pr int, head string) {
	prd = e.addPRD("PRD", "PRD body", append([]string{label.PRD, string(label.Review)}, labels...)...)
	head = gittest.Commit(e.t, e.origin, branchName(prd), "main", feature)
	gittest.Commit(e.t, e.origin, "main", "", main)
	pr = e.gh.AddPull(branchName(prd), "main")
	e.gh.SetMergeable(pr, new(false))
	return prd, pr, head
}

func (e *env) resolver() *resolver { return &resolver{e.deps, e.runs} }

func (e *env) resolve(n int) {
	e.t.Helper()
	if err := e.resolver().Handle(e.t.Context(), n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) resolverBox(n int) string { return e.runs.name(resolverRole, n) }

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

// conflicted is a PRD whose PR conflicts on app.txt, with resolver's sandbox started for it.
func (e *env) conflicted() (prd int, name, head string) {
	e.t.Helper()
	prd, _, head = e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.resolve(prd)
	name = e.resolverBox(prd)
	e.wantState(name, running)
	return prd, name, head
}

func TestResolverRunsThePRDsTier(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		labels []string
		want   label.Tier
	}{
		{nil, label.Normal},
		{[]string{string(label.Heavy)}, label.Heavy},
	} {
		e := newEnv(t, map[string]string{"app.txt": "v1\n"})
		prd, _, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"}, c.labels...)
		e.resolve(prd)
		if got := e.startedModel(e.resolverBox(prd)); got != testModels[c.want].Resolver {
			t.Errorf("PRD with %q got %v, want %v", c.labels, got, testModels[c.want].Resolver)
		}
	}
}

func TestResolverPicks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	conflicting, _, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	mergeable, _, _ := e.reviewPRD()
	e.sbx.add(e.resolverBox(mergeable))
	e.sbx.add(e.runs.name(builderRole, 99))

	picked, err := e.resolver().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{conflicting, mergeable})
}

// The approval was for code that no longer merges, so the PRD goes back to review, where the
// next round resolves it.
func TestResolverSendsAnApprovedPRDThatConflictsBackToReview(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, _, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.gh.Relabel(prd, string(label.Review), string(label.Approved))

	e.resolve(prd)

	e.wantStatus(prd, label.Review)
	if e.sbx.exists(e.resolverBox(prd)) {
		t.Error("a run started in the same round")
	}
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("%d comments, want none", n)
	}
	e.resolve(prd)
	e.wantState(e.resolverBox(prd), running)
}

func TestResolverLeavesAnApprovedPRDThatMerges(t *testing.T) {
	t.Parallel()
	for _, mergeable := range []*bool{new(true), nil} {
		e := newEnv(t, map[string]string{"app.txt": "v1\n"})
		prd, pr := e.approvedPRD()
		e.gh.SetMergeable(pr, mergeable)

		e.resolve(prd)

		e.wantStatus(prd, label.Approved)
		if e.sbx.exists(e.resolverBox(prd)) {
			t.Error("a run started")
		}
	}
}

func TestResolverPushesResolvedMerge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name, _ := e.conflicted()
	repo := e.sbx.repo(name)
	if _, err := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); err != nil {
		t.Errorf("no merge in progress in the sandbox: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "app.txt")); !strings.Contains(string(b), "<<<<<<<") || !strings.Contains(string(b), "||||||| ") {
		t.Errorf("app.txt has no conflict markers with the original lines: %q", b)
	}
	input := e.readCtl(name, "input.md")
	wantContains(t, "claude input", input, "PRD body", "## Conflicted files (merging `origin/main`)", "- app.txt")
	wantNotContains(t, "claude input", input, "## Setup")

	e.play(name, resolveWith(t, "main\nfeature\n"), resolverDone)
	e.resolve(prd)

	branch := branchName(prd)
	if n := len(gittest.Parents(t, e.origin, branch)); n != 2 {
		t.Errorf("%s head has %d parents, want a merge commit", branch, n)
	}
	if got := gittest.ReadFile(t, e.origin, branch, "app.txt"); got != "main\nfeature" {
		t.Errorf("app.txt = %q", got)
	}
	e.wantAuthor(branch)
	e.wantStatus(prd, label.Review)
	e.wantRemoved(name)
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("resolved merge left %d comments", n)
	}
}

func TestResolverReadsThePRAndLeavesHowItResolved(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.gh.AddLineComment(pr, "app.txt", 1, "keep main's line first")
	e.resolve(prd)
	name := e.resolverBox(prd)
	wantContains(t, "claude input", e.readCtl(name, "input.md"),
		fmt.Sprintf("(person, PR #%d, on app.txt line 1)\n\nkeep main's line first", pr))

	e.play(name, resolveWith(t, "main\nfeature\n"), `{"result": "resolved", "reason": "kept both lines", "details": "app.txt: main's line first"}`)
	e.resolve(prd)

	comments := e.gh.Pull(pr).Comments
	want := github.NoteBody(github.Note{Worker: "resolver", Kind: github.KindResolved},
		"kept both lines\n\n<details>\n<summary>Details</summary>\n\napp.txt: main's line first\n\n</details>")
	if len(comments) != 1 || comments[0].Body != want {
		t.Errorf("PR comments = %+v, want resolver's reason", comments)
	}
}

func TestResolverStuckWhenMarkersRemain(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name, head := e.conflicted()
	e.play(name, func(repo string) { commitMerge(t, repo) }, `{"result": "resolved", "reason": "done"}`)
	e.resolve(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "conflict markers remain in: app.txt", label.Review)
	if got := gittest.Head(e.origin, branchName(prd)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
	e.wantRemoved(name)
}

func TestResolverStuck(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name, head := e.conflicted()
	e.play(name, nil, `{"result": "stuck", "reason": "choose between A and B"}`)
	e.resolve(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "choose between A and B", label.Review)
	if got := gittest.Head(e.origin, branchName(prd)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
	e.wantRemoved(name)
}

func TestResolverStuckWhenMergeNotCommitted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name, head := e.conflicted()
	e.play(name, func(repo string) { writeFile(t, repo, "app.txt", "main\nfeature\n") }, resolverDone)
	e.resolve(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "the merge was not committed", label.Review)
	if got := gittest.Head(e.origin, branchName(prd)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
}

func TestResolverStuckWhenTheResultLacksTheBase(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name, _ := e.conflicted()
	e.play(name, func(repo string) {
		gittest.Run(t, repo, "merge", "--abort")
		writeFile(t, repo, "app.txt", "feature only\n")
		gittest.CommitAll(t, repo, "keep mine")
	}, resolverDone)
	e.resolve(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "does not include the base branch", label.Review)
}

func TestResolverCommitsCleanMergeThroughClaude(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, _, _ := e.conflictPRD(map[string]string{"other.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.resolve(prd)
	name := e.resolverBox(prd)
	wantContains(t, "claude input", e.readCtl(name, "input.md"), "None. The merge only needs to be committed.")
	e.play(name, func(repo string) { commitMerge(t, repo) }, `{"result": "resolved", "reason": "nothing to resolve"}`)
	e.resolve(prd)

	if n := len(gittest.Parents(t, e.origin, branchName(prd))); n != 2 {
		t.Errorf("branch head has %d parents, want a merge commit", n)
	}
	e.wantStatus(prd, label.Review)
}

// A project setting can make git refuse the merge with no conflict, the same way on every try.
func TestResolverStuckWhenTheMergeFailsWithoutAConflict(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.writeSetup(setupScript, "git config merge.verifySignatures true\n")
	prd, _, head := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	e.resolve(prd)

	e.wantStatus(prd, label.Stuck)
	comment := e.lastComment(prd)
	wantStuckComment(t, comment, "Merging `main` into this PRD's branch in the sandbox failed without a conflict", label.Review)
	wantNotContains(t, "stuck comment", comment, "GPG signature")
	wantContains(t, "kept merge output", e.wantKept(e.resolverBox(prd), "merge.log"), "does not have a GPG signature")
	e.wantRemoved(e.resolverBox(prd))
	if got := gittest.Head(e.origin, branchName(prd)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
}

func TestResolverRetriesWhenSbxFailsTheMerge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, _, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	name := e.resolverBox(prd)
	e.sbx.failExec = "merge --no-commit"

	if err := e.resolver().Handle(t.Context(), prd); err == nil {
		t.Fatal("Handle returned nil for an sbx error")
	}

	e.wantStatus(prd, label.Review)
	e.wantRemoved(name)
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("an sbx error left %d comments", n)
	}
	e.sbx.failExec = ""
	e.resolve(prd)
	e.wantState(name, running)
}

func TestResolverNothingToMerge(t *testing.T) {
	t.Parallel()
	// The script fails if setup runs, which it must not with nothing to merge.
	e := newSetupEnv(t, "exit 1\n")
	prd := e.addPRD("PRD", "PRD body", label.PRD, string(label.Review))
	head := gittest.Commit(t, e.origin, branchName(prd), "main", map[string]string{"app.txt": "feature\n"})
	pr := e.gh.AddPull(branchName(prd), "main")
	e.gh.SetMergeable(pr, new(false))

	e.resolve(prd)

	if e.sbx.exists(e.resolverBox(prd)) {
		t.Error("a sandbox started with nothing to merge")
	}
	if got := gittest.Head(e.origin, branchName(prd)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
	e.wantStatus(prd, label.Review)
}

func TestResolverWaitsForAFreeSlot(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ProjectLimit = 1
	e.sbx.add(e.runs.name(builderRole, 99))
	prd, _, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	e.resolve(prd)

	if e.sbx.exists(e.resolverBox(prd)) {
		t.Error("started a sandbox over the limit")
	}
	e.wantStatus(prd, label.Review)
}

func TestResolverStartsOverWhenTheBranchChangedDuringTheRun(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name, _ := e.conflicted()
	pushed := gittest.Commit(t, e.origin, branchName(prd), "", map[string]string{"other.txt": "a person's push\n"})
	e.play(name, resolveWith(t, "main\nfeature\n"), resolverDone)

	e.resolve(prd)

	e.wantRemoved(name)
	e.wantStatus(prd, label.Review)
	if got := gittest.Head(e.origin, branchName(prd)); got != pushed {
		t.Errorf("branch = %s, want the person's push %s left alone", got, pushed)
	}
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("a branch change left %d comments", n)
	}
}

func TestResolverStuckWhenNoCommitIsCheckedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name, _ := e.conflicted()
	e.play(name, func(repo string) {
		gittest.Run(t, repo, "merge", "--abort")
		gittest.Run(t, repo, "checkout", "-q", "--orphan", "empty")
	}, resolverDone)

	e.resolve(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "no commit checked out", label.Review)
}
