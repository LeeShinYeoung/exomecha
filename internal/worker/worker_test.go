package worker

import (
	"context"
	"errors"
	"maps"
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
	sbx    *fakeSandboxes
	envr   *fakeEnvironment
	clock  *clock
	deps   Deps
	runs   *runs
}

// Each tier has a model and an effort no other tier has, so a start's args show which tier it got.
var testModels = map[label.Tier]agent.Model{
	label.Light:  {Name: "light-model", Effort: "low"},
	label.Normal: {Name: "normal-model", Effort: "medium"},
	label.Heavy:  {Name: "heavy-model", Effort: "high"},
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
	e := &env{
		t:      t,
		gh:     s,
		origin: origin,
		sbx:    newFakeSandboxes(t),
		envr:   &fakeEnvironment{},
		clock:  newClock(),
	}
	e.deps = Deps{
		GitHub:        client,
		Git:           git.NewAt(t.TempDir(), origin, "main", "token"),
		Sandboxes:     e.sbx,
		Environment:   e.envr,
		SandboxDir:    t.TempDir(),
		SandboxPrefix: sandbox.Prefix("owner", "repo"),
		ProjectLimit:  2,
		ReadLimit:     2,
		GitName:       "Owner Name",
		GitEmail:      "4242+owner@users.noreply.github.com",
		Models:        maps.Clone(testModels),
	}
	e.runs = newRuns(e.deps, nil)
	e.runs.now = e.clock.now
	return e
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
// since that decides which worker gets the PRD back. A PRD's comment says to remove the label.
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
	// handling runs in every Handle, if set.
	handling func()

	mu      sync.Mutex
	handled []int
}

