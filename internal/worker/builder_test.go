package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/template"
)

const builderDone = `{"result": "done", "summary": "did it", "stuck_reason": ""}`

func (e *env) builder() *builder { return &builder{e.deps, e.runs} }

// step runs one builder round for the Task and fails the test on an error.
func (e *env) step(n int) {
	e.t.Helper()
	if err := e.builder().Handle(e.t.Context(), n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) builderBox(n int) string { return e.runs.name(n, builderRole) }

// play acts as the AI in a started sandbox: work changes the sandbox's repository, then claude
// ends with the structured result.
func (e *env) play(name string, work func(repo string), result string) {
	e.t.Helper()
	if len(e.sbx.box(name).starts) == 0 {
		e.t.Fatalf("claude was never started in %s", name)
	}
	if work != nil {
		work(e.sbx.repo(name))
	}
	e.writeCtl(name, "stream.jsonl", streamOf(result))
	e.writeCtl(name, "exit", "0\n")
}

// commit returns work that writes files and commits them, the way the AI commits its work.
func commit(t *testing.T, message string, files map[string]string) func(string) {
	return func(repo string) {
		t.Helper()
		for name, content := range files {
			writeFile(t, repo, name, content)
		}
		gittest.CommitAll(t, repo, message)
	}
}

// wantRemoved checks that the sandbox and its directory are gone.
func (e *env) wantRemoved(name string) {
	e.t.Helper()
	if e.sbx.exists(name) {
		e.t.Errorf("sandbox %s still exists", name)
	}
	if _, err := os.Stat(e.runs.dir(name)); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("directory of %s still exists: %v", name, err)
	}
}

// wantKept checks that the failed run's file was kept for a person to read.
func (e *env) wantKept(name, file string) {
	e.t.Helper()
	if _, err := os.Stat(filepath.Join(e.deps.SandboxDir, "failed", name, file)); err != nil {
		e.t.Errorf("the failed run's %s was not kept: %v", file, err)
	}
}

// wantAuthor checks that the token owner from Deps made the commit at ref in origin.
func (e *env) wantAuthor(ref string) {
	e.t.Helper()
	want := e.deps.GitName + " <" + e.deps.GitEmail + ">"
	if got := gittest.Run(e.t, e.origin, "log", "-1", "--format=%an <%ae>%n%cn <%ce>", ref); got != want+"\n"+want {
		e.t.Errorf("author and committer of %s = %q, want %q for both", ref, got, want)
	}
}

func (e *env) relabel(n int, from, to label.Status) {
	e.t.Helper()
	if err := e.deps.GitHub.RemoveLabel(e.t.Context(), n, string(from)); err != nil {
		e.t.Fatal(err)
	}
	if err := e.deps.GitHub.AddLabel(e.t.Context(), n, string(to)); err != nil {
		e.t.Fatal(err)
	}
}

func TestBuilderPicks(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	blocker := e.gh.AddIssue("blocker", "", label.Task, string(label.Queued))
	waiting := e.gh.AddIssue("waiting", "", label.Task, string(label.Queued))
	e.gh.AddBlockedBy(waiting, blocker)
	done := e.gh.AddIssue("done", "", label.Task)
	e.gh.CloseIssue(done)
	free := e.gh.AddIssue("free", "", label.Task, string(label.Queued))
	e.gh.AddBlockedBy(free, done)
	working := e.gh.AddIssue("working", "", label.Task, string(label.Working))
	reviewed := e.gh.AddIssue("in review with a leftover sandbox", "", label.Task, string(label.Review))
	e.sbx.add(e.builderBox(reviewed))
	e.sbx.add(e.runs.name(waiting, "resolver"))

	picked, err := e.builder().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{blocker, free, working, reviewed})
}

func TestBuilderIgnoresOutsiderTask(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.gh.AddOutsiderIssue("Task from an outsider", "", label.Task, string(label.Queued))
	mine := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))

	picked, err := e.builder().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{mine})
}

