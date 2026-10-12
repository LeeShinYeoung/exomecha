package github

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestUnchangedAnswerComesBackAs304(t *testing.T) {
	var mu sync.Mutex
	title, etag := "first", `"v1"`
	notModified := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("If-None-Match") == etag {
			notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"number": 1, "title": %q, "state": "open"}`, title)
	}))
	t.Cleanup(srv.Close)
	c, err := NewAt(srv.URL+"/", "owner", "repo", "token")
	if err != nil {
		t.Fatal(err)
	}
	title1 := func() string {
		t.Helper()
		i, err := c.Issue(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		return i.Title
	}

	if got := title1(); got != "first" {
		t.Fatalf("first call: title %q", got)
	}
	if got := title1(); got != "first" {
		t.Fatalf("second call: title %q, want the stored answer", got)
	}
	mu.Lock()
	if notModified != 1 {
		t.Errorf("server answered 304 %d times, want 1", notModified)
	}
	title, etag = "second", `"v2"`
	mu.Unlock()
	if got := title1(); got != "second" {
		t.Errorf("after a change: title %q, want the new answer", got)
	}
}

// CheckCI asks a new URL for every commit, so a store that never drops an answer grows for as long
// as exomecha runs.
func TestETagStoreDropsTheLeastRecentlyUsed(t *testing.T) {
	var mu sync.Mutex
	conditional := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conditional[r.URL.Path] = r.Header.Get("If-None-Match") != ""
		mu.Unlock()
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	tr := newETagTransport(srv.Client().Transport)
	tr.limit = 2
	// stored reports whether the request for path asked with the ETag of a stored answer.
	stored := func(path string) bool {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || string(body) != path {
			t.Fatalf("GET %s = %q, %v", path, body, err)
		}
		mu.Lock()
		defer mu.Unlock()
		return conditional[path]
	}

	stored("/a")
	stored("/b")
	if !stored("/a") {
		t.Fatal("/a was asked without its ETag")
	}
	stored("/c")
	if !stored("/a") {
		t.Error("/a was dropped, though /b was used less recently")
	}
	if stored("/b") {
		t.Error("/b was kept past the limit")
	}
	if n := len(tr.seen); n > tr.limit {
		t.Errorf("store holds %d answers, want at most %d", n, tr.limit)
	}
}
