package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
	"github.com/LeeShinYeoung/exomecha/internal/template"
)

// TestRealBuilderRun takes a small Task through builder with the real sbx and the real claude,
// against a fake GitHub and a local origin. It spends Claude usage and takes a few minutes, so it
// runs only with EXOMECHA_TEST_SBX=1 and EXOMECHA_TEST_CLAUDE=1.
func TestRealBuilderRun(t *testing.T) {
	if os.Getenv("EXOMECHA_TEST_SBX") != "1" || os.Getenv("EXOMECHA_TEST_CLAUDE") != "1" {
		t.Skip("set EXOMECHA_TEST_SBX=1 and EXOMECHA_TEST_CLAUDE=1 to run claude in a real sandbox")
	}
	e := newEnv(t, map[string]string{"README.md": "A practice project.\n"})
	runRealBuilder(t, e, fmt.Sprintf("impl-run-w%d-", time.Now().UnixNano()%100_000))
}

// TestRealBuilderRunFromTemplate is TestRealBuilderRun with the environment main.go wires in, so
// the project's Dockerfile is built into a template and the Task's sandbox starts from it.
func TestRealBuilderRunFromTemplate(t *testing.T) {
	if os.Getenv("EXOMECHA_TEST_SBX") != "1" || os.Getenv("EXOMECHA_TEST_CLAUDE") != "1" {
		t.Skip("set EXOMECHA_TEST_SBX=1 and EXOMECHA_TEST_CLAUDE=1 to run claude in a real sandbox")
	}
	dockerfile := "FROM docker/sandbox-templates:claude-code-docker\nUSER root\nRUN echo from-template > /etc/exomecha-test-marker\nUSER agent\n"
	e := newEnv(t, map[string]string{
		"README.md":               "A practice project.\n",
		".exomecha/Dockerfile":    dockerfile,
		".exomecha/allowed-hosts": "example.com\n",
		// Fails, which sends the Task to stuck, unless the sandbox came from the template.
		setupScript: "grep -qx from-template /etc/exomecha-test-marker\n",
	})
	owner, repo := "impl-int", fmt.Sprintf("t%d", time.Now().UnixNano()%100_000)
	e.deps.Environment = template.New(e.deps.Git, sandbox.CLI{}, owner, repo)
	t.Cleanup(func() {
		sum := sha256.Sum256([]byte(dockerfile))
		ref := "docker.io/library/exomecha-" + sandbox.Slug(owner, repo) + ":" + hex.EncodeToString(sum[:])[:12]
		if out, err := exec.Command("sbx", "template", "rm", "--force", ref).CombinedOutput(); err != nil {
			t.Errorf("remove template %s: %v %s", ref, err, out)
		}
	})
	runRealBuilder(t, e, fmt.Sprintf("impl-int-w%d-", time.Now().UnixNano()%100_000))
}

// useOpus runs every tier on opus. The sandbox's own default is sonnet, so a run on opus shows that
// --model reached claude. Not haiku: claude does not run haiku in auto permission mode.
func (e *env) useOpus() {
	for t := range e.deps.Models {
		e.deps.Models[t] = agent.Model{Name: "opus", Effort: "low"}
	}
}

// runModel returns the model in claude's init line, or "" before claude wrote one. It must be read
// before the Handle that collects the run, which removes the run's folder.
func (e *env) runModel(name string) string {
	f, err := os.Open(filepath.Join(e.runs.ctl(name), "stream.jsonl"))
	if err != nil {
		return ""
	}
	defer f.Close()
	s, err := agent.ReadStream(f)
	if err != nil {
		return ""
	}
	return s.Model
}

func wantOpus(t *testing.T, model string) {
	t.Helper()
	if !strings.Contains(model, "opus") {
		t.Errorf("claude ran on %q, want opus", model)
	}
}

