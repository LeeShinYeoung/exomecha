package github

import (
	"fmt"
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