func (*fakeWorker) Name() string                          { return "test" }
func (w *fakeWorker) Pick(context.Context) ([]int, error) { return w.picks, nil }
func (w *fakeWorker) Handle(_ context.Context, n int) error {
	if w.handling != nil {
		w.handling()
	}
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

// early is how much sooner than interval after the last a round may start.
const early = 10 * time.Millisecond

func TestRoundSkipsAnItemThatIsNotDue(t *testing.T) {
	c := newClock()
	w := &fakeWorker{picks: []int{1, 2}, errs: map[int]error{1: errors.New("sbx down")}}
	r := newRounds(w, c)

	r.run(t.Context())
	wantInts(t, "first round", w.took(), []int{1, 2})
	c.advance(minBackoff / 2)
	r.run(t.Context())
	wantInts(t, "before the wait passed", w.took(), []int{2})
	c.advance(minBackoff/2 - early)
	r.run(t.Context())
	wantInts(t, "after the wait", w.took(), []int{1, 2})
}

// A Handle that takes long does not push the next try back a round.
func TestRoundWaitsFromTheRoundStart(t *testing.T) {
	c := newClock()
	w := &fakeWorker{picks: []int{1}, errs: map[int]error{1: errors.New("sbx down")}, handling: func() { c.advance(10 * time.Second) }}
	r := newRounds(w, c)

	r.run(t.Context())
	c.advance(interval - 10*time.Second - early)
	r.run(t.Context())
	wantInts(t, "tries", w.took(), []int{1, 1})
}

func TestRoundDelaysDouble(t *testing.T) {
	c := newClock()
	w := &fakeWorker{picks: []int{1}, errs: map[int]error{1: errors.New("sbx down")}}
	r := newRounds(w, c)
	r.run(t.Context())
	w.took()

	for _, wait := range []time.Duration{1, 2, 4, 8, 16, 30, 30, 30} {
		// The round before the wait ends, then the next one.
		c.advance(wait*time.Minute - interval)
		r.run(t.Context())
		if got := w.took(); len(got) != 0 {
			t.Fatalf("tried again before %v passed", wait*time.Minute)
		}
		c.advance(interval - early)
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
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
	name := e.builderBox(prd)
	e.sbx.fail["create"] = errors.New("sbx run: daemon not running")
	r := newRounds(e.builder(), e.clock)

	r.run(t.Context())
	e.wantStatus(prd, label.Ready)
	e.wantRemoved(name)
	delete(e.sbx.fail, "create")
	e.clock.advance(minBackoff / 2)
	r.run(t.Context())
	if e.sbx.exists(name) {
		t.Fatal("started again before a minute passed")
	}
	e.clock.advance(minBackoff / 2)
	r.run(t.Context())
	e.wantStatus(prd, label.Working)
	e.wantState(name, running)
}

// A real reviewer whose stuck comment GitHub fails waits a minute in the loop, then comments.
func TestRoundRetriesAReviewerAfterAMinute(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _ := e.reviewPRD()
	e.gh.ClosePull(pr)
	e.gh.Fail(commentsRoute, http.StatusBadGateway)
	r := newRounds(e.reviewer(), e.clock)

	r.run(t.Context())
	e.wantStatus(prd, label.Review)
	e.gh.Fail(commentsRoute, 0)
	e.clock.advance(minBackoff / 2)
	r.run(t.Context())
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Fatal("tried again before a minute passed")
	}
	e.clock.advance(minBackoff / 2)
	r.run(t.Context())
	e.wantStatus(prd, label.Stuck)
}

const (
	labelsRoute   = "PUT /repos/{owner}/{repo}/issues/{n}/labels"
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
	ready := func(e *env) (int, string) {
		prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
		return prd, e.builderBox(prd)
	}
	built := func(e *env) (int, string) {
		prd, name := e.startedPRD()
		e.play(name, commit(e.t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
		return prd, name
	}
	inReview := func(e *env) (int, string) {
		prd, _, _ := e.reviewPRD()
		return prd, e.reviewerBox(prd)
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
		{"builder", "start's create", ready, sbxFault("create"), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Working)
			e.wantState(name, running)
		}},
		{"builder", "claim", ready, githubFault(labelsRoute), builderHandle, func(e *env, n int, name string) {
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
		// The PRD keeps its one status, so the next try still finds it working.
		{"builder", "collect's SetStatus", built, githubFault(labelsRoute), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Review)
			e.wantRemoved(name)
		}},
		{"builder", "restart's SetStatus", func(e *env) (int, string) {
			prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Working))
			return prd, e.builderBox(prd)
		}, githubFault(labelsRoute), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Ready)
		}},
		{"builder", "failed's AddComment", func(e *env) (int, string) {
			prd, name := e.startedPRD()
			e.sbx.stop(name)
			return prd, name
		}, githubFault(commentsRoute), builderHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Stuck)
			e.wantKept(name, "input.md")
		}},
		{"resolver", "start's merge", func(e *env) (int, string) {
			prd, _, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})
			return prd, e.resolverBox(prd)
		}, execFault("git merge"), resolverHandle, func(e *env, n int, name string) {
			e.wantState(name, running)
		}},
		{"resolver", "collect's push", func(e *env) (int, string) {
			prd, name, _ := e.conflicted()
			e.play(name, resolveWith(e.t, "main\nfeature\n"), resolverDone)
			return prd, name
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
			prd, pr, _ := e.reviewPRD()
			e.gh.ClosePull(pr)
			return prd, e.reviewerBox(prd)
		}, githubFault(commentsRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Stuck)
		}},
		{"reviewer", "failing's AddReview", func(e *env) (int, string) {
			prd, _, sha := e.reviewPRD()
			e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure"})
			return prd, e.reviewerBox(prd)
		}, githubFault(reviewsRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Ready)
		}},
		{"reviewer", "start's create", inReview, sbxFault("create"), reviewerHandle, func(e *env, n int, name string) {
			e.wantState(name, running)
		}},
		{"reviewer", "changes' AddReview", func(e *env) (int, string) {
			prd, _, _, name := e.reviewing()
			e.play(name, nil, `{"verdict": "changes", "body": "rename it"}`)
			return prd, name
		}, githubFault(reviewsRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Ready)
			e.wantRemoved(name)
		}},
		{"reviewer", "approve's AddApproval", func(e *env) (int, string) {
			prd, _, name := e.approving()
			return prd, name
		}, githubFault(reviewsRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantApproved(n, e.gh.Pulls()[0].Number)
			e.wantRemoved(name)
		}},
		{"reviewer", "approve's SetStatus", func(e *env) (int, string) {
			prd, _, name := e.approving()
			return prd, name
		}, githubFault(labelsRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantApproved(n, e.gh.Pulls()[0].Number)
			e.wantRemoved(name)
		}},
		{"reviewer", "an approved PRD's close", func(e *env) (int, string) {
			prd, pr := e.approvedPRD()
			e.gh.MergePull(pr)
			return prd, e.reviewerBox(prd)
		}, githubFault(closeRoute), reviewerHandle, func(e *env, n int, name string) {
			e.wantFinished(n)
		}},
		{"resolver", "stale's SetStatus", func(e *env) (int, string) {
			prd, pr := e.approvedPRD()
			e.gh.SetMergeable(pr, new(false))
			return prd, e.resolverBox(prd)
		}, githubFault(labelsRoute), resolverHandle, func(e *env, n int, name string) {
			e.wantStatus(n, label.Review)
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
