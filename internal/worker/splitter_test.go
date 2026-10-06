package worker

import (
	"slices"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/label"
)

func (e *env) splitter() *splitter { return &splitter{e.deps, e.runs} }

// split runs one splitter round for the PRD and fails the test on an error.
func (e *env) split(n int) {
	e.t.Helper()
	if err := e.splitter().Handle(e.t.Context(), n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) splitterBox(n int) string { return e.runs.name(splitterRole, n) }

// splitting is a PRD with splitter's run started for it.
func (e *env) splitting() (prd int, name string) {
	e.t.Helper()
	prd = e.gh.AddIssue("PRD", "the plan", label.PRD)
	e.split(prd)
	name = e.splitterBox(prd)
	e.wantState(name, running)
	return prd, name
}

func TestSplitterCreatesTasks(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", "the plan", label.PRD)
	e.gh.AddComment(prd, "a person's note")

	picked, err := e.splitter().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{prd})
	e.split(prd)
	name := e.splitterBox(prd)
	e.wantState(name, running)
	wantContains(t, "claude input", e.readCtl(name, "input.md"), "the plan", "a person's note")
	e.play(name, nil, `{"tasks": [
		{"title": "A", "body": "a", "blocked_by": [], "tier": "light"},
		{"title": "B", "body": "b", "blocked_by": [1, 2, 0, 9], "tier": "heavy"},
		{"title": "C", "body": "c", "blocked_by": [1, 2, 3]}
	]}`)
	e.split(prd)

	tasks := e.gh.Issue(prd).SubIssues
	if len(tasks) != 3 {
		t.Fatalf("PRD sub-issues = %v, want 3 tasks", tasks)
	}
	a, b, c := tasks[0], tasks[1], tasks[2]
	for _, n := range tasks {
		if !e.gh.Issue(n).HasLabel(label.Task) {
			t.Errorf("task #%d has no %s label", n, label.Task)
		}
		e.wantStatus(n, label.Pending)
	}
	// C's answer has no tier, as from a run that started before tiers existed.
	for n, want := range map[int][]string{a: {string(label.Light)}, b: {string(label.Heavy)}, c: nil} {
		tiers := slices.DeleteFunc(e.gh.Issue(n).Labels, func(l string) bool { return !strings.HasPrefix(l, "tier:") })
		if !slices.Equal(tiers, want) {
			t.Errorf("task #%d tier labels = %q, want %q", n, tiers, want)
		}
	}
	wantInts(t, "A blocked by", e.gh.Issue(a).BlockedBy, nil)
	wantInts(t, "B blocked by", e.gh.Issue(b).BlockedBy, []int{a})
	wantInts(t, "C blocked by", e.gh.Issue(c).BlockedBy, []int{a, b})
	if !e.gh.Issue(prd).HasLabel(label.Tasked) {
		t.Errorf("PRD has no %s label", label.Tasked)
	}
	e.wantStatus(prd, label.None)
	e.wantRemoved(name)

	picked, err = e.splitter().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick after split", picked, nil)
}

func TestSplitterPicks(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	ready := e.gh.AddIssue("PRD", "", label.PRD)
	e.gh.AddIssue("tasked PRD", "", label.PRD, label.Tasked)
	e.gh.AddIssue("stuck PRD", "", label.PRD, string(label.Stuck))
	left := e.gh.AddIssue("tasked PRD with a leftover sandbox", "", label.PRD, label.Tasked)
	e.sbx.add(e.splitterBox(left))
	e.sbx.add(e.runs.name(reviewerRole, 99))

	picked, err := e.splitter().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{ready, left})
}

func TestSplitterIgnoresOutsiderPRD(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.gh.AddOutsiderIssue("PRD from an outsider", "", label.PRD)
	mine := e.gh.AddIssue("PRD", "", label.PRD)

	picked, err := e.splitter().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{mine})
}

func TestSplitterRecoversUnfinishedSplit(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.splitting()
	old := e.gh.AddIssue("left over", "", label.Task, string(label.Pending))
	e.gh.AddSubIssue(prd, old)
	e.play(name, nil, `{"tasks": [{"title": "fresh", "body": "f", "blocked_by": [], "tier": "normal"}]}`)

	e.split(prd)

	left := e.gh.Issue(old)
	if left.Open || left.StateReason != "not_planned" {
		t.Errorf("left-over task: open=%v reason=%q, want closed as not_planned", left.Open, left.StateReason)
	}
	e.wantStatus(old, label.None)
	subs := e.gh.Issue(prd).SubIssues
	if len(subs) != 2 {
		t.Fatalf("PRD sub-issues = %v, want the left-over task and one new task", subs)
	}
	if fresh := e.gh.Issue(subs[1]); fresh.Title != "fresh" || !fresh.Open {
		t.Errorf("new task = %+v", fresh)
	}
	e.wantStatus(subs[1], label.Pending)
	if !e.gh.Issue(prd).HasLabel(label.Tasked) {
		t.Errorf("PRD has no %s label", label.Tasked)
	}
}

// Once a person removes the stuck label, splitter splits the PRD again.
func TestSplitterFirstFailureMakesThePRDStuck(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.splitting()
	e.writeCtl(name, "stream.jsonl", `{"type":"system","subtype":"init","permissionMode":"auto"}`+"\n"+
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}`+"\n")
	e.writeCtl(name, "exit", "1\n")

	e.split(prd)

	e.wantStatus(prd, label.Stuck)
	comment := e.lastComment(prd)
	wantStuckComment(t, comment, "The AI's run failed because claude ended with an error", label.None)
	wantContains(t, "stuck comment", comment, "This PRD is blocked")
	e.wantRemoved(name)
	e.wantKept(name, "stream.jsonl")
	if subs := e.gh.Issue(prd).SubIssues; len(subs) != 0 {
		t.Errorf("a failed split made Tasks %v", subs)
	}
	picked, err := e.splitter().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick while stuck", picked, nil)

	e.gh.Relabel(prd, string(label.Stuck), "")
	e.split(prd)
	e.wantState(name, running)
}

func TestSplitterZeroTasksIsAFailure(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.splitting()
	e.play(name, nil, `{"tasks": []}`)

	e.split(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "The AI's run failed because the AI split the PRD into no Tasks", label.None)
	e.wantKept(name, "stream.jsonl")
	if e.gh.Issue(prd).HasLabel(label.Tasked) {
		t.Errorf("PRD with no Tasks has %s", label.Tasked)
	}
}
