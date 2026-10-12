package github

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const labelsRoute = "PUT /repos/{owner}/{repo}/issues/{n}/labels"

func TestEnsureLabels(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
	var all []string
	for _, d := range label.All() {
		all = append(all, d.Name)
	}

	created, err := client.EnsureLabels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(created, all) || !slices.Equal(s.Labels(), all) {
		t.Errorf("created %q, the repository has %q, want %q", created, s.Labels(), all)
	}

	// It runs at every start, so a second call must add nothing.
	if created, err = client.EnsureLabels(t.Context()); err != nil || len(created) != 0 {
		t.Errorf("second call created %q, %v, want none", created, err)
	}
	if got := s.Labels(); !slices.Equal(got, all) {
		t.Errorf("labels after a second call = %q, want %q", got, all)
	}
}

// A status change that fails must leave the old status, never the old one and the new one: every
// worker leaves an issue with two statuses alone.
func TestSetStatus(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
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

	// A person moves the PRD after the worker read it as working.
	s.Relabel(n, string(label.Working), string(label.Review))
	err := client.SetStatus(t.Context(), n, label.Working, label.Ready)
	if err == nil || !strings.Contains(err.Error(), string(label.Review)) {
		t.Errorf("SetStatus after a person moved the PRD = %v, want an error that names %s", err, label.Review)
	}
	want(label.PRD, "priority", string(label.Review))

	s.Relabel(n, "", string(label.Stuck))
	if err := client.SetStatus(t.Context(), n, label.Review, label.Ready); err == nil {
		t.Error("SetStatus returned nil with a second status a person added")
	}
	want(label.PRD, "priority", string(label.Review), string(label.Stuck))

	if err := client.ClearStatus(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	want(label.PRD, "priority")
}
