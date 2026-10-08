package worker

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/logging"
)

// The file the test setup scripts write. Git ignores it, the way it ignores what a real setup script creates.
const setupMarker = ".setup-ran"

func setupRepo(script string) map[string]string {
	return map[string]string{"app.txt": "v1\n", ".gitignore": setupMarker + "\n", setupScript: script}
}

// captureLog swaps the process-wide logger, so a test that calls it must not run in parallel.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(slog.NewTextHandler(&buf, nil))))
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

func TestBuilderDoesNotPushTheSetupMarker(t *testing.T) {
	t.Parallel()
	e := newEnv(t, setupRepo("echo ran > "+setupMarker+"\n"))
	prd, name := e.startedPRD()
	if _, err := os.Stat(filepath.Join(e.sbx.repo(name), setupMarker)); err != nil {
		t.Fatalf("setup did not run: %v", err)
	}
	e.play(name, commit(t, "feat: add feature", map[string]string{"feature.txt": "hello\n"}), builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Review)
	files := gittest.Run(t, e.origin, "ls-tree", "-r", "--name-only", branchName(prd))
	wantContains(t, "pushed files", files, "feature.txt")
	wantNotContains(t, "pushed files", files, setupMarker)
}

func TestBuilderRunsBaseSetupOnPRBranch(t *testing.T) {
	t.Parallel()
	e := newEnv(t, setupRepo("echo base > "+setupMarker+"\n"))
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
	branch := branchName(prd)
	gittest.Commit(t, e.origin, branch, "main", map[string]string{
		setupScript:   "echo branch > " + setupMarker + "\n",
		"feature.txt": "first\n",
	})
	e.gh.AddPull(branch, "main")

	e.step(prd)

	b, err := os.ReadFile(filepath.Join(e.sbx.repo(e.builderBox(prd)), setupMarker))
	if err != nil || string(b) != "base\n" {
		t.Errorf("setup marker = %q, %v, want the base branch's script to have run", b, err)
	}
}

func TestBuilderRequeuesWhenSetupIsInterrupted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, setupRepo("touch "+setupMarker+"\nexec sleep 10\n"))
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelWhenExists(ctx, cancel, filepath.Join(e.sbx.repo(e.builderBox(prd)), setupMarker))

	if err := e.builder().Handle(ctx, prd); err == nil {
		t.Fatal("Handle returned nil for a setup interrupted by shutdown")
	}

	e.wantStatus(prd, label.Ready)
	e.wantRemoved(e.builderBox(prd))
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("interrupted setup left %d comments", n)
	}
}

func TestResolverRunsSetupBeforeMerge(t *testing.T) {
	t.Parallel()
	// The first line fails if the merge has already started.
	script := "test ! -e \"$(git rev-parse --git-path MERGE_HEAD)\"\necho ran > " + setupMarker + "\n"
	e := newEnv(t, setupRepo(script))
	prd, name, _ := e.conflicted()
	if _, err := os.Stat(filepath.Join(e.sbx.repo(name), setupMarker)); err != nil {
		t.Fatalf("setup did not run: %v", err)
	}
	wantContains(t, "claude input", e.readCtl(name, "input.md"), "## Setup", "before the merge started")
	e.play(name, resolveWith(t, "main\nfeature\n"), resolverDone)
	e.resolve(prd)

	e.wantStatus(prd, label.Review)
	if n := len(gittest.Parents(t, e.origin, branchName(prd))); n != 2 {
		t.Errorf("branch head has %d parents, want a merge commit", n)
	}
}

func TestResolverStuckWhenSetupFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t, setupRepo("exit 1\n"))
	prd, _, head := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	e.resolve(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "`"+setupScript+"` did not end cleanly (exit status 1)", label.Review)
	if got := gittest.Head(e.origin, branchName(prd)); got != head {
		t.Errorf("branch moved to %s, want it left at %s", got, head)
	}
	e.wantRemoved(e.resolverBox(prd))
}
