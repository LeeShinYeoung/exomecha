package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/logging"
)

// usageFresh is how long one usage reading is shared by every worker. Each reading is a small claude call.
const usageFresh = 3 * time.Minute

// A reading answers in seconds. Every start waits on the gate while it runs, so one that hangs
// must give up.
var usageTimeout = 2 * time.Minute

// Gate stops workers from starting new work while the Claude plan is close to its limits.
// Work already running is left to finish, so usage can end up a little over the limits.
type Gate struct {
	Check    func(ctx context.Context) (agent.Usage, error)
	FiveHour float64 // share of the 5-hour limit, from 0 to 1, at which new work stops
	Weekly   float64 // share of the weekly limit, from 0 to 1, at which new work stops

	now    func() time.Time
	mu     sync.Mutex
	usage  agent.Usage
	at     time.Time
	closed bool
}

func (g *Gate) Open(ctx context.Context) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now
	if g.now != nil {
		now = g.now
	}
	if !g.fresh(now()) {
		checkCtx, cancel := context.WithTimeout(ctx, usageTimeout)
		u, err := g.Check(checkCtx)
		cancel()
		if err != nil {
			return false, fmt.Errorf("check Claude usage: %w", err)
		}
		g.usage, g.at = u, now()
	}

	u := g.usage
	over := u.Known && (u.Rejected || u.FiveHour.Used >= g.FiveHour || u.Weekly.Used >= g.Weekly)
	// Not ctx: the usage is every worker's, not the PRD's whose start read it.
	if over != g.closed {
		g.closed = over
		if over {
			slog.Warn("Claude usage over the limit, pausing new work",
				logging.FiveHour, percent(u.FiveHour.Used), logging.Weekly, percent(u.Weekly.Used), logging.Until, g.reopens())
		} else {
			slog.Info("Claude usage under the limit, resuming new work",
				logging.FiveHour, percent(u.FiveHour.Used), logging.Weekly, percent(u.Weekly.Used))
		}
	}
	return !over, nil
}

// markStale makes the next Open read usage again, after a run hit a limit the last reading did
// not show.
func (g *Gate) markStale() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.at = time.Time{}
}

// fresh turns false once a window resets, or the old reading would keep the gate closed after
// the reset. Usage only goes down at a reset, so while closed a reading stays fresh until then.
func (g *Gate) fresh(now time.Time) bool {
	if g.at.IsZero() {
		return false
	}
	resets := false
	for _, w := range []agent.Window{g.usage.FiveHour, g.usage.Weekly} {
		if w.ResetsAt.IsZero() {
			continue
		}
		if !now.Before(w.ResetsAt) {
			return false
		}
		resets = true
	}
	if g.closed && resets {
		return true
	}
	return now.Sub(g.at) < usageFresh
}

// reopens is when the windows at or over their limits reset, the latest of them. A rejected call
// with every window under its limit waits for the latest reset known.
func (g *Gate) reopens() time.Time {
	var over, all time.Time
	for _, w := range []struct {
		agent.Window
		limit float64
	}{{g.usage.FiveHour, g.FiveHour}, {g.usage.Weekly, g.Weekly}} {
		if w.ResetsAt.After(all) {
			all = w.ResetsAt
		}
		if w.Used >= w.limit && w.ResetsAt.After(over) {
			over = w.ResetsAt
		}
	}
	if over.IsZero() {
		return all
	}
	return over
}

func percent(used float64) string {
	return fmt.Sprintf("%.0f%%", used*100)
}
