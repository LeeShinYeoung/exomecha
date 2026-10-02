package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
)

// usageFresh is how long one usage reading is shared by every worker. Each reading is a small claude call.
const usageFresh = 3 * time.Minute

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
		u, err := g.Check(ctx)
		if err != nil {
			return false, fmt.Errorf("check Claude usage: %w", err)
		}
		g.usage, g.at = u, now()
	}

	u := g.usage
	closed := u.Known && (u.Rejected || u.FiveHour.Used >= g.FiveHour || u.Weekly.Used >= g.Weekly)
	if closed != g.closed {
		g.closed = closed
		if closed {
			slog.Warn("Claude usage over the limit, pausing new work",
				"five_hour", percent(u.FiveHour.Used), "five_hour_resets", u.FiveHour.ResetsAt.Local().Format(time.DateTime),
				"weekly", percent(u.Weekly.Used), "weekly_resets", u.Weekly.ResetsAt.Local().Format(time.DateTime))
		} else {
			slog.Info("Claude usage back under the limit, resuming")
		}
	}
	return !closed, nil
}

// stale makes the next Open read usage again, after a run hit a limit the last reading did not show.
func (g *Gate) stale() {
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

func percent(used float64) string {
	return fmt.Sprintf("%.0f%%", used*100)
}
