package github

import (
	"net/http"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
)

const closeRoute = "PATCH /repos/{owner}/{repo}/issues/{n}"

func TestCloseRefused(t *testing.T) {
	s := githubtest.New(t)
	client, err := NewAt(s.URL, s.Owner, s.Repo, "token")
	if err != nil {
		t.Fatal(err)
	}
	s.Fail(closeRoute, http.StatusUnprocessableEntity)

	open := s.AddIssue("PRD", "")
	if err := client.Close(t.Context(), open); err == nil {
		t.Error("Close returned nil for an open issue GitHub refused to close")
	}

	// A merged PR that says "Closes" closed it a moment before.
	closed := s.AddIssue("PRD", "")
	s.CloseIssue(closed)
	if err := client.Close(t.Context(), closed); err != nil {
		t.Errorf("Close of an issue already closed = %v, want nil", err)
	}
}