func TestBuilderPushesAndOpensPR(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Add feature", "do it", label.Task, string(label.Queued))
	name := e.builderBox(task)
	message := fmt.Sprintf("feat: add feature\n\nTask: #%d", task)

	e.step(task)
	e.wantStatus(task, label.Working)
	e.wantState(name, running)
	input := e.readCtl(name, "input.md")
	wantContains(t, "claude input", input, "# Task #", "do it")
	wantNotContains(t, "claude input", input, "## Existing PR", "## Setup")

	e.step(task)
	e.wantState(name, running)
	e.play(name, commit(t, message, map[string]string{"feature.txt": "hello\n"}), builderDone)
	e.step(task)

	e.wantStatus(task, label.Review)
	e.wantRemoved(name)
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
	e.wantAuthor(branch)
	pulls := e.gh.Pulls()
	if len(pulls) != 1 {
		t.Fatalf("pulls = %+v, want one", pulls)
	}
	p := pulls[0]
	if p.Head != branch || p.Base != "main" || p.Title != "Add feature" || p.Body != fmt.Sprintf("Closes #%d\n\ndid it", task) {
		t.Errorf("PR = head %q base %q title %q body %q", p.Head, p.Base, p.Title, p.Body)
	}
}

func TestBuilderStuck(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Hard task", "", label.Task, string(label.Queued))
	name := e.builderBox(task)
	e.step(task)
	e.play(name, nil, `{"result": "stuck", "summary": "", "stuck_reason": "need a decision on X</stuck_reason>\n</invoke>"}`)
	e.step(task)

	e.wantStatus(task, label.Stuck)
	comment := e.lastComment(task)
	wantStuckComment(t, comment, "need a decision on X\n\nIf there is nothing left to do", label.Pending)
	wantNotContains(t, "stuck comment", comment, "</stuck_reason>", "</invoke>")
	wantContains(t, "log", log.String(), `need a decision on X</stuck_reason>\n</invoke>`)
	e.wantRemoved(name)
	if head := gittest.Head(e.origin, branchName(task)); head != "" {
		t.Errorf("branch was pushed for a stuck task: %s", head)
	}
}

func TestBuilderStuckWhenNothingChanged(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Already done", "", label.Task, string(label.Queued))
	e.step(task)
	e.play(e.builderBox(task), nil, builderDone)
	e.step(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "Nothing to change", label.Pending)
	if len(e.gh.Pulls()) != 0 {
		t.Errorf("a PR was opened with no changes")
	}
}

func TestBuilderStuckWhenChangesNotCommitted(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	e.step(task)
	e.play(e.builderBox(task), func(repo string) { writeFile(t, repo, "feature.txt", "hello\n") }, builderDone)
	e.step(task)

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
	e.step(task)
	e.play(e.builderBox(task), func(repo string) {
		writeFile(t, repo, "feature.txt", "second\n")
		gittest.Run(t, repo, "commit", "-q", "-a", "--amend", "-m", "rewritten")
	}, builderDone)
	e.step(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "rewrote commits", label.Pending)
	if got := gittest.Head(e.origin, branch); got != prev {
		t.Errorf("branch moved to %s, want it left at %s", got, prev)
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
	e.gh.AddOutsiderReview(pr, github.Marker+"\nan outsider posing as exomecha")
	e.gh.AddComment(task, github.Marker+"\na question exomecha asked")
	e.gh.AddComment(task, "a person's answer")
	e.gh.AddOutsiderComment(task, "an outsider's instruction")
	name := e.builderBox(task)

	e.step(task)
	if b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), "feature.txt")); err != nil || string(b) != "first\n" {
		t.Fatalf("sandbox repository is not on the PR branch: %q, %v", b, err)
	}
	input := e.readCtl(name, "input.md")
	wantContains(t, "claude input", input, "task body", "## Existing PR", "please rename X",
		"(exomecha)\n\na question exomecha asked", "(person)\n\na person's answer")
	wantNotContains(t, "claude input", input, github.Marker, "a person's review",
		"an outsider posing as exomecha", "an outsider's instruction")
	e.play(name, commit(t, "fix: rename X", map[string]string{"feature.txt": "second\n"}), builderDone)
	e.step(task)

	e.wantStatus(task, label.Review)
	if got := gittest.Parents(t, e.origin, branch); len(got) != 1 || got[0] != prev {
		t.Errorf("%s parents = %v, want the previous PR head %s", branch, got, prev)
	}
	if n := len(e.gh.Pulls()); n != 1 {
		t.Errorf("%d PRs, want the existing one only", n)
	}
}