// runRealBuilder takes a Task that asks for hello.txt through builder until its PR opens.
func runRealBuilder(t *testing.T, e *env, prefix string) {
	t.Helper()
	sbx := sandbox.CLI{}
	e.deps.Sandboxes = sbx
	e.deps.SandboxPrefix = prefix
	e.useOpus()
	e.runs = newRuns(e.deps, nil)
	task := e.gh.AddIssue("Add hello.txt",
		"## To do\n\nCreate a file `hello.txt` at the repository root whose only line is `hello`. Commit it with the message `feat: add hello`.\n",
		label.PRD, string(label.Queued))
	name := e.builderBox(task)
	t.Cleanup(func() {
		if err := sbx.Remove(context.Background(), name); err != nil {
			t.Errorf("remove %s: %v", name, err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	var model string
	for {
		if m := e.runModel(name); m != "" {
			model = m
		}
		if err := e.builder().Handle(ctx, task); err != nil {
			t.Fatal(err)
		}
		status := e.statuses(task)
		if len(status) != 1 || status[0] != string(label.Working) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("still working after 10 minutes. stderr: %s", e.runs.tail(name, "stderr.log"))
		case <-time.After(5 * time.Second):
		}
	}

	if got := e.statuses(task); len(got) != 1 || got[0] != string(label.Review) {
		t.Fatalf("status = %q, last comment %q", got, e.gh.Issue(task).Comments)
	}
	wantOpus(t, model)
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
	if os.Getenv("EXOMECHA_TEST_SBX") != "1" || os.Getenv("EXOMECHA_TEST_CLAUDE") != "1" {
		t.Skip("set EXOMECHA_TEST_SBX=1 and EXOMECHA_TEST_CLAUDE=1 to run claude in a real sandbox")
	}
	e := newEnv(t, map[string]string{"greeting.txt": "hello\n"})
	sbx := sandbox.CLI{}
	e.deps.Sandboxes = sbx
	e.deps.SandboxPrefix = fmt.Sprintf("impl-run-r%d-", time.Now().UnixNano()%100_000)
	e.useOpus()
	e.runs = newRuns(e.deps, nil)
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
	var model string
	for {
		if m := e.runModel(name); m != "" {
			model = m
		}
		if err := e.resolver().Handle(ctx, task); err != nil {
			t.Fatal(err)
		}
		if gittest.Head(e.origin, branchName(task)) != head || len(e.gh.Issue(task).Comments) > 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("not resolved after 10 minutes. stderr: %s", e.runs.tail(name, "stderr.log"))
		case <-time.After(5 * time.Second):
		}
	}

	if comments := e.gh.Issue(task).Comments; len(comments) > 1 {
		t.Fatalf("resolver commented: %q", comments[len(comments)-1].Body)
	}
	wantOpus(t, model)
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

// TestRealReviewerRun is TestRealBuilderRun for reviewer: a PR that adds hello.txt, reviewed in a
// sandbox from sbx's default template and merged with the head the run reviewed.
func TestRealReviewerRun(t *testing.T) {
	if os.Getenv("EXOMECHA_TEST_SBX") != "1" || os.Getenv("EXOMECHA_TEST_CLAUDE") != "1" {
		t.Skip("set EXOMECHA_TEST_SBX=1 and EXOMECHA_TEST_CLAUDE=1 to run claude in a real sandbox")
	}
	e := newEnv(t, map[string]string{"README.md": "A practice project.\n"})
	sbx := sandbox.CLI{}
	e.deps.Sandboxes = sbx
	e.deps.SandboxPrefix = fmt.Sprintf("impl-run-v%d-", time.Now().UnixNano()%100_000)
	e.useOpus()
	e.runs = newRuns(e.deps, nil)
	task := e.gh.AddIssue("Add hello.txt",
		"## To do\n\nCreate a file `hello.txt` at the repository root whose only line is `hello`.\n\n"+
			"## Acceptance criteria\n\n- `hello.txt` exists at the root and holds exactly the line `hello`.\n",
		label.PRD, string(label.Review))
	head := gittest.Commit(t, e.origin, branchName(task), "main", map[string]string{"hello.txt": "hello\n"})
	pr := e.gh.AddPull(branchName(task), "main")
	e.gh.SetMergeable(pr, new(true))
	name := e.reviewerBox(task)
	t.Cleanup(func() {
		if err := sbx.Remove(context.Background(), name); err != nil {
			t.Errorf("remove %s: %v", name, err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	var model string
	for {
		if m := e.runModel(name); m != "" {
			model = m
		}
		if err := e.reviewer().Handle(ctx, task); err != nil {
			t.Fatal(err)
		}
		status := e.statuses(task)
		if e.gh.Pull(pr).Merged || len(status) != 1 || status[0] != string(label.Review) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("not reviewed after 10 minutes. stderr: %s", e.runs.tail(name, "stderr.log"))
		case <-time.After(5 * time.Second):
		}
	}

	p := e.gh.Pull(pr)
	if !p.Merged {
		var reviews []string
		for _, r := range p.Reviews {
			reviews = append(reviews, r.Body)
		}
		t.Fatalf("PR not merged: status %q, reviews %q, comments %q", e.statuses(task), reviews, e.gh.Issue(task).Comments)
	}
	wantOpus(t, model)
	if p.MergedSHA != head {
		t.Errorf("merged %s, want the reviewed head %s", p.MergedSHA, head)
	}
	if i := e.gh.Issue(task); i.Open || i.StateReason != "completed" {
		t.Errorf("Task open=%v reason=%q, want closed as completed", i.Open, i.StateReason)
	}
	e.wantStatus(task, label.None)
	if _, err := os.Lstat(e.runs.dir(name)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("run directory left after the merge: %v", err)
	}
	if boxes, err := sbx.List(t.Context()); err == nil {
		for _, b := range boxes {
			if b.Name == name {
				t.Errorf("sandbox %s left after the merge", name)
			}
		}
	}
}
