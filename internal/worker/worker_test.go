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
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
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
	runs   *runs
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
		SandboxPrefix: sandbox.Prefix("owner", "repo"),
		ProjectLimit:  2,
		ReadLimit:     2,
		GitName:       "Owner Name",
		GitEmail:      "4242+owner@users.noreply.github.com",
	}
	e.runs = newRuns(e.deps, nil)
	e.runs.now = e.clock.now
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

func TestTrimClosingTags(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// The end of the reason on #130.
		{"then the test can be re-run.</stuck_reason>\n</invoke>", "then the test can be re-run."},
		{"  need a decision on X \n", "need a decision on X"},
		{"Close the `</div>` in index.html, or drop the page.", "Close the `</div>` in index.html, or drop the page."},
		{"Which wins? </a> </b>\n</c>", "Which wins?"},
		{"Line break with <br>", "Line break with <br>"},
		{"An empty tag </>", "An empty tag </>"},
	} {
		if got := trimClosingTags(c.in); got != c.want {
			t.Errorf("trimClosingTags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// wantStuckComment also checks which label the comment tells the person to set,
// since that decides which worker gets the Task back.
func wantStuckComment(t *testing.T, comment, reason string, back label.Status) {
	t.Helper()
	if !strings.HasPrefix(comment, github.Marker) {
		t.Errorf("stuck comment does not start with the exomecha marker:\n%s", comment)
	}
	wantContains(t, "stuck comment", comment, reason, "relabel it `"+string(back)+"`")
}

// fakeWorker picks fixed numbers, and its Handle returns the error the test set for the number.
type fakeWorker struct {
	picks []int
	errs  map[int]error

	mu      sync.Mutex
	handled []int
}

func (*fakeWorker) Name() string                          { return "test" }
func (w *fakeWorker) Pick(context.Context) ([]int, error) { return w.picks, nil }
func (w *fakeWorker) Handle(_ context.Context, n int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handled = append(w.handled, n)
	return w.errs[n]
}

// took returns the numbers handled since it was last called, sorted.
func (w *fakeWorker) took() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.handled
	w.handled = nil
	slices.Sort(out)
	return out
}

// rounds runs one worker's rounds the way loop does, on the test's clock.
type rounds struct {
	w      Worker
	locks  *locks
	delays *delays
}

func newRounds(w Worker, c *clock) *rounds {
	return &rounds{w: w, locks: &locks{held: map[int]bool{}}, delays: &delays{now: c.now, items: map[int]delay{}}}
}

// run returns once every Handle the round started has returned.
func (r *rounds) run(ctx context.Context, gate *Gate) {
	var wg sync.WaitGroup
	round(ctx, r.w, nil, r.locks, r.delays, &wg, gate)
	wg.Wait()
}

func TestRoundSkipsAnItemThatIsNotDue(t *testing.T) {
	c := newClock()
	w := &fakeWorker{picks: []int{1, 2}, errs: map[int]error{1: errors.New("sbx down")}}
	r := newRounds(w, c)

	r.run(t.Context(), nil)
	wantInts(t, "first round", w.took(), []int{1, 2})
	c.advance(minBackoff - time.Second)
	r.run(t.Context(), nil)
	wantInts(t, "before the wait passed", w.took(), []int{2})
	c.advance(time.Second)
	r.run(t.Context(), nil)
	wantInts(t, "after the wait", w.took(), []int{1, 2})
}

func TestRoundDelaysDouble(t *testing.T) {
	c := newClock()
	w := &fakeWorker{picks: []int{1}, errs: map[int]error{1: errors.New("sbx down")}}
	r := newRounds(w, c)
	r.run(t.Context(), nil)
	w.took()

	for _, wait := range []time.Duration{1, 2, 4, 8, 16, 30, 30, 30} {
		c.advance(wait*time.Minute - time.Second)
		r.run(t.Context(), nil)
		if got := w.took(); len(got) != 0 {
			t.Fatalf("tried again before %v passed", wait*time.Minute)
		}
		c.advance(time.Second)
		r.run(t.Context(), nil)
		if got := w.took(); len(got) != 1 {
			t.Fatalf("not tried again after %v", wait*time.Minute)
		}
	}
}

func TestRoundForgetsOnSuccess(t *testing.T) {
	c := newClock()
	fail := errors.New("sbx down")
	w := &fakeWorker{picks: []int{1}, errs: map[int]error{1: fail}}
	r := newRounds(w, c)
	r.run(t.Context(), nil)
	c.advance(minBackoff)
	r.run(t.Context(), nil)
	w.errs[1] = nil
	c.advance(2 * minBackoff)
	r.run(t.Context(), nil)
	wantInts(t, "tries", w.took(), []int{1, 1, 1})

	// Two errors before the success. A new error starts the waits over at one minute.
	w.errs[1] = fail
	r.run(t.Context(), nil)
	c.advance(minBackoff)
	r.run(t.Context(), nil)
	wantInts(t, "tries after the success", w.took(), []int{1, 1})
}

func TestRoundForgetsItemsNoLongerPicked(t *testing.T) {
	w := &fakeWorker{picks: []int{1}, errs: map[int]error{1: errors.New("sbx down")}}
	r := newRounds(w, newClock())
	r.run(t.Context(), nil)
	w.picks = nil
	r.run(t.Context(), nil)
	w.picks = []int{1}
	r.run(t.Context(), nil)
	wantInts(t, "tries", w.took(), []int{1, 1})
}

func TestRoundDoesNotStrikeOnShutdown(t *testing.T) {
	w := &fakeWorker{picks: []int{1}, errs: map[int]error{1: context.Canceled}}
	r := newRounds(w, newClock())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.run(ctx, nil)
	w.errs[1] = nil
	r.run(t.Context(), nil)
	wantInts(t, "tries", w.took(), []int{1, 1})
}

// Another worker holds the issue. That is no error, so the next round tries it at once.
func TestRoundDoesNotStrikeALockedItem(t *testing.T) {
	w := &fakeWorker{picks: []int{1}}
	r := newRounds(w, newClock())
	r.locks.lock(1)
	r.run(t.Context(), nil)
	wantInts(t, "tries while locked", w.took(), nil)
	r.locks.unlock(1)
	r.run(t.Context(), nil)
	wantInts(t, "tries once unlocked", w.took(), []int{1})
}

// An issue that fails in one worker is not held back in another.
func TestRoundDelaysArePerWorker(t *testing.T) {
	c := newClock()
	failing := &fakeWorker{picks: []int{1}, errs: map[int]error{1: errors.New("sbx down")}}
	other := &fakeWorker{picks: []int{1}}
	f, o := newRounds(failing, c), newRounds(other, c)
	o.locks = f.locks

	f.run(t.Context(), nil)
	o.run(t.Context(), nil)
	f.run(t.Context(), nil)
	o.run(t.Context(), nil)
	wantInts(t, "failing worker's tries", failing.took(), []int{1})
	wantInts(t, "other worker's tries", other.took(), []int{1, 1})
}

// A real builder start that sbx fails waits a minute in the loop, then starts.
func TestRoundRetriesABuilderStartAfterAMinute(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
	name := e.builderBox(task)
	e.sbx.fail["create"] = errors.New("sbx run: daemon not running")
	r := newRounds(e.builder(), e.clock)

	r.run(t.Context(), nil)
	e.wantStatus(task, label.Queued)
	e.wantRemoved(name)
	delete(e.sbx.fail, "create")
	e.clock.advance(minBackoff - time.Second)
	r.run(t.Context(), nil)
	if e.sbx.exists(name) {
		t.Fatal("started again before a minute passed")
	}
	e.clock.advance(time.Second)
	r.run(t.Context(), nil)
	e.wantStatus(task, label.Working)
	e.wantState(name, running)
}
