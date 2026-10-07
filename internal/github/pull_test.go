package github

import (
	"fmt"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
)

func TestMerged(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
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
	client := clientOf(t, s)
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

func TestPRComments(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
	pr := s.AddPull("exomecha/prd-1", "main")
	s.AddLineComment(pr, "hello.go", 12, "rename this")
	s.AddReview(pr, "")
	s.AddPullComment(pr, "also update the README")
	s.AddPendingReview(pr, "not sent yet")
	s.AddOutsiderReview(pr, "an outsider's review")
	s.AddLineComment(pr, "hello.go", 0, "split this file")
	if err := client.AddReview(t.Context(), pr, "fix the typo"); err != nil {
		t.Fatal(err)
	}

	got, err := client.PRComments(t.Context(), pr)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range got {
		lines = append(lines, fmt.Sprintf("%s:%d %q trusted=%v exomecha=%v", c.Path, c.Line, c.Body, c.Trusted, c.Exomecha))
	}
	want := []string{
		`hello.go:12 "rename this" trusted=true exomecha=false`,
		`:0 "also update the README" trusted=true exomecha=false`,
		`:0 "an outsider's review" trusted=false exomecha=false`,
		`hello.go:0 "split this file" trusted=true exomecha=false`,
		`:0 "fix the typo" trusted=true exomecha=true`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("PRComments =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
