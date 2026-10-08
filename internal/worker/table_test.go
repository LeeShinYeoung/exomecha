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
	away []func(e *env, n int)
	// settled checks what Handle did to the issue after the run's action, for the places where it
	// does more than that.
	settled map[place]func(e *env, n int)
	place   func(e *env, n int) place
	handle  func(e *env, n int) error
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
			prd, _ := e.startedPRD()
			return prd
		},
		put: func(e *env, n int, p place) {
			if p == ready {
				e.gh.Relabel(n, string(label.Working), string(label.Ready))
			}
		},
		away: leave(label.Review),
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
			prd, _, _ := e.conflicted()
			return prd
		},
		put: func(e *env, n int, p place) {
			if p == clean {
				e.gh.SetMergeable(e.gh.Pulls()[0].Number, new(true))
			}
		},
		away: leave(label.Ready),
		place: func(e *env, n int) place {
			p, _, err := e.resolver().place(t.Context(), e.issue(n))
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
			away:    {idle, drop, drop, drop, drop, drop, drop},
			merged:  {idle, drop, drop, drop, drop, drop, drop},
			closed:  {idle, drop, drop, drop, drop, drop, drop},
			failing: {idle, drop, drop, drop, drop, drop, drop},
			waiting: {idle, restart, idle, fail, fail, fail, idle},
			ready:   {begin, restart, idle, fail, fail, fail, harvest},
		},
		start: func(e *env) int {
			prd, _, _, _ := e.reviewing()
			return prd
		},
		put: func(e *env, n int, p place) {
			pr := e.gh.Pulls()[0].Number
			switch p {
			case merged:
				e.gh.MergePull(pr)
			case closed:
				e.gh.ClosePull(pr)
			case failing:
				e.gh.SetRuns(gittest.Head(e.origin, branchName(n)), githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure"})
			case waiting:
				e.gh.SetMergeable(pr, nil)
			}
		},
		// A conflict is resolver's, and an approved PRD waits for a person.
		away: append(leave(label.Ready),
			func(e *env, n int) { e.gh.SetMergeable(e.gh.Pulls()[0].Number, new(false)) },
			func(e *env, n int) { e.gh.Relabel(n, string(label.Review), string(label.Approved)) }),
		settled: map[place]func(e *env, n int){
			merged: func(e *env, n int) {
				e.wantFinished(n)
			},
			closed: func(e *env, n int) {
				e.wantStatus(n, label.Stuck)
				wantStuckComment(t, e.lastComment(n), "closed without being merged", label.Ready)
			},
			failing: func(e *env, n int) {
				e.wantStatus(n, label.Ready)
				if reviews := e.gh.Pulls()[0].Reviews; len(reviews) != 1 || !strings.Contains(reviews[0].Body, "CI failed") {
					t.Errorf("reviews = %+v, want one on the failed CI", reviews)
				}
			},
		},
		place: func(e *env, n int) place {
			p, _, _, err := e.reviewer().look(t.Context(), e.issue(n), e.deps.Repos)
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
			e.wantApproved(n, e.gh.Pulls()[0].Number)
		},
	}
}

// leave returns the ways a PRD leaves a worker: closed, relabeled to, and given a second status
// by a person. to must be none of the worker's own statuses.
func leave(to label.Status) []func(e *env, n int) {
	return []func(e *env, n int){
		func(e *env, n int) { e.gh.CloseIssue(n) },
		func(e *env, n int) { e.gh.Relabel(n, e.statuses(n)[0], string(to)) },
		func(e *env, n int) { e.gh.Relabel(n, "", string(label.Stuck)) },
	}
}

var placeNames = map[place]string{
	away: "away", ready: "ready", working: "working", clean: "clean",
	waiting: "waiting", merged: "merged", closed: "closed", failing: "failing", stale: "stale",
}

func actionNames(r row) []string {
	var out []string
	for _, a := range r {
		out = append(out, [...]string{"idle", "begin", "harvest", "fail", "drop", "restart"}[a])
	}
	return out
}

func TestTables(t *testing.T) {
	t.Parallel()
	for _, cells := range []func(*testing.T) tableWorker{builderCells, resolverCells, reviewerCells} {
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
					t.Parallel()
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
	reviews := func() (total int) {
		for _, p := range e.gh.Pulls() {
			total += len(p.Reviews)
		}
		return total
	}
	reviewed := reviews()

	if err := w.handle(e, n); err != nil {
		t.Fatal(err)
	}

	settled := w.settled[p]
	if settled != nil {
		settled(e, n)
	}
	sameStatus := func() {
		t.Helper()
		if got := e.statuses(n); settled == nil && !slices.Equal(got, statuses) {
			t.Errorf("status labels = %q, want %q left as they were", got, statuses)
		}
	}
	noComment := func() {
		t.Helper()
		if got := len(e.gh.Issue(n).Comments); settled == nil && got != comments {
			t.Errorf("%d new comments, want none", got-comments)
		}
		if got := reviews(); settled == nil && got != reviewed {
			t.Errorf("%d new PR reviews, want none", got-reviewed)
		}
	}
	notKept := func() {
		t.Helper()
		if dirs := e.kept(name); len(dirs) != 0 {
			t.Errorf("kept the run's output in %q, want nothing kept", dirs)
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
		if w.role.ready != label.None {
			e.wantStatus(n, w.role.ready)
		} else {
			sameStatus()
		}
	case fail:
		e.wantRemoved(name)
		e.wantStatus(n, label.Stuck)
		wantStuckComment(t, e.lastComment(n), "The AI's run failed because "+e.runs.why(s), w.role.back)
		// Only a stalled run here wrote any output.
		if s == stalled {
			e.wantKept(name, "stream.jsonl")
		}
	case harvest:
		e.wantRemoved(name)
		w.harvested(e, n)
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
		if err := e.runs.remove(e.t.Context(), name); err != nil {
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
