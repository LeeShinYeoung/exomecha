package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/git"
	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
	"github.com/LeeShinYeoung/coworker/internal/label"
)

// The file the test setup scripts write. Git ignores it, the way it ignores what a real setup script creates.
const setupMarker = ".setup-ran"

func setupRepo(script string) map[string]string {
	return map[string]string{"app.txt": "v1\n", ".gitignore": setupMarker + "\n", setupScript: script}
}

func (e *env) freshWorktree() *git.Worktree {
	e.t.Helper()
	wt, err := e.deps.Git.NewBranch(e.t.Context(), branchName(1))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(wt.Remove)
	return wt
}

func (e *env) runSetup(ctx context.Context) (ran bool, problem string, err error) {
	return setup(ctx, e.freshWorktree(), e.deps.Git.BaseRef(), 1)
}

// wantSetupProblem runs setup and checks it blames the script, not the environment.
func (e *env) wantSetupProblem(subs ...string) {
	e.t.Helper()
	ran, problem, err := e.runSetup(e.t.Context())
	if err != nil || ran || problem == "" {
		e.t.Fatalf("setup = ran %v, problem %q, err %v, want a problem with the script", ran, problem, err)
	}
	wantContains(e.t, "setup problem", problem, subs...)
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// cancelWhenExists cancels once the setup script has created path, so the cancel lands while it runs.
func cancelWhenExists(ctx context.Context, cancel context.CancelFunc, path string) {
	go func() {
		for {
			if _, err := os.Stat(path); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
}

func TestSetupWithoutScript(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	if ran, problem, err := e.runSetup(t.Context()); ran || problem != "" || err != nil {
		t.Errorf("setup = ran %v, problem %q, err %v, want nothing to happen", ran, problem, err)
	}
}

func TestSetupUsesPipefail(t *testing.T) {
	e := newEnv(t, setupRepo("false | true\n"))
	e.wantSetupProblem("did not end cleanly (exit status 1)")
}

func TestSetupScriptDoesNotGetGitAuth(t *testing.T) {
	e := newEnv(t, setupRepo("env > "+setupMarker+"\n"))
	wt := e.freshWorktree()

	if ran, problem, err := setup(t.Context(), wt, e.deps.Git.BaseRef(), 1); !ran || problem != "" || err != nil {
		t.Fatalf("setup = ran %v, problem %q, err %v", ran, problem, err)
	}

	b, err := os.ReadFile(filepath.Join(wt.Dir, setupMarker))
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "script environment", string(b), "COWORKER_TASK=1")
	// What the git runner adds to carry the token.
	wantNotContains(t, "script environment", string(b), "GIT_CONFIG_COUNT", "GIT_CONFIG_VALUE", "AUTHORIZATION")
}

func TestSetupWithoutShellIsAnEnvironmentError(t *testing.T) {
	setupShell = "coworker-test-no-such-shell"
	t.Cleanup(func() { setupShell = "bash" })
	e := newEnv(t, setupRepo("true\n"))

	if ran, problem, err := e.runSetup(t.Context()); ran || problem != "" || err == nil {
		t.Errorf("setup = ran %v, problem %q, err %v, want an error and no problem with the script", ran, problem, err)
	}
}

func TestSetupTimesOut(t *testing.T) {
	log := captureLog(t)
	setupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { setupTimeout = 10 * time.Minute })
	// exec makes sleep the process that gets killed, so nothing is left holding the output open.
	e := newEnv(t, setupRepo("exec sleep 30\n"))
	e.wantSetupProblem("did not end cleanly")
	wantContains(t, "log", log.String(), "timed_out=true")
}

func TestSetupInterruptedIsNotAProblemWithTheScript(t *testing.T) {
	log := captureLog(t)
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("SETUP_STARTED", started)
	e := newEnv(t, setupRepo("touch \"$SETUP_STARTED\"\nexec sleep 30\n"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelWhenExists(ctx, cancel, started)

	ran, problem, err := e.runSetup(ctx)

	if ran || problem != "" || !errors.Is(err, context.Canceled) {
		t.Errorf("setup = ran %v, problem %q, err %v, want only context.Canceled", ran, problem, err)
	}
	// A later git call fails on the cancelled context too, so the result alone does not show
	// that the killed script was never counted against the script.
	wantNotContains(t, "log", log.String(), "setup script rejected")
}

// Another worker's fetch can move the base ref while the script runs. A branch that was up to
// date when the script started must not be called behind.
func TestSetupOnFreshBranchIsNotCalledBehind(t *testing.T) {
	moveBase := "git update-ref refs/remotes/origin/main \"$(git commit-tree 'HEAD^{tree}' -p HEAD -m moved)\"\n"
	e := newEnv(t, setupRepo(moveBase+"exit 1\n"))

	_, problem, err := e.runSetup(t.Context())

	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "setup problem", problem, "did not end cleanly (exit status 1)")
	wantNotContains(t, "setup problem", problem, "merge the base branch")
}

func TestResolverRunsSetupBeforeMerge(t *testing.T) {
	// The first line fails if the merge has already started.
	script := "test ! -e \"$(git rev-parse --git-path MERGE_HEAD)\"\necho ran > " + setupMarker + "\n"
	e := newEnv(t, setupRepo(script))
	task, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
	e.claude.reply = func(req agent.Request) (string, error) {
		if _, err := os.Stat(filepath.Join(req.Dir, setupMarker)); err != nil {
			return "", fmt.Errorf("setup did not run before claude: %v", err)
		}
		writeFile(t, req.Dir, "app.txt", "main\nfeature\n")
		commitMerge(t, req.Dir)
		return `{"result": "resolved", "reason": "kept both lines"}`, nil
	}

	if err := (&resolver{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Review)
	if n := len(gittest.Parents(t, e.origin, branchName(task))); n != 2 {
		t.Errorf("branch head has %d parents, want a merge commit", n)
	}
	wantContains(t, "claude input", e.claude.lastInput(), "## Setup", "before the merge started")
}

func TestResolverStuckWhenSetupFails(t *testing.T) {
	e := newEnv(t, setupRepo("exit 1\n"))
	task, _, head := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	if err := (&resolver{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "`"+setupScript+"` did not end cleanly (exit status 1)", label.Review)
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
}

func TestResolverSkipsSetupWhenNothingToMerge(t *testing.T) {
	e := newEnv(t, setupRepo("exit 1\n"))
	task := e.gh.AddIssue("Task", "task body", label.Task, string(label.Review))
	gittest.Commit(t, e.origin, branchName(task), "main", map[string]string{"app.txt": "feature\n"})
	pr := e.gh.AddPull(branchName(task), "main")
	e.gh.SetMergeable(pr, new(false))

	if err := (&resolver{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	e.wantStatus(task, label.Review)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("a failing setup script ran with nothing to merge: %d comments", n)
	}
}

func TestSplitterDoesNotRunSetup(t *testing.T) {
	e := newEnv(t, setupRepo("exit 1\n"))
	prd := e.gh.AddIssue("PRD", "the plan", label.PRD)
	e.claude.answer(`{"tasks": [{"title": "A", "body": "a", "blocked_by": []}]}`)

	if err := (&splitter{e.deps}).Handle(t.Context(), prd); err != nil {
		t.Fatal(err)
	}

	if !e.gh.Issue(prd).HasLabel(label.Tasked) {
		t.Errorf("PRD has no %s label", label.Tasked)
	}
}

func TestSequencerDoesNotRunSetup(t *testing.T) {
	e := newEnv(t, setupRepo("exit 1\n"))
	e.gh.AddIssue("lower", "touches x.go", label.Task, string(label.Queued))
	task := e.gh.AddIssue("this one", "also touches x.go", label.Task, string(label.Pending))
	e.claude.answer(`{"blocked_by": []}`)

	if err := (&sequencer{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	if n := e.claude.callCount(); n != 1 {
		t.Errorf("claude called %d times, want once", n)
	}
	e.wantStatus(task, label.Queued)
}

func TestTailBufferKeepsABoundedValidEnd(t *testing.T) {
	var out tailBuffer
	for range 300 {
		out.Write([]byte(strings.Repeat("한", 1000)))
	}
	// Three bytes per character and eight here, so the cut at tailLimit lands inside a character.
	out.Write([]byte("the end."))

	if len(out.b) > 2*tailLimit+3000 {
		t.Errorf("buffer holds %d bytes after 900 KB written", len(out.b))
	}
	got := out.String()
	if len(got) > tailLimit || !utf8.ValidString(got) || !strings.HasSuffix(got, "한the end.") {
		t.Errorf("tail = %d bytes, valid UTF-8 %v, ends %q", len(got), utf8.ValidString(got), got[max(0, len(got)-12):])
	}
}

// reviewer only reads the code, so a broken setup script must not get in its way.
func TestReviewerDoesNotRunSetup(t *testing.T) {
	e := newEnv(t, setupRepo("exit 1\n"))
	task, pr, _ := e.reviewTask()
	e.claude.answer(`{"verdict": "approve", "body": "looks right"}`)

	if err := (&reviewer{e.deps}).Handle(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	if !e.gh.Pull(pr).Merged {
		t.Error("PR was not merged")
	}
	wantNotContains(t, "claude input", e.claude.lastInput(), "## Setup")
}

func TestBuilderRunsSetupBeforeClaude(t *testing.T) {
	e := newEnv(t, setupRepo(`echo "task $COWORKER_TASK" > `+setupMarker+"\n"))
	task, name := e.startedTask()
	b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), setupMarker))
	if want := fmt.Sprintf("task %d\n", task); err != nil || string(b) != want {
		t.Fatalf("setup marker = %q, %v, want %q", b, err, want)
	}
	wantContains(t, "claude input", e.readCtl(name, "input.md"), "## Setup", "before you started")
	e.play(name, commit(t, "feat: add feature", map[string]string{"feature.txt": "hello\n"}), builderDone)
	e.step(task)

	e.wantStatus(task, label.Review)
	files := gittest.Run(t, e.origin, "ls-tree", "-r", "--name-only", branchName(task))
	wantContains(t, "pushed files", files, "feature.txt")
	wantNotContains(t, "pushed files", files, setupMarker)
}

func TestBuilderStuckWhenSetupFails(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, setupRepo("echo 'a secret in the output' >&2\nfalse\nexit 0\n"))
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))

	e.step(task)

	e.wantStatus(task, label.Stuck)
	comment := e.lastComment(task)
	wantStuckComment(t, comment, "`"+setupScript+"` did not end cleanly (exit status 1)", label.Pending)
	wantNotContains(t, "stuck comment", comment, "a secret in the output", "merge the base branch")
	wantContains(t, "log", log.String(), "a secret in the output")
	e.wantRemoved(e.builderBox(task))
	if head := gittest.Head(e.origin, branchName(task)); head != "" {
		t.Errorf("branch was pushed after a failed setup: %s", head)
	}
}

func TestBuilderRunsBaseSetupOnPRBranch(t *testing.T) {
	e := newEnv(t, setupRepo("echo base > "+setupMarker+"\n"))
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	branch := branchName(task)
	gittest.Commit(t, e.origin, branch, "main", map[string]string{
		setupScript:   "echo branch > " + setupMarker + "\n",
		"feature.txt": "first\n",
	})
	e.gh.AddPull(branch, "main")

	e.step(task)

	b, err := os.ReadFile(filepath.Join(e.sbx.repo(e.builderBox(task)), setupMarker))
	if err != nil || string(b) != "base\n" {
		t.Errorf("setup marker = %q, %v, want the base branch's script to have run", b, err)
	}
}

func TestBuilderStuckOnBranchOlderThanSetup(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	branch := branchName(task)
	gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	e.gh.AddPull(branch, "main")
	gittest.Commit(t, e.origin, "main", "", setupRepo("echo ran > "+setupMarker+"\n"))

	e.step(task)

	e.wantStatus(task, label.Stuck)
	comment := e.lastComment(task)
	wantStuckComment(t, comment, "changed the working copy", label.Pending)
	wantContains(t, "stuck comment", comment, "merge the base branch into `"+branch+"`")
}

func TestBuilderRequeuesWhenSetupIsInterrupted(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("SETUP_STARTED", started)
	e := newEnv(t, setupRepo("touch \"$SETUP_STARTED\"\nexec sleep 10\n"))
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelWhenExists(ctx, cancel, started)

	if err := e.builder().Handle(ctx, task); err == nil {
		t.Fatal("Handle returned nil for a setup interrupted by shutdown")
	}

	e.wantStatus(task, label.Queued)
	e.wantRemoved(e.builderBox(task))
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("interrupted setup left %d comments", n)
	}
}
