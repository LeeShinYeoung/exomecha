package github

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type headerTrip struct{ remaining []string }

func (h *headerTrip) RoundTrip(*http.Request) (*http.Response, error) {
	res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}
	res.Header.Set("X-RateLimit-Remaining", h.remaining[0])
	res.Header.Set("X-RateLimit-Reset", strconv.FormatInt(time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC).Unix(), 10))
	h.remaining = h.remaining[1:]
	return res, nil
}

func TestRateWatchLogsTheWaitOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	w := &rateWatch{base: &headerTrip{remaining: []string{"1", "0", "0", "4999", "4998"}}, now: func() time.Time { return now }}
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
	for range 5 {
		res, err := w.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		now = now.Add(30 * time.Minute)
	}

	got := buf.String()
	reset := time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC).Local().Format("2006-01-02T15:04:05")
	if strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, "until="+reset) {
		t.Errorf("log %q, want one wait line with the reset time", got)
	}
	if strings.Count(got, "level=INFO") != 1 || !strings.Contains(got, "waited=1h0m0s") {
		t.Errorf("log %q, want one line when calls go through again, with the time waited", got)
	}
}