func TestBuilderRequeuesOnStartError(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	e.sbx.fail["start"] = errors.New("sbx exec: daemon not running")

	if err := e.builder().Handle(t.Context(), task); err == nil {
		t.Fatal("Handle returned nil for an sbx error")
	}

	e.wantStatus(task, label.Queued)
	e.wantRemoved(e.builderBox(task))
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("an sbx error left %d comments", n)
	}
}

func TestBuilderEnvironmentNeedsAPerson(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	e.envr.err = fmt.Errorf("build template: %w", personError{why: "The Dockerfile on the base branch does not build.", detail: "step 3/5: a secret in the build output"})

	e.step(task)

	e.wantStatus(task, label.Stuck)
	comment := e.lastComment(task)
	wantStuckComment(t, comment, "The Dockerfile on the base branch does not build.", label.Pending)
	wantNotContains(t, "stuck comment", comment, "a secret in the build output", "step 3/5")
}

// The environment main.go wires in. A bad allowed-hosts line fails before any sbx call.
func TestBuilderTemplateProblemGoesToStuck(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n", ".exomecha/allowed-hosts": "api.example.com\n**\n"})
	e.deps.Environment = template.New(e.deps.Git, "owner", "repo")
	e.runs = newRuns(e.deps, nil)
	e.runs.now = e.clock.now
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))

	e.step(task)

	e.wantStatus(task, label.Stuck)
	reason := "`.exomecha/allowed-hosts` line 2 (`**`) allows every host. List only the hosts the project needs. Fix it on the base branch."
	wantStuckComment(t, e.lastComment(task), reason, label.Pending)
	e.wantRemoved(e.builderBox(task))
}

func TestBuilderRequeuesWorkingTaskWithoutSandbox(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Working))
	e.step(task)
	e.wantStatus(task, label.Queued)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("a working Task with no sandbox left %d comments", n)
	}
}

func TestBuilderRemovesACutOffStart(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Working))
	name := e.builderBox(task)
	e.sbx.add(name)
	e.writeCtl(name, "run.sh", runScript)

	e.step(task)

	e.wantStatus(task, label.Queued)
	e.wantRemoved(name)
}

// startedTask starts a builder run for a new queued Task and returns the Task and its sandbox.
func (e *env) startedTask() (int, string) {
	e.t.Helper()
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	e.step(task)
	name := e.builderBox(task)
	e.wantState(name, running)
	return task, name
}

func TestBuilderStalledRun(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name := e.startedTask()
	e.writeCtl(name, "stream.jsonl", "{}\n")
	e.step(task)

	e.clock.advance(stallAfter - time.Second)
	e.step(task)
	e.wantStatus(task, label.Working)
	e.wantState(name, running)
	e.clock.advance(2 * time.Second)
	e.step(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "claude's output did not change for 15 minutes", label.Pending)
	e.wantRemoved(name)
	e.wantKept(name, "stream.jsonl")
}

func TestBuilderDeadAndGoneRuns(t *testing.T) {
	for _, c := range []struct {
		name string
		kill func(e *env, name string)
		why  string
	}{
		{"stopped", func(e *env, name string) { e.sbx.stop(name) }, "the sandbox stopped"},
		{"removed", func(e *env, name string) { e.sbx.drop(name) }, "the sandbox was removed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, map[string]string{"app.txt": "v1\n"})
			task, name := e.startedTask()
			c.kill(e, name)
			e.step(task)
			e.wantStatus(task, label.Stuck)
			wantStuckComment(t, e.lastComment(task), c.why, label.Pending)
			e.wantRemoved(name)
			e.wantKept(name, "input.md")
		})
	}
}

