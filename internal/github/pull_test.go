package github

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
)

func TestLastClosedPR(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
	const branch = "exomecha/prd-1"
	want := func(what string, number int, merged, ok bool) {
		t.Helper()
		n, m, found, err := client.LastClosedPR(t.Context(), branch)
		if err != nil || n != number || m != merged || found != ok {
			t.Errorf("%s: LastClosedPR = %d, %v, %v, %v, want %d, %v, %v", what, n, m, found, err, number, merged, ok)
		}
	}

	want("no PR", 0, false, false)
	open := s.AddPull(branch, "main")
	want("an open PR", 0, false, false)
	s.ClosePull(open)
	want("closed without a merge", open, false, true)
	merged := s.AddPull(branch, "main")
	s.MergePull(merged)
	want("merged", merged, true, true)
	closed := s.AddPull(branch, "main")
	s.ClosePull(closed)
	want("closed after a merged one", closed, false, true)
}

func TestParseNote(t *testing.T) {
	for _, c := range []struct {
		body, text string
		note       Note
		ok         bool
	}{
		{Noted(Note{Worker: "resolver", Kind: KindResolved}, "kept both sides"), "kept both sides", Note{"resolver", KindResolved}, true},
		{Marker + "\nfrom before notes", "from before notes", Note{}, true},
		{"  " + Marker + "\n\nwith space around\n", "with space around", Note{}, true},
		{"a person's comment", "a person's comment", Note{}, false},
		{"<!-- exomechanic -->\nnot exomecha's", "<!-- exomechanic -->\nnot exomecha's", Note{}, false},
		{"<!-- exomecha", "<!-- exomecha", Note{}, false},
	} {
		n, text, ok := ParseNote(c.body)
		if n != c.note || text != c.text || ok != c.ok {
			t.Errorf("ParseNote(%q) = %v, %q, %v, want %v, %q, %v", c.body, n, text, ok, c.note, c.text, c.ok)
		}
	}
	if got := Noted(Note{Worker: "builder", Kind: KindSummary}, "did it"); got != "<!-- exomecha:builder:summary -->\n**exomecha builder**\n\ndid it" {
		t.Errorf("Noted = %q", got)
	}
}

func TestSendBacks(t *testing.T) {
	s := githubtest.New(t)
	client := clientOf(t, s)
	prd := s.AddIssue("PRD", "")
	pr := s.AddPull("exomecha/prd-1", "main")
	want := func(what string, n int) {
		t.Helper()
		comments, err := client.Comments(t.Context(), prd)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := client.SendBacks(t.Context(), pr, comments); err != nil || got != n {
			t.Errorf("%s: SendBacks = %d, %v, want %d", what, got, err, n)
		}
		slices.Reverse(comments)
		if got, err := client.SendBacks(t.Context(), pr, comments); err != nil || got != n {
			t.Errorf("%s: SendBacks with the PRD's comments newest first = %d, %v, want %d", what, got, err, n)
		}
	}
	review := func(kind, body string) {
		t.Helper()
		if err := client.AddReview(t.Context(), pr, Note{Worker: "reviewer", Kind: kind}, body); err != nil {
			t.Fatal(err)
		}
	}

	if err := client.AddComment(t.Context(), prd, Note{Worker: "builder", Kind: KindStuck}, "An older question."); err != nil {
		t.Fatal(err)
	}
	want("no review", 0)
	review(KindChanges, "fix the typo")
	review(KindCI, "CI failed.")
	s.AddReview(pr, Marker+"\nfrom before reviews had a note")
	s.AddReview(pr, "a person's review")
	s.AddPendingReview(pr, Marker+"\nnot sent yet")
	s.AddOutsiderReview(pr, Marker+"\nan outsider's review with the marker")
	s.AddComment(prd, "a person's comment")
	want("three send-backs among other reviews", 3)
	if err := client.AddApproval(t.Context(), pr, "adds the greeting"); err != nil {
		t.Fatal(err)
	}
	want("approved", 0)
	review(KindChanges, "rename it")
	want("after the approval", 1)
	if err := client.AddComment(t.Context(), prd, Note{Worker: "builder", Kind: KindStuck}, "This PRD is blocked."); err != nil {
		t.Fatal(err)
	}
	want("after a stuck comment", 0)
	review(KindChanges, "still wrong")
	want("after the stuck comment", 1)
	s.AddReview(pr, Marker+"\n"+approval+"\n\nfrom before reviews had a note")
	want("after an approval from before reviews had a note", 0)
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
	if err := client.AddReview(t.Context(), pr, Note{Worker: "reviewer", Kind: KindChanges}, "fix the typo"); err != nil {
		t.Fatal(err)
	}

	got, err := client.PRComments(t.Context(), pr)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range got {
		lines = append(lines, fmt.Sprintf("%s:%d %q trusted=%v exomecha=%v %v", c.Path, c.Line, c.Body, c.Trusted, c.Exomecha, c.Note))
	}
	want := []string{
		`hello.go:12 "rename this" trusted=true exomecha=false { }`,
		`:0 "also update the README" trusted=true exomecha=false { }`,
		`:0 "an outsider's review" trusted=false exomecha=false { }`,
		`hello.go:0 "split this file" trusted=true exomecha=false { }`,
		`:0 "fix the typo" trusted=true exomecha=true {reviewer changes}`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("PRComments =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
