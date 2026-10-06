package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

// fakeUsage is a Gate whose clock the test moves and whose readings the test sets.
type fakeUsage struct {
	gate   *Gate
	clock  time.Time
	usage  agent.Usage
	err    error
	checks int
}

func newFakeUsage() *fakeUsage {
	f := &fakeUsage{clock: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	f.usage = agent.Usage{
		Known:    true,
		FiveHour: agent.Window{Used: 0.5, ResetsAt: f.clock.Add(2 * time.Hour)},
		Weekly:   agent.Window{Used: 0.5, ResetsAt: f.clock.Add(48 * time.Hour)},
	}
	f.gate = &Gate{
		Check: func(context.Context) (agent.Usage, error) {
			f.checks++
			return f.usage, f.err
		},
		FiveHour: 0.9,
		Weekly:   0.8,
		now:      func() time.Time { return f.clock },
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
	f := newFakeUsage()
	f.wantOpen(t, true)
	f.clock = f.clock.Add(usageFresh - time.Second)
	f.wantOpen(t, true)
	f.wantChecks(t, 1)

	f.clock = f.clock.Add(time.Second)
	f.wantOpen(t, true)
	f.wantChecks(t, 2)
}

func TestGateChecksAgainAfterReset(t *testing.T) {
	f := newFakeUsage()
	f.usage.FiveHour = agent.Window{Used: 0.95, ResetsAt: f.clock.Add(time.Minute)}
	f.wantOpen(t, false)

	f.usage.FiveHour = agent.Window{Used: 0, ResetsAt: f.clock.Add(5 * time.Hour)}
	f.clock = f.clock.Add(time.Minute)
	f.wantOpen(t, true)
	f.wantChecks(t, 2)
}

func TestGateWaitsForResetWhileClosed(t *testing.T) {
	f := newFakeUsage()
	f.usage.FiveHour = agent.Window{Used: 0.95, ResetsAt: f.clock.Add(2 * time.Hour)}
	f.wantOpen(t, false)
	f.clock = f.clock.Add(time.Hour)
	f.wantOpen(t, false)
	f.wantChecks(t, 1)

	f.usage.FiveHour = agent.Window{Used: 0, ResetsAt: f.clock.Add(5 * time.Hour)}
	f.clock = f.clock.Add(time.Hour)
	f.wantOpen(t, true)
	f.wantChecks(t, 2)
}

func TestGateLimits(t *testing.T) {
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
			f := newFakeUsage()
			f.usage.FiveHour.Used, f.usage.Weekly.Used, f.usage.Rejected = c.fiveHour, c.weekly, c.rejected
			f.wantOpen(t, c.open)
		})
	}
}

func TestGateOpenWhenUsageUnknown(t *testing.T) {
	f := newFakeUsage()
	f.usage = agent.Usage{}
	f.wantOpen(t, true)
}

func TestGateCheckError(t *testing.T) {
	f := newFakeUsage()
	f.err = errors.New("claude crashed")
	if open, err := f.gate.Open(t.Context()); err == nil || open {
		t.Errorf("Open = %v, %v, want closed with an error", open, err)
	}
}

// A run ended at the limit, so the reading missed it. The next start reads again, and the gate
// then stays closed until the reset.
func TestGateReadsAgainWhenStale(t *testing.T) {
	f := newFakeUsage()
	f.wantOpen(t, true)
	f.usage.FiveHour.Used = 0.95
	f.gate.stale()
	f.wantOpen(t, false)
	f.wantChecks(t, 2)

	f.clock = f.clock.Add(time.Hour)
	f.wantOpen(t, false)
	f.wantChecks(t, 2)
}

// Only a start reads usage, so a step that starts nothing spends none of it.
func TestUsageIsReadOnlyForAStart(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	f := newFakeUsage()
	e.runs.gate = f.gate
	alone := e.gh.AddIssue("PRD with no status", "", label.PRD)
	task, pr, _ := e.reviewTask()
	e.gh.SetMergeable(pr, nil)

	if err := e.builder().Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.review(task)
	f.wantChecks(t, 0)
	e.wantStatus(alone, label.Queued)

	e.gh.SetMergeable(pr, new(true))
	e.review(task)
	f.wantChecks(t, 1)
	e.wantState(e.reviewerBox(task), running)

	f.clock = f.clock.Add(usageFresh)
	e.review(task)
	f.wantChecks(t, 1)
}

// Over the limit no run starts, a project run or a reader's. A step without claude still goes on.
func TestNoRunStartsOverTheLimit(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	f := newFakeUsage()
	f.usage.FiveHour.Used = 0.95
	e.runs.gate = f.gate
	alone := e.gh.AddIssue("PRD with no status", "", label.PRD)
	queued := e.gh.AddIssue("queued", "", label.PRD, string(label.Queued))
	review, _, _ := e.reviewTask()
	conflict, _, _ := e.conflictTask(map[string]string{"app.txt": "feature\n"}, map[string]string{"app.txt": "main\n"})

	if err := e.builder().Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.step(queued)
	e.review(review)
	e.resolve(conflict)

	e.wantStatus(alone, label.Queued)
	for _, name := range []string{e.builderBox(queued), e.reviewerBox(review), e.resolverBox(conflict)} {
		if e.sbx.exists(name) {
			t.Errorf("%s started over the limit", name)
		}
	}
	e.wantStatus(queued, label.Queued)
}
