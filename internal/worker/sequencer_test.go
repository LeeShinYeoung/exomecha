package worker

import (
	"fmt"
	"testing"

	"github.com/LeeShinYeoung/coworker/internal/label"
)

func TestSequencerBlocksOnLowerOpenTasksOnly(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	lower := e.gh.AddIssue("lower", "touches x.go", label.Task, string(label.Queued))
	closed := e.gh.AddIssue("closed", "", label.Task)
	e.gh.CloseIssue(closed)
	task := e.gh.AddIssue("this one", "also touches x.go", label.Task, string(label.Pending))
	higher := e.gh.AddIssue("higher", "", label.Task, string(label.Pending))
	pr := e.gh.AddPull(branchName(lower), "main")
	e.gh.SetFiles(pr, "x.go")
	e.claude.answer(fmt.Sprintf(`{"blocked_by": [%d, %d, %d, %d, 99]}`, lower, closed, task, higher))
	s := &sequencer{e.deps}

	picked, err := s.Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{task, higher})
	if err := s.Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	wantInts(t, "blocked by", e.gh.Issue(task).BlockedBy, []int{lower})
	e.wantStatus(task, label.Queued)
	wantContains(t, "claude input", e.claude.lastInput(), "also touches x.go", "lower", "- x.go")
	wantNotContains(t, "claude input", e.claude.lastInput(), "higher")
}

func TestSequencerSkipsClaudeWithoutCandidates(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("only one", "", label.Task, string(label.Pending))

	if err := (&sequencer{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	if n := e.claude.callCount(); n != 0 {
		t.Errorf("claude called %d times, want none", n)
	}
	e.wantStatus(task, label.Queued)
}
