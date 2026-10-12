package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

// fakeUsage is a Gate whose clock the test moves and whose readings the test sets.
type fakeUsage struct {
	gate   *Gate
	clock  *clock
	usage  agent.Usage
	err    error
	checks int
}

func newFakeUsage(c *clock) *fakeUsage {
	f := &fakeUsage{clock: c}
	f.usage = agent.Usage{
		Known:    true,
		FiveHour: agent.Window{Used: 0.5, ResetsAt: f.clock.now().Add(2 * time.Hour)},
		Weekly:   agent.Window{Used: 0.5, ResetsAt: f.clock.now().Add(48 * time.Hour)},
	}
	f.gate = &Gate{
		Check: func(context.Context) (agent.Usage, error) {
			f.checks++
			return f.usage, f.err
		},
		FiveHour: 0.9,
		Weekly:   0.8,
		now:      c.now,
	}
	return f
}

func (f *fakeUsage) wantOpen(t *testing.T, want bool) {
	t.Helper()
	open, err := f.gate.Open(t.Context())
	if err != nil || open != want {
		t.Fatalf("Open = %v, %v, want %v", open, err, want)
	}
}

func (f *fakeUsage) wantChecks(t *testing.T, want int) {
	t.Helper()
	if f.checks != want {
		t.Errorf("usage checked %d times, want %d", f.checks, want)
	}
}

func TestGateReusesReadingForThreeMinutes(t *testing.T) {
	t.Parallel()
	f := newFakeUsage(newClock())
	f.wantOpen(t, true)
	f.clock.advance(usageFresh - time.Second)
	f.wantOpen(t, true)
	f.wantChecks(t, 1)

	f.clock.advance(time.Second)
	f.wantOpen(t, true)
	f.wantChecks(t, 2)
}

func TestGateChecksAgainAfterReset(t *testing.T) {
	t.Parallel()
	f := newFakeUsage(newClock())
	f.usage.FiveHour = agent.Window{Used: 0.95, ResetsAt: f.clock.now().Add(time.Minute)}
	f.wantOpen(t, false)

	f.usage.FiveHour = agent.Window{Used: 0, ResetsAt: f.clock.now().Add(5 * time.Hour)}
	f.clock.advance(time.Minute)
	f.wantOpen(t, true)
	f.wantChecks(t, 2)
}

func TestGateWaitsForResetWhileClosed(t *testing.T) {
	t.Parallel()
	f := newFakeUsage(newClock())
	f.usage.FiveHour = agent.Window{Used: 0.95, ResetsAt: f.clock.now().Add(2 * time.Hour)}
	f.wantOpen(t, false)
	f.clock.advance(time.Hour)
	f.wantOpen(t, false)
	f.wantChecks(t, 1)

	f.usage.FiveHour = agent.Window{Used: 0, ResetsAt: f.clock.now().Add(5 * time.Hour)}
	f.clock.advance(time.Hour)
	f.wantOpen(t, true)
	f.wantChecks(t, 2)
}

func TestGateLimits(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name             string
		fiveHour, weekly float64
		rejected         bool
		open             bool
	}{
		{"under both", 0.89, 0.79, false, true},
		{"five-hour limit", 0.9, 0.5, false, false},
		{"weekly limit", 0.5, 0.8, false, false},
		{"claude refuses", 0.1, 0.1, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeUsage(newClock())
			f.usage.FiveHour.Used, f.usage.Weekly.Used, f.usage.Rejected = c.fiveHour, c.weekly, c.rejected
			f.wantOpen(t, c.open)
		})
	}
}

func TestGateOpenWhenUsageUnknown(t *testing.T) {
	t.Parallel()
	f := newFakeUsage(newClock())
	f.usage = agent.Usage{}
	f.wantOpen(t, true)
}

func TestGateCheckError(t *testing.T) {
	t.Parallel()
	f := newFakeUsage(newClock())
	f.err = errors.New("claude crashed")
	if open, err := f.gate.Open(t.Context()); err == nil || open {
		t.Errorf("Open = %v, %v, want closed with an error", open, err)
	}
}

// A run ended at the limit, so the reading missed it. The next start reads again, and the gate
// then stays closed until the reset.
func TestGateReadsAgainWhenStale(t *testing.T) {
	t.Parallel()
	f := newFakeUsage(newClock())
	f.wantOpen(t, true)
	f.usage.FiveHour.Used = 0.95
	f.gate.markStale()
	f.wantOpen(t, false)
	f.wantChecks(t, 2)

	f.clock.advance(time.Hour)
	f.wantOpen(t, false)
	f.wantChecks(t, 2)
}

// Every start waits on the gate while it reads usage, so a reading that hangs must give up.
func TestGateGivesUpOnAHungCheck(t *testing.T) {
	old := usageTimeout
	usageTimeout = 10 * time.Millisecond
	t.Cleanup(func() { usageTimeout = old })
	g := &Gate{Check: func(ctx context.Context) (agent.Usage, error) {
		<-ctx.Done()
		return agent.Usage{}, ctx.Err()
	}}

	err := returns(t, "Open", func() error {
		_, err := g.Open(t.Context())
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Open = %v, want it to give up on the usage check", err)
	}
}

// Only a start reads usage, so a step that starts nothing spends none of it.
func TestUsageIsReadOnlyForAStart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	f := newFakeUsage(newClock())
	e.runs.gate = f.gate
	prd, pr, _ := e.reviewPRD()
	e.gh.SetMergeable(pr, nil)

	e.review(prd)
	f.wantChecks(t, 0)

	e.gh.SetMergeable(pr, new(true))
	e.review(prd)
	f.wantChecks(t, 1)
	e.wantState(e.reviewerBox(prd), running)

	f.clock.advance(usageFresh)
	e.review(prd)
	f.wantChecks(t, 1)
}

// Over the limit no run starts, a project run or a reader's. A step without claude still goes on.
func TestNoRunStartsOverTheLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	f := newFakeUsage(newClock())
	f.usage.FiveHour.Used = 0.95
	e.runs.gate = f.gate
	ready := e.addPRD("ready", "", label.PRD, string(label.Ready))
	review, _, _ := e.reviewPRD()
	failed, _, sha := e.reviewPRD()
	e.gh.SetRuns(sha, githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure", URL: "http://ci/test"})
	conflict, _, _ := e.conflictPRD(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	e.step(ready)
	e.review(review)
	e.review(failed)
	e.resolve(conflict)

	for _, name := range []string{e.builderBox(ready), e.reviewerBox(review), e.resolverBox(conflict)} {
		if e.sbx.exists(name) {
			t.Errorf("%s started over the limit", name)
		}
	}
	e.wantStatus(ready, label.Ready)
	e.wantStatus(failed, label.Ready)
}
