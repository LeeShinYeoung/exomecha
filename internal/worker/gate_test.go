package worker

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
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

// pickOnly is a Worker that picks fixed numbers and records what it handles.
type pickOnly struct {
	picks   []int
	mu      sync.Mutex
	handled []int
}

func (*pickOnly) Name() string                          { return "test" }
func (p *pickOnly) Pick(context.Context) ([]int, error) { return p.picks, nil }
func (p *pickOnly) Handle(_ context.Context, n int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handled = append(p.handled, n)
	return nil
}

func runRound(t *testing.T, w *pickOnly, gate *Gate) {
	t.Helper()
	var wg sync.WaitGroup
	round(t.Context(), w, nil, &locks{held: map[int]bool{}}, &wg, gate)
	wg.Wait()
}

func TestRoundChecksUsageOnlyWithWork(t *testing.T) {
	f := newFakeUsage()
	runRound(t, &pickOnly{}, f.gate)
	f.wantChecks(t, 0)

	w := &pickOnly{picks: []int{1}}
	runRound(t, w, f.gate)
	f.wantChecks(t, 1)
	wantInts(t, "handled", w.handled, []int{1})
}

func TestRoundSkipsWorkOverTheLimit(t *testing.T) {
	f := newFakeUsage()
	f.usage.FiveHour.Used = 0.95
	w := &pickOnly{picks: []int{1, 2}}
	runRound(t, w, f.gate)
	wantInts(t, "handled", w.handled, nil)
}

// builder and resolver run with no gate in round, and check it themselves before a start.
func TestRoundWithoutGateHandlesEverything(t *testing.T) {
	w := &pickOnly{picks: []int{1, 2}}
	runRound(t, w, nil)
	slices.Sort(w.handled)
	wantInts(t, "handled", w.handled, []int{1, 2})
}
