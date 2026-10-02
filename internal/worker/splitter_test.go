package worker

import (
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/label"
)

func TestSplitterCreatesTasks(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", "the plan", label.PRD)
	e.gh.AddComment(prd, "a person's note")
	e.claude.answer(`{"tasks": [
		{"title": "A", "body": "a", "blocked_by": []},
		{"title": "B", "body": "b", "blocked_by": [1, 2, 0, 9]},
		{"title": "C", "body": "c", "blocked_by": [1, 2, 3]}
	]}`)
	s := &splitter{e.deps}

	picked, err := s.Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{prd})
	if err := s.Handle(t.Context(), prd); err != nil {
		t.Fatal(err)
	}

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
	wantInts(t, "A blocked by", e.gh.Issue(a).BlockedBy, nil)
	wantInts(t, "B blocked by", e.gh.Issue(b).BlockedBy, []int{a})
	wantInts(t, "C blocked by", e.gh.Issue(c).BlockedBy, []int{a, b})
	if !e.gh.Issue(prd).HasLabel(label.Tasked) {
		t.Errorf("PRD has no %s label", label.Tasked)
	}
	e.wantStatus(prd, label.None)

	input := e.claude.lastInput()
	wantContains(t, "claude input", input, "the plan", "a person's note")

	picked, err = s.Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick after split", picked, nil)
}

func TestSplitterIgnoresOutsiderPRD(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.gh.AddOutsiderIssue("PRD from an outsider", "", label.PRD)
	mine := e.gh.AddIssue("PRD", "", label.PRD)

	picked, err := (&splitter{e.deps}).Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{mine})
}

func TestSplitterRecoversUnfinishedSplit(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", "the plan", label.PRD)
	old := e.gh.AddIssue("left over", "", label.Task, string(label.Pending))
	e.gh.AddSubIssue(prd, old)
	e.claude.answer(`{"tasks": [{"title": "fresh", "body": "f", "blocked_by": []}]}`)

	if err := (&splitter{e.deps}).Handle(t.Context(), prd); err != nil {
		t.Fatal(err)
	}

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
