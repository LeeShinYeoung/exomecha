package github

import (
	"errors"
	"net/http"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
)

const mergeRoute = "PUT /repos/{owner}/{repo}/pulls/{n}/merge"

func TestMerge(t *testing.T) {
	for _, c := range []struct {
		name   string
		refuse func(s *githubtest.Server, pr int)
		status int // of the MergeRefused, 0 for none
		err    bool
	}{
		{"merged", func(*githubtest.Server, int) {}, 0, false},
		{"refused by a rule", func(s *githubtest.Server, pr int) {
			s.RefuseMerge(pr, "At least 1 approving review is required by reviewers with write access.", new(true))
		}, http.StatusMethodNotAllowed, true},
		{"head moved", func(s *githubtest.Server, pr int) { s.Fail(mergeRoute, http.StatusConflict) }, http.StatusConflict, true},
		{"raced another merge", func(s *githubtest.Server, pr int) {
			s.RefuseMerge(pr, "Base branch was modified. Review and try the merge again.", new(true))
		}, 0, true},
		{"server error", func(s *githubtest.Server, pr int) { s.Fail(mergeRoute, http.StatusBadGateway) }, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := githubtest.New(t)
			client, err := NewAt(s.URL, s.Owner, s.Repo, "token")
			if err != nil {
				t.Fatal(err)
			}
			pr := s.AddPull("exomecha/task-1", "main")
			c.refuse(s, pr)

			err = client.Merge(t.Context(), pr, "")

			var refused *MergeRefused
			switch {
			case c.err != (err != nil):
				t.Fatalf("Merge = %v, want an error %v", err, c.err)
			case c.status == 0 && errors.As(err, &refused):
				t.Errorf("Merge = %#v, want an error that is no refusal", refused)
			case c.status != 0 && (!errors.As(err, &refused) || refused.Status != c.status || refused.Message == ""):
				t.Errorf("Merge = %v, want a refusal with status %d and GitHub's message", err, c.status)
			}
			if merged := s.Pull(pr).Merged; merged == c.err {
				t.Errorf("PR merged = %v", merged)
			}
		})
	}
}
