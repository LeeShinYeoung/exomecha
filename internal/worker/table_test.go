package worker

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

// tableWorker is what TestTables needs to take one worker through every cell of its table.
type tableWorker struct {
	role  role
	table table
	// want is the table as designed, written out again so that a change to the worker's table
	// has to change it here too.
	want table
	// start makes an issue in the ready place and starts its run.
	start func(e *env) int
	// put moves a started issue from ready to p, for every place but away.
	put func(e *env, n int, p place)
	// away are the ways the issue leaves the worker. Each state's cells take a different one, so
	// every way is covered.
	away   []func(e *env, n int)
	place  func(e *env, n int) place
	handle func(e *env, n int) error
	// running is the issue's status while its run goes.
	running label.Status
	// finish plays a run that ends with a result the worker takes, and harvested checks what the
	// worker did with it.
	finish    func(e *env, name string)
	harvested func(e *env, n int)
}

func builderCells(t *testing.T) tableWorker {
	return tableWorker{
		role:  builderRole,
		table: builderTable,
		want: table{
			away:    {idle, drop, drop, drop, drop, drop, drop},
			ready:   {begin, drop, drop, drop, drop, drop, drop},
			working: {restart, restart, idle, fail, fail, fail, harvest},
		},
		start: func(e *env) int {
			task, _ := e.startedTask()
			return task
		},
		put: func(e *env, n int, p place) {
			if p == ready {
				e.relabel(n, label.Working, label.Queued)
			}
		},
		away: leave(label.Pending),
		place: func(e *env, n int) place {
			return e.builder().place(e.issue(n))
		},
		handle: func(e *env, n int) error {
			return e.builder().Handle(t.Context(), n)
		},
		running: label.Working,
		finish: func(e *env, name string) {
			e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
		},
		harvested: func(e *env, n int) {
			e.wantStatus(n, label.Review)
			if got := gittest.ReadFile(t, e.origin, branchName(n), "x.txt"); got != "x" {
				t.Errorf("x.txt on the branch = %q", got)
			}
			if n := len(e.gh.Pulls()); n != 1 {
				t.Errorf("%d PRs, want one", n)
			}
		},
	}
}

