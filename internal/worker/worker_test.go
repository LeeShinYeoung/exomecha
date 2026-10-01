package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/git"
	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/github/githubtest"
	"github.com/LeeShinYeoung/coworker/internal/label"
)

type env struct {
	t      *testing.T
	gh     *githubtest.Server
	origin string
	claude *fakeClaude
	sbx    *fakeSandboxes
	envr   *fakeEnvironment
	clock  *clock
	deps   Deps
	box    *taskbox
}

func newEnv(t *testing.T, files map[string]string) *env {
	t.Helper()
	gittest.Setup(t)
	origin := gittest.NewOrigin(t, files)
	s := githubtest.New(t)
	s.HeadSHA = func(branch string) string { return gittest.Head(origin, branch) }
	client, err := github.NewAt(s.URL, s.Owner, s.Repo, "token")
	if err != nil {
		t.Fatal(err)
	}
	claude := &fakeClaude{t: t}
	e := &env{
		t:      t,
		gh:     s,
		origin: origin,
		claude: claude,
		sbx:    newFakeSandboxes(t),
		envr:   &fakeEnvironment{},
		clock:  newClock(),
	}
	e.deps = Deps{
		GitHub:        client,
		Git:           git.NewAt(t.TempDir(), origin, "main", "token"),
		Agent:         claude,
		Sandboxes:     e.sbx,
		Environment:   e.envr,
		SandboxDir:    t.TempDir(),
		SandboxPrefix: "coworker-task-owner-repo-",
		SandboxLimit:  2,
	}
	e.box = newTaskbox(e.deps, nil)
	e.box.now = e.clock.now
	return e
}

// fakeClaude answers with whatever the test sets in reply, and records every request.
// With no reply set, a call fails the test.
type fakeClaude struct {
	t     *testing.T
	mu    sync.Mutex
	reply func(req agent.Request) (string, error)
	calls []agent.Request
}

func (f *fakeClaude) Run(_ context.Context, req agent.Request) (agent.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	reply := f.reply
	f.mu.Unlock()
	if reply == nil {
		f.t.Errorf("unexpected claude call with input:\n%s", req.Input)
		return agent.Result{}, errors.New("unexpected claude call")
	}
	if _, err := os.Stat(req.Dir); err != nil {
		f.t.Errorf("claude runs in a missing worktree: %v", err)
	}
	out, err := reply(req)
	if err != nil {
		return agent.Result{}, err
	}
	return agent.Result{Structured: json.RawMessage(out)}, nil
}

func (f *fakeClaude) answer(out string) {
	f.reply = func(agent.Request) (string, error) { return out, nil }
}

func (f *fakeClaude) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeClaude) lastInput() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		f.t.Fatal("claude was never called")
	}
	return f.calls[len(f.calls)-1].Input
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *env) statuses(n int) []string {
	var out []string
	for _, l := range e.gh.Issue(n).Labels {
		if label.IsStatus(l) {
			out = append(out, l)
		}
	}
	return out
}

// wantStatus checks that the issue carries exactly the status want, or none for label.None.
func (e *env) wantStatus(n int, want label.Status) {
	e.t.Helper()
	got := e.statuses(n)
	if want == label.None && len(got) == 0 || len(got) == 1 && got[0] == string(want) {
		return
	}
	e.t.Errorf("issue #%d status labels = %q, want %q", n, got, want)
}

func (e *env) lastComment(n int) string {
	e.t.Helper()
	comments := e.gh.Issue(n).Comments
	if len(comments) == 0 {
		e.t.Fatalf("issue #%d has no comments", n)
	}
	return comments[len(comments)-1].Body
}

func wantContains(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s does not contain %q:\n%s", what, sub, s)
		}
	}
}

func wantNotContains(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Errorf("%s contains %q:\n%s", what, sub, s)
		}
	}
}

func wantInts(t *testing.T, what string, got, want []int) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// wantStuckComment also checks which label the comment tells the person to set,
// since that decides which worker gets the Task back.
func wantStuckComment(t *testing.T, comment, reason string, back label.Status) {
	t.Helper()
	if !strings.HasPrefix(comment, github.Marker) {
		t.Errorf("stuck comment does not start with the coworker marker:\n%s", comment)
	}
	wantContains(t, "stuck comment", comment, reason, "relabel it `"+string(back)+"`")
}
