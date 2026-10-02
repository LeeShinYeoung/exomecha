package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
	"github.com/LeeShinYeoung/coworker/internal/label"
	"github.com/LeeShinYeoung/coworker/internal/sandbox"
	"github.com/LeeShinYeoung/coworker/internal/template"
)

// TestRealBuilderRun takes a small Task through builder with the real sbx and the real claude,
// against a fake GitHub and a local origin. It spends Claude usage and takes a few minutes, so it
// runs only with COWORKER_TEST_SBX=1 and COWORKER_TEST_CLAUDE=1.
func TestRealBuilderRun(t *testing.T) {
	if os.Getenv("COWORKER_TEST_SBX") != "1" || os.Getenv("COWORKER_TEST_CLAUDE") != "1" {
		t.Skip("set COWORKER_TEST_SBX=1 and COWORKER_TEST_CLAUDE=1 to run claude in a real sandbox")
	}
	e := newEnv(t, map[string]string{"README.md": "A practice project.\n"})
	runRealBuilder(t, e, fmt.Sprintf("impl-run-w%d-", time.Now().UnixNano()%100_000))
}

// TestRealBuilderRunFromTemplate is TestRealBuilderRun with the environment main.go wires in, so
// the project's Dockerfile is built into a template and the Task's sandbox starts from it.
func TestRealBuilderRunFromTemplate(t *testing.T) {
	if os.Getenv("COWORKER_TEST_SBX") != "1" || os.Getenv("COWORKER_TEST_CLAUDE") != "1" {
		t.Skip("set COWORKER_TEST_SBX=1 and COWORKER_TEST_CLAUDE=1 to run claude in a real sandbox")
	}
	dockerfile := "FROM docker/sandbox-templates:claude-code-docker\nUSER root\nRUN echo from-template > /etc/coworker-test-marker\nUSER agent\n"
	e := newEnv(t, map[string]string{
		"README.md":               "A practice project.\n",
		".coworker/Dockerfile":    dockerfile,
		".coworker/allowed-hosts": "example.com\n",
		// Fails, which sends the Task to stuck, unless the sandbox came from the template.
		setupScript: "grep -qx from-template /etc/coworker-test-marker\n",
	})
	owner, repo := "impl-int", fmt.Sprintf("t%d", time.Now().UnixNano()%100_000)
	e.deps.Environment = template.New(e.deps.Git, owner, repo)
	t.Cleanup(func() {
		sum := sha256.Sum256([]byte(dockerfile))
		ref := "docker.io/library/coworker-" + template.Slug(owner, repo, 32) + ":" + hex.EncodeToString(sum[:])[:12]
		if out, err := exec.Command("sbx", "template", "rm", "--force", ref).CombinedOutput(); err != nil {
			t.Errorf("remove template %s: %v %s", ref, err, out)
		}
	})
	runRealBuilder(t, e, fmt.Sprintf("impl-int-w%d-", time.Now().UnixNano()%100_000))
}

// runRealBuilder takes a Task that asks for hello.txt through builder until its PR opens.
func runRealBuilder(t *testing.T, e *env, prefix string) {
	t.Helper()
	sbx := sandbox.CLI{}
	e.deps.Sandboxes = sbx
	e.deps.SandboxPrefix = prefix
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
	e.wantAuthor(branchName(task))
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

// TestRealResolverRun is TestRealBuilderRun for resolver: a PR that conflicts with main on one line.
func TestRealResolverRun(t *testing.T) {
	if os.Getenv("COWORKER_TEST_SBX") != "1" || os.Getenv("COWORKER_TEST_CLAUDE") != "1" {
		t.Skip("set COWORKER_TEST_SBX=1 and COWORKER_TEST_CLAUDE=1 to run claude in a real sandbox")
	}
	e := newEnv(t, map[string]string{"greeting.txt": "hello\n"})
	sbx := sandbox.CLI{}
	e.deps.Sandboxes = sbx
	e.deps.SandboxPrefix = fmt.Sprintf("impl-run-r%d-", time.Now().UnixNano()%100_000)
	e.box = newTaskbox(e.deps, nil)
	task, _, head := e.conflictTask(map[string]string{"greeting.txt": "hello\nfrom the feature\n"}, map[string]string{"greeting.txt": "hello\nfrom main\n"})
	e.gh.AddComment(task, "If greeting.txt conflicts, keep both added lines, main's line first.")
	name := e.resolverBox(task)
	t.Cleanup(func() {
		if err := sbx.Remove(context.Background(), name); err != nil {
			t.Errorf("remove %s: %v", name, err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	for {
		if err := e.resolver().Handle(ctx, task); err != nil {
			t.Fatal(err)
		}
		if gittest.Head(e.origin, branchName(task)) != head || len(e.gh.Issue(task).Comments) > 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("not resolved after 10 minutes. stderr: %s", e.box.tail(name, "stderr.log"))
		case <-time.After(5 * time.Second):
		}
	}

	if comments := e.gh.Issue(task).Comments; len(comments) > 1 {
		t.Fatalf("resolver commented: %q", comments[len(comments)-1].Body)
	}
	if n := len(gittest.Parents(t, e.origin, branchName(task))); n != 2 {
		t.Errorf("branch head has %d parents, want a merge commit", n)
	}
	message := gittest.Run(t, e.origin, "log", "-1", "--format=%B", branchName(task))
	lines := strings.Split(message, "\n")
	if lines[len(lines)-1] != fmt.Sprintf("Task: #%d", task) || slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "#") }) {
		t.Errorf("merge message = %q, want Task: #%d as the last line and no line starting with #", message, task)
	}
	got := gittest.ReadFile(t, e.origin, branchName(task), "greeting.txt")
	if strings.Contains(got, "<<<<<<<") || strings.Contains(got, ">>>>>>>") {
		t.Errorf("greeting.txt still has conflict markers: %q", got)
	}
	t.Logf("greeting.txt after the merge: %q", got)
	e.wantAuthor(branchName(task))
	e.wantStatus(task, label.Review)
}
