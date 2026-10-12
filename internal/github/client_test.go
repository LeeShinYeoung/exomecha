package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
)

func clientOf(t *testing.T, s *githubtest.Server) *Client {
	t.Helper()
	c, err := NewAt(s.URL, s.Owner, s.Repo, "token")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRequestGivesUpOnStalledServer(t *testing.T) {
	old := requestTimeout
	requestTimeout = time.Second
	t.Cleanup(func() { requestTimeout = old })

	// Never answers. Closing done lets the handler return so the server can shut down.
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-done }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })

	c, err := NewAt(srv.URL+"/", "owner", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := c.Issue(ctx, 1); err == nil {
		t.Fatal("Issue succeeded against a server that never answers")
	}
	if ctx.Err() != nil {
		t.Fatal("the request kept waiting until the test deadline")
	}
	t.Logf("gave up after %s", time.Since(start).Round(100*time.Millisecond))
}

func TestIdentity(t *testing.T) {
	for _, c := range []struct{ profile, name string }{
		{"", "owner"},
		{"Owner Name", "Owner Name"},
	} {
		s := githubtest.New(t)
		s.UserName = c.profile
		client := clientOf(t, s)
		name, email, err := client.Identity(t.Context())
		if err != nil || name != c.name || email != "4242+owner@users.noreply.github.com" {
			t.Errorf("profile name %q: Identity = %q, %q, %v", c.profile, name, email, err)
		}
	}
}

func TestReach(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
	if err := client.Reach(t.Context()); err != nil {
		t.Errorf("Reach = %v", err)
	}
	s.Fail("GET /repos/{owner}/{repo}", http.StatusNotFound)
	if err := client.Reach(t.Context()); err == nil {
		t.Error("Reach of a repository the token cannot see = nil")
	}
}

func TestWaitForRateLimit(t *testing.T) {
	reset := time.Now().Add(time.Second).Truncate(time.Second).Add(time.Second)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message": "API rate limit exceeded"}`)
			return
		}
		fmt.Fprint(w, `{"number": 1, "title": "after reset", "state": "open"}`)
	}))
	t.Cleanup(srv.Close)
	c, err := NewAt(srv.URL+"/", "owner", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(WaitForRateLimit(t.Context()), 20*time.Second)
	defer cancel()
	i, err := c.Issue(ctx, 1)
	if err != nil {
		t.Fatalf("Issue failed instead of waiting for the reset: %v", err)
	}
	if i.Title != "after reset" || calls.Load() != 2 {
		t.Errorf("title %q after %d calls, want the answer to the retry", i.Title, calls.Load())
	}
	if time.Now().Before(reset) {
		t.Errorf("retried before the reset time %s", reset)
	}
}
