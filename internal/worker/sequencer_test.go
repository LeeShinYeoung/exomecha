package worker

import (
	"fmt"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/label"
)

func (e *env) sequencer() *sequencer { return &sequencer{e.deps, e.runs} }

// sequence runs one sequencer round for the Task and fails the test on an error.
func (e *env) sequence(n int) {
	e.t.Helper()
	if err := e.sequencer().Handle(e.t.Context(), n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) sequencerBox(n int) string { return e.runs.name(sequencerRole, n) }

// sequencing is a pending Task with one lower open Task, with sequencer's run started for it.
func (e *env) sequencing() (task, lower int, name string) {
	e.t.Helper()
	lower = e.gh.AddIssue("lower", "touches x.go", label.Task, string(label.Queued))
	task = e.gh.AddIssue("this one", "also touches x.go", label.Task, string(label.Pending))
	e.sequence(task)
	name = e.sequencerBox(task)
	e.wantState(name, running)
	return task, lower, name
}

func TestSequencerBlocksOnLowerOpenTasksOnly(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	lower := e.gh.AddIssue("lower", "touches x.go", label.Task, string(label.Queued))
	closed := e.gh.AddIssue("closed", "", label.Task)
	e.gh.CloseIssue(closed)
	task := e.gh.AddIssue("this one", "also touches x.go", label.Task, string(label.Pending))
	higher := e.gh.AddIssue("higher", "", label.Task, string(label.Pending))
	pr := e.gh.AddPull(branchName(lower), "main")
	e.gh.SetFiles(pr, "x.go")
	name := e.sequencerBox(task)

	picked, err := e.sequencer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{task, higher})
	e.sequence(task)
	e.wantState(name, running)
	input := e.readCtl(name, "input.md")
	wantContains(t, "claude input", input, "also touches x.go", "lower", "- x.go")
	wantNotContains(t, "claude input", input, "higher")
	e.play(name, nil, fmt.Sprintf(`{"blocked_by": [%d, %d, %d, %d, 99]}`, lower, closed, task, higher))
	e.sequence(task)

	wantInts(t, "blocked by", e.gh.Issue(task).BlockedBy, []int{lower})
	e.wantStatus(task, label.Queued)
	e.wantRemoved(name)
}

func TestSequencerPicksEveryTaskWithARun(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	pending := e.gh.AddIssue("pending", "", label.Task, string(label.Pending))
	queued := e.gh.AddIssue("queued with a leftover sandbox", "", label.Task, string(label.Queued))
	e.sbx.add(e.sequencerBox(queued))
	e.sbx.add(e.runs.name(splitterRole, 99))

	picked, err := e.sequencer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{pending, queued})
}

func TestSequencerSkipsClaudeWithoutCandidates(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("only one", "", label.Task, string(label.Pending))

	e.sequence(task)

	e.wantStatus(task, label.Queued)
	e.wantRemoved(e.sequencerBox(task))
}

func TestSequencerCandidateClosedDuringTheRun(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, lower, name := e.sequencing()
	e.gh.CloseIssue(lower)
	e.play(name, nil, fmt.Sprintf(`{"blocked_by": [%d]}`, lower))

	e.sequence(task)

	wantInts(t, "blocked by", e.gh.Issue(task).BlockedBy, nil)
	e.wantStatus(task, label.Queued)
	e.wantRemoved(name)
}

func TestSequencerFirstFailureGoesStuckBackToPending(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, _, name := e.sequencing()
	e.writeCtl(name, "stream.jsonl", `{"type":"system","subtype":"init","permissionMode":"auto"}`+"\n"+
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}`+"\n")
	e.writeCtl(name, "exit", "1\n")

	e.sequence(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "The AI's run failed because claude ended with an error", label.Pending)
	e.wantRemoved(name)
	e.wantKept(name, "stream.jsonl")
	wantInts(t, "blocked by", e.gh.Issue(task).BlockedBy, nil)
}