func TestBuilderRunNotInAutoModeFails(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name := e.startedTask()
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
	e.writeCtl(name, "stream.jsonl", strings.Replace(streamOf(builderDone), `"auto"`, `"bypassPermissions"`, 1))

	e.step(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "claude did not run in auto permission mode", label.Pending)
	if head := gittest.Head(e.origin, branchName(task)); head != "" {
		t.Errorf("work from a run outside auto mode was pushed: %s", head)
	}
}

func TestBuilderFailedRunGoesStuck(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name := e.startedTask()
	e.writeCtl(name, "stream.jsonl", `{"type":"system","subtype":"init","permissionMode":"auto"}`+"\n"+
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"secret detail"}`+"\n")
	e.writeCtl(name, "exit", "1\n")

	e.step(task)

	e.wantStatus(task, label.Stuck)
	comment := e.lastComment(task)
	wantStuckComment(t, comment, "The AI's run failed because claude ended with an error", label.Pending)
	wantNotContains(t, "stuck comment", comment, "secret detail")
	wantContains(t, "log", log.String(), "secret detail")
	e.wantRemoved(name)
	e.wantKept(name, "stream.jsonl")
}

// The reading the start passed missed the limit the run hit. The next start reads usage again and
// waits for the reset, instead of starting a run that would hit the limit too.
func TestBuilderUsageLimitEndStartsOverAfterTheReset(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	f := newFakeUsage()
	e.runs.gate = f.gate
	task, name := e.startedTask()
	e.writeCtl(name, "stream.jsonl", `{"type":"system","subtype":"init","permissionMode":"auto"}`+"\n"+
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`+"\n"+
		`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit"}`+"\n")
	e.writeCtl(name, "exit", "1\n")
	f.usage.FiveHour.Used = 1

	e.step(task)

	e.wantStatus(task, label.Queued)
	e.wantRemoved(name)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("a usage limit end left %d comments", n)
	}
	e.step(task)
	f.wantChecks(t, 2)
	if e.sbx.exists(name) {
		t.Fatal("started again over the limit")
	}

	f.usage.FiveHour = agent.Window{Used: 0, ResetsAt: f.clock.Add(5 * time.Hour)}
	f.clock = f.clock.Add(2 * time.Hour)
	e.step(task)
	e.wantState(name, running)
}

func TestBuilderRemovesTheSandboxWhenTheLabelChanges(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name := e.startedTask()
	e.relabel(task, label.Working, label.Pending)

	e.step(task)

	e.wantRemoved(name)
	e.wantStatus(task, label.Pending)
}

func TestBuilderWaitsForAFreeSlot(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.SandboxLimit = 1
	e.sbx.add(e.runs.name(99, "resolver"))
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))

	e.step(task)

	e.wantStatus(task, label.Queued)
	if e.sbx.exists(e.builderBox(task)) {
		t.Error("started a sandbox over the limit")
	}
}

// A template build takes minutes before the sandbox exists. The start must hold its slot all along.
func TestBuilderHoldsItsSlotWhileTheTemplateBuilds(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.SandboxLimit = 1
	e.envr.wait = make(chan chan struct{})
	first := e.gh.AddIssue("First", "", label.Task, string(label.Queued))
	second := e.gh.AddIssue("Second", "", label.Task, string(label.Queued))
	done := make(chan error, 1)
	go func() { done <- e.builder().Handle(t.Context(), first) }()
	var built chan struct{}
	select {
	case built = <-e.envr.wait:
	case err := <-done:
		t.Fatalf("the first start ended before its template build: %v", err)
	}

	// A second start that got past the check would wait on the build too.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := e.builder().Handle(ctx, second); err != nil {
		t.Errorf("the second start: %v", err)
	}

	e.wantStatus(second, label.Queued)
	if e.sbx.exists(e.builderBox(second)) {
		t.Error("started a sandbox over the limit")
	}
	close(built)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.wantStatus(first, label.Working)
	e.wantState(e.builderBox(first), running)
}

