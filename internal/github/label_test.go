package github

import (
	"net/http"
	"slices"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const labelsRoute = "PUT /repos/{owner}/{repo}/issues/{n}/labels"

// A status change that fails must leave the old status, never the old one and the new one: every
// worker leaves an issue with two statuses alone.
func TestSetStatus(t *testing.T) {
	s := githubtest.New(t)
	client, err := NewAt(s.URL, s.Owner, s.Repo, "token")
	if err != nil {
		t.Fatal(err)
	}
	n := s.AddIssue("PRD", "", label.PRD, "priority", string(label.Ready))
	want := func(labels ...string) {
		t.Helper()
		if got := s.Issue(n).Labels; !slices.Equal(got, labels) {
			t.Errorf("labels = %q, want %q", got, labels)
		}
	}

	s.Fail(labelsRoute, http.StatusBadGateway)
	if err := client.SetStatus(t.Context(), n, label.Ready, label.Working); err == nil {
		t.Fatal("SetStatus returned nil with GitHub failing")
	}
	want(label.PRD, "priority", string(label.Ready))

	s.Fail(labelsRoute, 0)
	if err := client.SetStatus(t.Context(), n, label.Ready, label.Working); err != nil {
		t.Fatal(err)
	}
	want(label.PRD, "priority", string(label.Working))

	s.Relabel(n, "", string(label.Review))
	if err := client.SetStatus(t.Context(), n, label.Working, label.Ready); err != nil {
		t.Fatal(err)
	}
	want(label.PRD, "priority", string(label.Ready))

	if err := client.ClearStatus(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	want(label.PRD, "priority")
}
