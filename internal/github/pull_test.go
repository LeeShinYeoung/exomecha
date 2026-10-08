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

func TestSendBacks(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
	prd := s.AddIssue("PRD", "")
	pr := s.AddPull("exomecha/prd-1", "main")
	want := func(what string, n int) {
		t.Helper()
		if got, err := client.SendBacks(t.Context(), prd, pr); err != nil || got != n {
			t.Errorf("%s: SendBacks = %d, %v, want %d", what, got, err, n)
		}
	}
	review := func(body string) {
		t.Helper()
		if err := client.AddReview(t.Context(), pr, body); err != nil {
			t.Fatal(err)
		}
	}

	want("no review", 0)
	review("fix the typo")
	review("CI failed.")
	s.AddReview(pr, "a person's review")
	s.AddPendingReview(pr, Marker+"\nnot sent yet")
	s.AddOutsiderReview(pr, Marker+"\nan outsider's review with the marker")
	s.AddComment(prd, "a person's comment")
	want("two send-backs among other reviews", 2)
	if err := client.AddApproval(t.Context(), pr, "adds the greeting"); err != nil {
		t.Fatal(err)
	}
	want("approved", 0)
	review("rename it")
	want("after the approval", 1)
	if err := client.AddComment(t.Context(), prd, "This PRD is blocked and needs a person."); err != nil {
		t.Fatal(err)
	}
	want("after a stuck comment", 0)
	review("still wrong")
	want("after the stuck comment", 1)
}

func TestFailedJobLogs(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
	var log strings.Builder
	log.WriteString("\ufeff")
	for k := 1; k <= 100; k++ {
		fmt.Fprintf(&log, "2026-01-01T00:00:%02d.1234567Z line %d\n", k%60, k)
	}
	s.SetRuns("abc", githubtest.Run{Name: "ci", Status: "completed", Conclusion: "failure", Jobs: []githubtest.Job{
		{Name: "lint", Conclusion: "success", Log: "fine\n"},
		{Name: "test", Conclusion: "failure", Log: log.String()},
		{Name: "deploy", Conclusion: "skipped"},
	}})

	_, failed, err := client.CheckCI(t.Context(), "abc")
	if err != nil || len(failed) != 1 {
		t.Fatalf("CheckCI = %+v, %v", failed, err)
	}
	logs, err := client.FailedJobLogs(t.Context(), failed[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Job != "test" {
		t.Fatalf("logs = %+v, want only the failed job's", logs)
	}
	lines := strings.Split(logs[0].Tail, "\n")
	if len(lines) != logLines || lines[0] != "line 21" || lines[len(lines)-1] != "line 100" {
		t.Errorf("tail = %d lines from %q to %q, want lines 21 to 100 without their times", len(lines), lines[0], lines[len(lines)-1])
	}
}

func TestFailedJobLogsWhenALogIsGone(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
	s.SetRuns("abc", githubtest.Run{Name: "ci", Status: "completed", Conclusion: "failure", Jobs: []githubtest.Job{
		{Name: "test", Conclusion: "failure", Gone: true},
	}})
	_, failed, err := client.CheckCI(t.Context(), "abc")
	if err != nil || len(failed) != 1 {
		t.Fatalf("CheckCI = %+v, %v", failed, err)
	}
	if logs, err := client.FailedJobLogs(t.Context(), failed[0].ID); err == nil {
		t.Errorf("FailedJobLogs = %+v, want an error", logs)
	}
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
