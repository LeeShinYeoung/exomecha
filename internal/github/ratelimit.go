package github

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/logging"
)

// rateWatch logs when the rate limit runs out and when calls go through again. go-github waits
// for the reset on its own and says nothing, so without these lines exomecha looks stuck.
type rateWatch struct {
	base http.RoundTripper
	now  func() time.Time

	mu    sync.Mutex
	since time.Time // when the limit ran out, zero while calls go through
}

func (t *rateWatch) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return res, err
	}
	remaining := res.Header.Get("X-RateLimit-Remaining")
	if remaining == "" {
		return res, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	// Not req's context: the limit is every worker's, not the PRD's whose call used it up.
	switch {
	case remaining == "0" && t.since.IsZero():
		reset, err := strconv.ParseInt(res.Header.Get("X-RateLimit-Reset"), 10, 64)
		if err != nil || !time.Unix(reset, 0).After(now) {
			return res, nil
		}
		t.since = now
		slog.Warn("GitHub rate limit used up, calls wait for the reset", logging.Until, time.Unix(reset, 0))
	case remaining != "0" && !t.since.IsZero():
		slog.Info("GitHub calls go through again", logging.Waited, now.Sub(t.since).Round(time.Second))
		t.since = time.Time{}
	}
	return res, nil
}
