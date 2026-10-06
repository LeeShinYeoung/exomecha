package worker

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

// The file the test setup scripts write. Git ignores it, the way it ignores what a real setup script creates.
const setupMarker = ".setup-ran"

func setupRepo(script string) map[string]string {
	return map[string]string{"app.txt": "v1\n", ".gitignore": setupMarker + "\n", setupScript: script}
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

// The workers that only read the code run in a sandbox without the setup script, so a broken
// script must not get in their way.
func TestReadersDoNotRunSetup(t *testing.T) {
	for _, c := range []struct {
		role role
		// start starts the worker's run and returns its issue.
		start func(e *env) int
	}{
		{reviewerRole, func(e *env) int {
			task, _, _, _ := e.reviewing()
			return task
		}},
	} {
		t.Run(c.role.name, func(t *testing.T) {
			e := newEnv(t, setupRepo("exit 1\n"))
			name := e.runs.name(c.role, c.start(e))

			e.wantState(name, running)
			if _, err := os.Stat(filepath.Join(e.runs.ctl(name), "setup.sh")); err == nil {
				t.Error("the setup script went into the run's directory")
			}
			wantNotContains(t, "claude input", e.readCtl(name, "input.md"), "## Setup")
		})
	}
}

func TestBuilderRunsSetupBeforeClaude(t *testing.T) {
	e := newEnv(t, setupRepo(`echo "task $EXOMECHA_TASK" > `+setupMarker+"\n"))
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
	task := e.gh.AddIssue("Task", "", label.PRD, string(label.Queued))

	e.step(task)

	e.wantStatus(task, label.Stuck)
	comment := e.lastComment(task)
	wantStuckComment(t, comment, "`"+setupScript+"` did not end cleanly (exit status 1)", label.Queued)
	wantNotContains(t, "stuck comment", comment, "a secret in the output", "merge the base branch")
	wantContains(t, "log", log.String(), "a secret in the output")
	e.wantRemoved(e.builderBox(task))
	if head := gittest.Head(e.origin, branchName(task)); head != "" {
		t.Errorf("branch was pushed after a failed setup: %s", head)
	}
}

func TestBuilderRunsBaseSetupOnPRBranch(t *testing.T) {
	e := newEnv(t, setupRepo("echo base > "+setupMarker+"\n"))
	task := e.gh.AddIssue("Task", "", label.PRD, string(label.Queued))
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
	task := e.gh.AddIssue("Task", "", label.PRD, string(label.Queued))
	branch := branchName(task)
	gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	e.gh.AddPull(branch, "main")
	gittest.Commit(t, e.origin, "main", "", setupRepo("echo ran > "+setupMarker+"\n"))

	e.step(task)

	e.wantStatus(task, label.Stuck)
	comment := e.lastComment(task)
	wantStuckComment(t, comment, "changed the working copy", label.Queued)
	wantContains(t, "stuck comment", comment, "merge the base branch into `"+branch+"`")
}

func TestBuilderRequeuesWhenSetupIsInterrupted(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("SETUP_STARTED", started)
	e := newEnv(t, setupRepo("touch \"$SETUP_STARTED\"\nexec sleep 10\n"))
	task := e.gh.AddIssue("Task", "", label.PRD, string(label.Queued))
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

func TestResolverRunsSetupBeforeMerge(t *testing.T) {
	// The first line fails if the merge has already started.
	script := "test ! -e \"$(git rev-parse --git-path MERGE_HEAD)\"\necho ran > " + setupMarker + "\n"
	e := newEnv(t, setupRepo(script))
	task, name, _ := e.conflicted()
	if _, err := os.Stat(filepath.Join(e.sbx.repo(name), setupMarker)); err != nil {
		t.Fatalf("setup did not run: %v", err)
	}
	wantContains(t, "claude input", e.readCtl(name, "input.md"), "## Setup", "before the merge started")
	e.play(name, resolveWith(t, "main\nfeature\n"), resolverDone)
	e.resolve(task)

	e.wantStatus(task, label.Review)
	if n := len(gittest.Parents(t, e.origin, branchName(task))); n != 2 {
		t.Errorf("branch head has %d parents, want a merge commit", n)
	}
}

func TestResolverStuckWhenSetupFails(t *testing.T) {
	e := newEnv(t, setupRepo("exit 1\n"))
	task, _, head := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	e.resolve(task)

	e.wantStatus(task, label.Stuck)
	wantStuckComment(t, e.lastComment(task), "`"+setupScript+"` did not end cleanly (exit status 1)", label.Review)
	if got := gittest.Head(e.origin, branchName(task)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
	e.wantRemoved(e.resolverBox(task))
}

func TestResolverSkipsSetupWhenNothingToMerge(t *testing.T) {
	e := newEnv(t, setupRepo("exit 1\n"))
	task := e.gh.AddIssue("Task", "task body", label.PRD, string(label.Review))
	gittest.Commit(t, e.origin, branchName(task), "main", map[string]string{"app.txt": "feature\n"})
	pr := e.gh.AddPull(branchName(task), "main")
	e.gh.SetMergeable(pr, new(false))

	e.resolve(task)

	e.wantStatus(task, label.Review)
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Errorf("a failing setup script ran with nothing to merge: %d comments", n)
	}
}