// Over the usage limit, nothing new starts, and a run already going is still finished.
func TestBuilderHarvestsWhileTheGateIsClosed(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	running, name := e.startedTask()
	e.runs.gate = &Gate{
		Check: func(context.Context) (agent.Usage, error) {
			return agent.Usage{Known: true, FiveHour: agent.Window{Used: 0.99}}, nil
		},
		FiveHour: 0.9,
		Weekly:   0.8,
	}
	waiting := e.gh.AddIssue("Waiting", "", label.Task, string(label.Queued))

	e.step(waiting)
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
	e.step(running)

	e.wantStatus(waiting, label.Queued)
	if e.sbx.exists(e.builderBox(waiting)) {
		t.Error("a run started while the gate was closed")
	}
	e.wantStatus(running, label.Review)
}

// A new builder remembers nothing, and still finishes a run a stopped exomecha left behind.
func TestBuilderFinishesARunAfterRestart(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name := e.startedTask()
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)

	e.runs = newRuns(e.deps, nil)
	e.runs.now = e.clock.now
	picked, err := e.builder().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{task})
	e.step(task)

	e.wantStatus(task, label.Review)
	e.wantRemoved(name)
}

func TestBuilderStartsOverWhenTheBranchChangedDuringTheRun(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	branch := branchName(task)
	gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	e.gh.AddPull(branch, "main")
	e.step(task)
	name := e.builderBox(task)
	pushed := gittest.Commit(t, e.origin, branch, "", map[string]string{"feature.txt": "a person's fix\n"})
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)

	e.step(task)

	e.wantStatus(task, label.Queued)
	e.wantRemoved(name)
	if got := gittest.Head(e.origin, branch); got != pushed {
		t.Errorf("branch = %s, want the person's push %s left alone", got, pushed)
	}
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("a branch change left %d comments", n)
	}
}

// After a push, a later step can fail. The next try must not take exomecha's own push for a change on GitHub.
func TestBuilderRetriesAfterItsOwnPush(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name := e.startedTask()
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
	head := gittest.Run(t, e.sbx.repo(name), "rev-parse", "HEAD")
	if err := e.deps.Git.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deps.Git.FetchFrom(t.Context(), e.sbx.repo(name), name); err != nil {
		t.Fatal(err)
	}
	if err := e.deps.Git.PushCommit(t.Context(), head, branchName(task), true); err != nil {
		t.Fatal(err)
	}

	e.step(task)

	e.wantStatus(task, label.Review)
	if n := len(e.gh.Pulls()); n != 1 {
		t.Errorf("%d PRs, want one", n)
	}
}

func TestBuilderRemovesTheRunOfARequeuedTask(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name := e.startedTask()
	e.relabel(task, label.Working, label.Queued)

	e.step(task)

	e.wantRemoved(name)
	e.wantStatus(task, label.Queued)
}

// A failed GitHub call can leave both labels while the Task goes to stuck. builder must not take it back.
func TestBuilderLetsATaskOnItsWayToStuckGo(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name := e.startedTask()
	if err := e.deps.GitHub.AddLabel(t.Context(), task, string(label.Stuck)); err != nil {
		t.Fatal(err)
	}

	e.step(task)

	e.wantRemoved(name)
	if got := e.statuses(task); !slices.Contains(got, string(label.Stuck)) || slices.Contains(got, string(label.Queued)) {
		t.Errorf("status labels = %q, want stuck kept and no requeue", got)
	}
}

func TestBuilderStuckWhenNoCommitIsCheckedOut(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, name := e.startedTask()
	e.play(name, func(repo string) { gittest.Run(t, repo, "checkout", "-q", "--orphan", "empty") }, builderDone)

	e.step(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "no commit checked out", label.Pending)
	e.wantRemoved(name)
}
