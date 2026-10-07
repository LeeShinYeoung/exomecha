package github

import (
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
)

func TestMerged(t *testing.T) {
	s := githubtest.New(t)
	client, err := NewAt(s.URL, s.Owner, s.Repo, "token")
	if err != nil {
		t.Fatal(err)
	}
	const branch = "exomecha/prd-1"
	want := func(what string, merged bool) {
		t.Helper()
		if got, err := client.Merged(t.Context(), branch); err != nil || got != merged {
			t.Errorf("%s: Merged = %v, %v, want %v", what, got, err, merged)
		}
	}

	want("no PR", false)
	first := s.AddPull(branch, "main")
	s.ClosePull(first)
	want("closed without a merge", false)
	s.MergePull(s.AddPull(branch, "main"))
	want("merged", true)
	s.ClosePull(s.AddPull(branch, "main"))
	want("closed after a merged one", false)
}

func TestLatestReview(t *testing.T) {
	s := githubtest.New(t)
	client, err := NewAt(s.URL, s.Owner, s.Repo, "token")
	if err != nil {
		t.Fatal(err)
	}
	pr := s.AddPull("exomecha/prd-1", "main")
	want := func(what, review string) {
		t.Helper()
		if got, err := client.LatestReview(t.Context(), pr); err != nil || got != review {
			t.Errorf("%s: LatestReview = %q, %v, want %q", what, got, err, review)
		}
	}

	want("no review", "")
	if err := client.AddReview(t.Context(), pr, "fix the typo"); err != nil {
		t.Fatal(err)
	}
	s.AddReview(pr, "a person's review")
	s.AddOutsiderReview(pr, Marker+"\nan outsider's review with the marker")
	want("changes asked", "fix the typo")
	if err := client.AddApproval(t.Context(), pr, "adds the greeting"); err != nil {
		t.Fatal(err)
	}
	want("approved after the changes", "")
	if err := client.AddReview(t.Context(), pr, "CI failed"); err != nil {
		t.Fatal(err)
	}
	want("CI failed after the approval", "CI failed")
}
