package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
// since that decides which worker gets the Task back. A PRD's comment says to remove the label.
func wantStuckComment(t *testing.T, comment, reason string, back label.Status) {
	t.Helper()
	if !strings.HasPrefix(comment, github.Marker) {
		t.Errorf("stuck comment does not start with the exomecha marker:\n%s", comment)
	}
	again := "relabel it `" + string(back) + "`"
	if back == label.None {
		again = "remove the `" + string(label.Stuck) + "` label"
	}
	wantContains(t, "stuck comment", comment, reason, again)
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
func (r *rounds) run(ctx context.Context) {
	var wg sync.WaitGroup
	round(ctx, r.w, r.locks, r.delays, &wg)
	wg.Wait()
}

func TestRoundSkipsAnItemThatIsNotDue(t *testing.T) {
	c := newClock()
	w := &fakeWorker{picks: []int{1, 2}, errs: map[int]error{1: errors.New("sbx down")}}
	r := newRounds(w, c)

	r.run(t.Context())
	wantInts(t, "first round", w.took(), []int{1, 2})
	c.advance(minBackoff - time.Second)
	r.run(t.Context())
	wantInts(t, "before the wait passed", w.took(), []int{2})
	c.advance(time.Second)
	r.run(t.Context())
	wantInts(t, "after the wait", w.took(), []int{1, 2})
}

func TestRoundDelaysDouble(t *testing.T) {
	c := newClock()
	w := &fakeWorker{picks: []int{1}, errs: map[int]error{1: errors.New("sbx down")}}
	r := newRounds(w, c)
	r.run(t.Context())
	w.took()

	for _, wait := range []time.Duration{1, 2, 4, 8, 16, 30, 30, 30} {
		c.advance(wait*time.Minute - time.Second)
		r.run(t.Context())
		if got := w.took(); len(got) != 0 {
			t.Fatalf("tried again before %v passed", wait*time.Minute)
		}
		c.advance(time.Second)
		r.run(t.Context())
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
	r.run(t.Context())
	c.advance(minBackoff)
	r.run(t.Context())
	w.errs[1] = nil
	c.advance(2 * minBackoff)
	r.run(t.Context())
	wantInts(t, "tries", w.took(), []int{1, 1, 1})

	// Two errors before the success. A new error starts the waits over at one minute.
	w.errs[1] = fail
	r.run(t.Context())
	c.advance(minBackoff)
	r.run(t.Context())
	wantInts(t, "tries after the success", w.took(), []int{1, 1})
}

func TestRoundForgetsItemsNoLongerPicked(t *testing.T) {
	w := &fakeWorker{picks: []int{1}, errs: map[int]error{1: errors.New("sbx down")}}
	r := newRounds(w, newClock())
	r.run(t.Context())
	w.picks = nil
	r.run(t.Context())
	w.picks = []int{1}
	r.run(t.Context())
	wantInts(t, "tries", w.took(), []int{1, 1})
}

func TestRoundDoesNotStrikeOnShutdown(t *testing.T) {
	w := &fakeWorker{picks: []int{1}, errs: map[int]error{1: context.Canceled}}
	r := newRounds(w, newClock())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.run(ctx)
	w.errs[1] = nil
	r.run(t.Context())
	wantInts(t, "tries", w.took(), []int{1, 1})
}

// Another worker holds the issue. That is no error, so the next round tries it at once.
func TestRoundDoesNotStrikeALockedItem(t *testing.T) {
	w := &fakeWorker{picks: []int{1}}
	r := newRounds(w, newClock())
	r.locks.lock(1)
	r.run(t.Context())
	wantInts(t, "tries while locked", w.took(), nil)
	r.locks.unlock(1)
	r.run(t.Context())
	wantInts(t, "tries once unlocked", w.took(), []int{1})
}

// An issue that fails in one worker is not held back in another.
func TestRoundDelaysArePerWorker(t *testing.T) {
	c := newClock()
	failing := &fakeWorker{picks: []int{1}, errs: map[int]error{1: errors.New("sbx down")}}
	other := &fakeWorker{picks: []int{1}}
	f, o := newRounds(failing, c), newRounds(other, c)
	o.locks = f.locks

	f.run(t.Context())
	o.run(t.Context())
	f.run(t.Context())
	o.run(t.Context())
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

	r.run(t.Context())
	e.wantStatus(task, label.Queued)
	e.wantRemoved(name)
	delete(e.sbx.fail, "create")
	e.clock.advance(minBackoff - time.Second)
	r.run(t.Context())
	if e.sbx.exists(name) {
		t.Fatal("started again before a minute passed")
	}
	e.clock.advance(time.Second)
	r.run(t.Context())
	e.wantStatus(task, label.Working)
	e.wantState(name, running)
}

// A real reviewer whose stuck comment GitHub fails waits a minute in the loop, then comments.
func TestRoundRetriesAReviewerAfterAMinute(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	task, pr, _ := e.reviewTask()
	e.gh.ClosePull(pr)
	e.gh.Fail(commentsRoute, http.StatusBadGateway)
	r := newRounds(e.reviewer(), e.clock)

	r.run(t.Context())
	e.wantStatus(task, label.Review)
	e.gh.Fail(commentsRoute, 0)
	e.clock.advance(minBackoff - time.Second)
	r.run(t.Context())
	if n := len(e.gh.Issue(task).Comments); n != 0 {
		t.Fatal("tried again before a minute passed")
	}
	e.clock.advance(time.Second)
	r.run(t.Context())
	e.wantStatus(task, label.Stuck)
}

const (
	issuesRoute   = "POST /repos/{owner}/{repo}/issues"
	labelsRoute   = "POST /repos/{owner}/{repo}/issues/{n}/labels"
	blockedRoute  = "POST /repos/{owner}/{repo}/issues/{n}/dependencies/blocked_by"
	commentsRoute = "POST /repos/{owner}/{repo}/issues/{n}/comments"
	pullsRoute    = "POST /repos/{owner}/{repo}/pulls"
	pullRoute     = "GET /repos/{owner}/{repo}/pulls/{n}"
	reviewsRoute  = "POST /repos/{owner}/{repo}/pulls/{n}/reviews"
)

// refusePushes makes every push to origin fail, as GitHub refusing it would, until it is called
// with false.
func (e *env) refusePushes(refuse bool) {
	e.t.Helper()
	if refuse {
		gittest.Run(e.t, "", "config", "--global", "url./nowhere.pushInsteadOf", e.origin)
		return
	}
	gittest.Run(e.t, "", "config", "--global", "--unset", "url./nowhere.pushInsteadOf")
}

// fault makes one of exomecha's own steps fail until heal.
type fault struct{ fail, heal func(e *env) }

func githubFault(route string) fault {
	return fault{
		func(e *env) { e.gh.Fail(route, http.StatusBadGateway) },
		func(e *env) { e.gh.Fail(route, 0) },
	}
}

func sbxFault(verb string) fault {
	return fault{
		func(e *env) { e.sbx.fail[verb] = errors.New("sbx: daemon not running") },
		func(e *env) { delete(e.sbx.fail, verb) },
	}
}

func execFault(command string) fault {
	return fault{
		func(e *env) { e.sbx.failExec = command },
		func(e *env) { e.sbx.failExec = "" },
	}
}

var pushFault = fault{
	func(e *env) { e.refusePushes(true) },
	func(e *env) { e.refusePushes(false) },
}

// When one of exomecha's own steps fails, Handle returns the error and leaves the issue's labels,
// its comments and its run as they were, so the loop's next try takes the same step again.
func TestEveryHandleErrorReachesTheLoop(t *testing.T) {
	builderHandle := func(e *env, n int) error { return e.builder().Handle(e.t.Context(), n) }
	resolverHandle := func(e *env, n int) error { return e.resolver().Handle(e.t.Context(), n) }
	reviewerHandle := func(e *env, n int) error { return e.reviewer().Handle(e.t.Context(), n) }
	splitterHandle := func(e *env, n int) error { return e.splitter().Handle(e.t.Context(), n) }
	sequencerHandle := func(e *env, n int) error { return e.sequencer().Handle(e.t.Context(), n) }
	queued := func(e *env) (int, string) {
		task := e.gh.AddIssue("Task", "", label.Task, string(label.Queued))
		return task, e.builderBox(task)
	}
	built := func(e *env) (int, string) {
		task, name := e.startedTask()
		e.play(name, commit(e.t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
		return task, name
	}
	inReview := func(e *env) (int, string) {
		task, _, _ := e.reviewTask()
		return task, e.reviewerBox(task)
	}
	for _, c := range []struct {
		worker, step string
		// arrange makes an issue whose next Handle takes the step, and returns it with its run's name.
		arrange func(e *env) (int, string)
		fault   fault
		handle  func(e *env, n int) error
		// done checks what the step did once it went through.
		done func(e *env, n int, name string)
	}{
		{"builder", "start's create", queued, sbxFault("create"), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Working)
			e.wantState(name, running)
		}},
		{"builder", "claim", queued, githubFault(labelsRoute), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Working)
			e.wantState(name, running)
		}},
		{"builder", "collect's fetch", built, execFault("rev-parse -q --verify HEAD"), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Review)
		}},
		{"builder", "collect's push", built, pushFault, builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Review)
			if got := gittest.ReadFile(e.t, e.origin, branchName(n), "x.txt"); got != "x" {
				e.t.Errorf("x.txt on the branch = %q", got)
			}
		}},
		{"builder", "CreatePR", built, githubFault(pullsRoute), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Review)
			if got := len(e.gh.Pulls()); got != 1 {
				e.t.Errorf("%d PRs, want one", got)
			}
		}},
		{"builder", "restart's SetStatus", func(e *env) (int, string) {
			task := e.gh.AddIssue("Task", "", label.Task, string(label.Working))
			return task, e.builderBox(task)
		}, githubFault(labelsRoute), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Queued)
		}},
		{"builder", "failed's AddComment", func(e *env) (int, string) {
			task, name := e.startedTask()
			e.sbx.stop(name)
			return task, name
		}, githubFault(commentsRoute), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Stuck)
			e.wantKept(name, "input.md")
		}},
		{"resolver", "start's merge", func(e *env) (int, string) {
			task, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
			return task, e.resolverBox(task)
		}, execFault("git merge"), resolverHandle, func(e *env, n int, name string) {
			e.wantState(name, running)
		}},
		{"resolver", "collect's push", func(e *env) (int, string) {
			task, name, _ := e.conflicted()
			e.play(name, resolveWith(e.t, "main\nfeature\n"), resolverDone)
			return task, name
		}, pushFault, resolverHandle, func(e *env, n int, name string) {
			e.wantRemoved(name)
			if got := len(gittest.Parents(e.t, e.origin, branchName(n))); got != 2 {
				e.t.Errorf("branch head has %d parents, want the merge", got)
			}
		}},
		{"reviewer", "look's PR read", inReview, githubFault(pullRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantState(name, running)
		}},
		{"reviewer", "closed's AddComment", func(e *env) (int, string) {
			task, pr, _ := e.reviewTask()
			e.gh.ClosePull(pr)
			return task, e.reviewerBox(task)
		}, githubFault(commentsRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Stuck)
		}},
		{"reviewer", "failing's AddReview", func(e *env) (int, string) {
			task, _, sha := e.reviewTask()
			e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure"})
			return task, e.reviewerBox(task)
		}, githubFault(reviewsRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Queued)
		}},
		{"reviewer", "start's create", inReview, sbxFault("create"), reviewerHandle, func(e *env, n int, name string) {
			e.wantState(name, running)
		}},
		{"reviewer", "changes' AddReview", func(e *env) (int, string) {
			task, _, _, name := e.reviewing()
			e.play(name, nil, `{"verdict": "changes", "body": "rename it"}`)
			return task, name
		}, githubFault(reviewsRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Queued)
			e.wantRemoved(name)
		}},
		{"reviewer", "approve's Merge", func(e *env) (int, string) {
			task, _, name := e.approved()
			return task, name
		}, githubFault(mergeRoute), reviewerHandle, func(e *env, n int, name string) {
			if !e.gh.Pulls()[0].Merged {
				e.t.Error("PR not merged")
			}
			e.wantRemoved(name)
		}},
		{"splitter", "start's create", func(e *env) (int, string) {
			prd := e.gh.AddIssue("PRD", "the plan", label.PRD)
			return prd, e.splitterBox(prd)
		}, sbxFault("create"), splitterHandle, func(e *env, n int, name string) {
			e.wantState(name, running)
		}},
		// A Task left from an earlier try is closed on every try, the one that works too.
		{"splitter", "collect's CreateIssue", func(e *env) (int, string) {
			prd, name := e.splitting()
			e.gh.AddSubIssue(prd, e.gh.AddIssue("left over", "", label.Task, string(label.Pending)))
			e.play(name, nil, `{"tasks": [{"title": "A", "body": "a", "blocked_by": []}, {"title": "B", "body": "b", "blocked_by": [1]}]}`)
			return prd, name
		}, githubFault(issuesRoute), splitterHandle, func(e *env, n int, name string) {
			e.wantRemoved(name)
			subs := e.gh.Issue(n).SubIssues
			if len(subs) != 3 || e.gh.Issue(subs[0]).Open {
				e.t.Fatalf("PRD sub-issues = %v, want the left-over Task closed and two new ones", subs)
			}
			for _, task := range subs[1:] {
				e.wantStatus(task, label.Pending)
			}
			wantInts(e.t, "B blocked by", e.gh.Issue(subs[2]).BlockedBy, []int{subs[1]})
			if !e.gh.Issue(n).HasLabel(label.Tasked) {
				e.t.Errorf("PRD has no %s label", label.Tasked)
			}
		}},
		{"sequencer", "SetStatus without candidates", func(e *env) (int, string) {
			task := e.gh.AddIssue("Task", "", label.Task, string(label.Pending))
			return task, e.sequencerBox(task)
		}, githubFault(labelsRoute), sequencerHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Queued)
			e.wantRemoved(name)
		}},
		{"sequencer", "start's create", func(e *env) (int, string) {
			e.gh.AddIssue("lower", "", label.Task, string(label.Queued))
			task := e.gh.AddIssue("Task", "", label.Task, string(label.Pending))
			return task, e.sequencerBox(task)
		}, sbxFault("create"), sequencerHandle, func(e *env, n int, name string) {
			e.wantState(name, running)
		}},
		{"sequencer", "collect's AddBlockedBy", func(e *env) (int, string) {
			task, lower, name := e.sequencing()
			e.play(name, nil, fmt.Sprintf(`{"blocked_by": [%d]}`, lower))
			return task, name
		}, githubFault(blockedRoute), sequencerHandle, func(e *env, n int, name string) {
			wantInts(e.t, "blocked by", e.gh.Issue(n).BlockedBy, []int{n - 1}) // sequencing's lower Task
			e.wantStatus(n, label.Queued)
			e.wantRemoved(name)
		}},
	} {
		t.Run(c.worker+"/"+c.step, func(t *testing.T) {
			e := newEnv(t, map[string]string{"app.txt": "v1\n"})
			n, name := c.arrange(e)
			statuses, comments, run := e.statuses(n), len(e.gh.Issue(n).Comments), e.state(name)

			c.fault.fail(e)
			for try := range 2 {
				if err := c.handle(e, n); err == nil {
					t.Fatalf("try %d: Handle returned nil with the step failing", try+1)
				}
				if got := e.statuses(n); !slices.Equal(got, statuses) {
					t.Errorf("try %d: status labels = %q, want %q", try+1, got, statuses)
				}
				if got := len(e.gh.Issue(n).Comments); got != comments {
					t.Errorf("try %d: %d new comments", try+1, got-comments)
				}
				if got := e.state(name); got != run {
					t.Errorf("try %d: run %s, want it left %s", try+1, got, run)
				}
			}
			c.fault.heal(e)
			if err := c.handle(e, n); err != nil {
				t.Fatal(err)
			}
			c.done(e, n, name)
		})
	}
}
