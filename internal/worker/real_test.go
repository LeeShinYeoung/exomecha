package worker

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
	"github.com/LeeShinYeoung/coworker/internal/label"
	"github.com/LeeShinYeoung/coworker/internal/sandbox"
)

// TestRealBuilderRun takes a small Task through builder with the real sbx and the real claude,
// against a fake GitHub and a local origin. It spends Claude usage and takes a few minutes, so it
// runs only with COWORKER_TEST_SBX=1 and COWORKER_TEST_CLAUDE=1.
func TestRealBuilderRun(t *testing.T) {
	if os.Getenv("COWORKER_TEST_SBX") != "1" || os.Getenv("COWORKER_TEST_CLAUDE") != "1" {
		t.Skip("set COWORKER_TEST_SBX=1 and COWORKER_TEST_CLAUDE=1 to run claude in a real sandbox")
	}
	// sbx gives the sandbox the commit identity of this machine's git, which these tests point
	// at a throwaway config. The setup script sets one for the sandbox's repository instead.
	e := newEnv(t, map[string]string{
		"README.md": "A practice project.\n",
		setupScript: "git config user.name 'coworker test'\ngit config user.email test@example.com\n",
	})
	sbx := sandbox.CLI{}
	e.deps.Sandboxes = sbx
	e.deps.SandboxPrefix = fmt.Sprintf("impl-run-w%d-", time.Now().UnixNano()%100_000)
	e.box = newTaskbox(e.deps, nil)
	task := e.gh.AddIssue("Add hello.txt",
		"## To do\n\nCreate a file `hello.txt` at the repository root whose only line is `hello`. Commit it with the message `feat: add hello`.\n",
		label.Task, string(label.Queued))
	name := e.builderBox(task)
	t.Cleanup(func() {
		if err := sbx.Remove(context.Background(), name); err != nil {
			t.Errorf("remove %s: %v", name, err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	for {
		if err := e.builder().Handle(ctx, task); err != nil {
			t.Fatal(err)
		}
		status := e.statuses(task)
		if len(status) != 1 || status[0] != string(label.Working) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("still working after 10 minutes. stderr: %s", e.box.tail(name, "stderr.log"))
		case <-time.After(5 * time.Second):
		}
	}

	if got := e.statuses(task); len(got) != 1 || got[0] != string(label.Review) {
		t.Fatalf("status = %q, last comment %q", got, e.gh.Issue(task).Comments)
	}
	if got := gittest.ReadFile(t, e.origin, branchName(task), "hello.txt"); got != "hello" {
		t.Errorf("hello.txt = %q", got)
	}
	if n := len(e.gh.Pulls()); n != 1 {
		t.Errorf("%d PRs, want one", n)
	}
	if boxes, err := sbx.List(t.Context()); err == nil {
		for _, b := range boxes {
			if b.Name == name {
				t.Errorf("sandbox %s left after the PR opened", name)
			}
		}
	}
}