func resolverCells(t *testing.T) tableWorker {
	return tableWorker{
		role:  resolverRole,
		table: resolverTable,
		want: table{
			away:  {idle, drop, drop, drop, drop, drop, drop},
			clean: {idle, restart, idle, fail, fail, fail, harvest},
			ready: {begin, restart, idle, fail, fail, fail, harvest},
		},
		start: func(e *env) int {
			task, _, _ := e.conflicted()
			return task
		},
		put: func(e *env, n int, p place) {
			if p == clean {
				e.gh.SetMergeable(e.gh.Pulls()[0].Number, new(true))
			}
		},
		away: leave(label.Pending),
		place: func(e *env, n int) place {
			p, err := e.resolver().place(t.Context(), e.issue(n))
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
		handle: func(e *env, n int) error {
			return e.resolver().Handle(t.Context(), n)
		},
		running: label.Review,
		finish: func(e *env, name string) {
			e.play(name, resolveWith(t, "main\nfeature\n"), resolverDone)
		},
		harvested: func(e *env, n int) {
			e.wantStatus(n, label.Review)
			if got := len(gittest.Parents(t, e.origin, branchName(n))); got != 2 {
				t.Errorf("branch head has %d parents, want the merge", got)
			}
		},
	}
}

func reviewerCells(t *testing.T) tableWorker {
	return tableWorker{
		role:  reviewerRole,
		table: reviewerTable,
		want: table{
			away:       {idle, drop, drop, drop, drop, drop, drop},
			conflicted: {idle, drop, drop, drop, drop, drop, drop},
			waiting:    {idle, restart, idle, fail, fail, fail, idle},
			ready:      {begin, restart, idle, fail, fail, fail, harvest},
		},
		start: func(e *env) int {
			task, _, _, _ := e.reviewing()
			return task
		},
		put: func(e *env, n int, p place) {
			pr := e.gh.Pulls()[0].Number
			switch p {
			case conflicted:
				e.gh.SetMergeable(pr, new(false))
			case waiting:
				e.gh.SetMergeable(pr, nil)
			}
		},
		away: leave(label.Pending),
		place: func(e *env, n int) place {
			p, _, _, err := e.reviewer().look(t.Context(), e.issue(n))
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
		handle: func(e *env, n int) error {
			return e.reviewer().Handle(t.Context(), n)
		},
		running: label.Review,
		finish: func(e *env, name string) {
			e.play(name, nil, approve)
		},
		harvested: func(e *env, n int) {
			if !e.gh.Pulls()[0].Merged {
				t.Error("PR not merged")
			}
			if e.gh.Issue(n).Open {
				t.Error("Task still open")
			}
		},
	}
}

func splitterCells(t *testing.T) tableWorker {
	return tableWorker{
		role:  splitterRole,
		table: splitterTable,
		want: table{
			away:  {idle, drop, drop, drop, drop, drop, drop},
			ready: {begin, restart, idle, fail, fail, fail, harvest},
		},
		start: func(e *env) int {
			prd, _ := e.splitting()
			return prd
		},
		put: func(*env, int, place) {},
		away: []func(e *env, n int){
			func(e *env, n int) { e.gh.CloseIssue(n) },
			func(e *env, n int) { e.addLabel(n, label.Tasked) },
			func(e *env, n int) { e.addLabel(n, string(label.Stuck)) },
		},
		place: func(e *env, n int) place {
			return e.splitter().place(e.issue(n))
		},
		handle: func(e *env, n int) error {
			return e.splitter().Handle(t.Context(), n)
		},
		running: label.None,
		finish: func(e *env, name string) {
			e.play(name, nil, `{"tasks": [{"title": "A", "body": "a", "blocked_by": []}]}`)
		},
		harvested: func(e *env, n int) {
			if !e.gh.Issue(n).HasLabel(label.Tasked) {
				t.Errorf("PRD has no %s label", label.Tasked)
			}
			if subs := e.gh.Issue(n).SubIssues; len(subs) != 1 {
				t.Errorf("PRD sub-issues = %v, want one Task", subs)
			}
		},
	}
}

// leave returns the ways a Task leaves a worker: closed, relabeled to, and on its way to stuck
// with the old status still on. to must be none of the worker's own statuses.
func leave(to label.Status) []func(e *env, n int) {
	return []func(e *env, n int){
		func(e *env, n int) { e.gh.CloseIssue(n) },
		func(e *env, n int) { e.relabel(n, label.Status(e.statuses(n)[0]), to) },
		func(e *env, n int) { e.addLabel(n, string(label.Stuck)) },
	}
}

func (e *env) addLabel(n int, name string) {
	e.t.Helper()
	if err := e.deps.GitHub.AddLabel(e.t.Context(), n, name); err != nil {
		e.t.Fatal(err)
	}
}

var placeNames = map[place]string{
	away: "away", ready: "ready", working: "working", clean: "clean",
	waiting: "waiting", conflicted: "conflicted", closed: "closed", failing: "failing",
}

func actionNames(r row) []string {
	var out []string
	for _, a := range r {
		out = append(out, [...]string{"idle", "begin", "harvest", "fail", "drop", "restart"}[a])
	}
	return out
}

func TestTables(t *testing.T) {
	for _, cells := range []func(*testing.T) tableWorker{builderCells, resolverCells, reviewerCells, splitterCells} {
		w := cells(t)
		if len(w.table) != len(w.want) {
			t.Errorf("%s table has %d rows, want %d", w.role.name, len(w.table), len(w.want))
		}
		for _, p := range slices.Sorted(maps.Keys(w.want)) {
			row := w.want[p]
			// Some actions differ only in the log, so the cells below cannot tell them apart.
			if got := w.table[p]; got != row {
				t.Errorf("%s %s row = %v, want %v", w.role.name, placeNames[p], actionNames(got), actionNames(row))
			}
			for s, a := range row {
				t.Run(fmt.Sprintf("%s/%s/%s", w.role.name, placeNames[p], state(s)), func(t *testing.T) {
					testCell(t, cells(t), p, state(s), a)
				})
			}
		}
	}
}

// testCell starts a run, moves it to state s and its issue to place p, and checks that Handle
// takes action a.
func testCell(t *testing.T, w tableWorker, p place, s state, a action) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	n := w.start(e)
	name := e.runs.name(w.role, n)
	e.runTo(name, s, w.finish)
	if p == away {
		w.away[int(s)%len(w.away)](e, n)
	} else {
		w.put(e, n, p)
	}
	if got := w.place(e, n); got != p {
		t.Fatalf("place = %s, want %s", placeNames[got], placeNames[p])
	}
	statuses := e.statuses(n)
	comments := len(e.gh.Issue(n).Comments)

	if err := w.handle(e, n); err != nil {
		t.Fatal(err)
	}

	sameStatus := func() {
		t.Helper()
		if got := e.statuses(n); !slices.Equal(got, statuses) {
			t.Errorf("status labels = %q, want %q left as they were", got, statuses)
		}
	}
	noComment := func() {
		t.Helper()
		if got := len(e.gh.Issue(n).Comments); got != comments {
			t.Errorf("%d new comments, want none", got-comments)
		}
	}
	notKept := func() {
		t.Helper()
		if _, err := os.Stat(filepath.Join(e.deps.SandboxDir, "failed", name)); err == nil {
			t.Error("the run's files were kept as a failure")
		}
	}
	switch a {
	case idle:
		e.wantState(name, s)
		sameStatus()
		noComment()
	case begin:
		e.wantState(name, running)
		e.wantStatus(n, w.running)
	case drop:
		e.wantRemoved(name)
		sameStatus()
		noComment()
		notKept()
	case restart:
		e.wantRemoved(name)
		noComment()
		notKept()
		if w.role.queued != label.None {
			e.wantStatus(n, w.role.queued)
		} else {
			sameStatus()
		}
	case fail:
		e.wantRemoved(name)
		e.wantStatus(n, label.Stuck)
		wantStuckComment(t, e.lastComment(n), "The AI's run failed because "+why(s), w.role.back)
		e.wantKept(name, "input.md")
	case harvest:
		e.wantRemoved(name)
		w.harvested(e, n)
	}
}

// reviewer settles a closed or failing PR before the run's step, so those places have no row.
// Whatever state its run is in, the run goes.
func TestReviewerDropsTheRunOfAClosedOrFailingPR(t *testing.T) {
	for _, p := range []place{closed, failing} {
		for s := range finished + 1 {
			t.Run(fmt.Sprintf("%s/%s", placeNames[p], s), func(t *testing.T) {
				w := reviewerCells(t)
				e := newEnv(t, map[string]string{"app.txt": "v1\n"})
				n := w.start(e)
				name := e.runs.name(w.role, n)
				e.runTo(name, s, w.finish)
				pr := e.gh.Pulls()[0].Number
				if p == closed {
					e.gh.ClosePull(pr)
				} else {
					e.gh.SetRuns(gittest.Head(e.origin, branchName(n)), githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure"})
				}
				if got := w.place(e, n); got != p {
					t.Fatalf("place = %s, want %s", placeNames[got], placeNames[p])
				}

				if err := w.handle(e, n); err != nil {
					t.Fatal(err)
				}

				e.wantRemoved(name)
				if p == closed {
					e.wantStatus(n, label.Stuck)
					wantStuckComment(t, e.lastComment(n), "closed without being merged", label.Pending)
					return
				}
				e.wantStatus(n, label.Queued)
				if reviews := e.gh.Pull(pr).Reviews; len(reviews) != 1 || !strings.Contains(reviews[0].Body, "CI failed") {
					t.Errorf("reviews = %+v, want one on the failed CI", reviews)
				}
			})
		}
	}
}

func (e *env) issue(n int) github.Issue {
	e.t.Helper()
	i, err := e.deps.GitHub.Issue(e.t.Context(), n)
	if err != nil {
		e.t.Fatal(err)
	}
	return i
}

// runTo moves a running run to state s. finish ends it the way the worker's AI would.
func (e *env) runTo(name string, s state, finish func(e *env, name string)) {
	e.t.Helper()
	switch s {
	case none:
		if err := e.runs.remove(e.t.Context(), name, false); err != nil {
			e.t.Fatal(err)
		}
	case preparing:
		if err := os.Remove(filepath.Join(e.runs.dir(name), "start.json")); err != nil {
			e.t.Fatal(err)
		}
	case stalled:
		e.writeCtl(name, "stream.jsonl", "{}\n")
		e.wantState(name, running)
		e.clock.advance(stallAfter)
	case dead:
		e.sbx.stop(name)
	case gone:
		e.sbx.drop(name)
	case finished:
		finish(e, name)
	}
	e.wantState(name, s)
}
