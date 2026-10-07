package githubtest

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
)

func TestMain(m *testing.M) { gittest.Main(m) }

func newLive(t *testing.T, defaultBranch string) (*Server, *github.Client, string) {
	t.Helper()
	origin := gittest.NewOrigin(t, map[string]string{"app.txt": "v1\n"})
	s := New(t)
	s.Live(origin, defaultBranch)
	client, err := github.NewAt(s.URL, s.Owner, s.Repo, "token")
	if err != nil {
		t.Fatal(err)
	}
	return s, client, origin
}

func TestLiveMergeable(t *testing.T) {
	t.Parallel()
	s, client, origin := newLive(t, "main")
	gittest.Commit(t, origin, "feature", "main", map[string]string{"app.txt": "feature\n"})
	pr := s.OpenPull("feature", "main", "Feature", "")

	got, err := client.PR(t.Context(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mergeable == nil || !*got.Mergeable {
		t.Fatalf("mergeable = %v before main moved, want true", got.Mergeable)
	}

	gittest.Commit(t, origin, "main", "", map[string]string{"app.txt": "main\n"})
	if got, err = client.PR(t.Context(), pr); err != nil {
		t.Fatal(err)
	}
	if got.Mergeable == nil || *got.Mergeable {
		t.Fatalf("mergeable = %v after a conflicting push to main, want false", got.Mergeable)
	}
}

func TestLiveMergePullClosesLinkedIssues(t *testing.T) {
	t.Parallel()
	s, _, origin := newLive(t, "main")
	linked := s.AddIssue("PRD", "", "prd", "status:approved")
	other := s.AddIssue("Other", "", "prd")
	head := gittest.Commit(t, origin, "feature", "main", map[string]string{"feature.txt": "x\n"})
	pr := s.OpenPull("feature", "main", "Feature", "Adds feature.txt.\n\nCloses #"+strconv.Itoa(linked))

	s.MergePull(pr)

	if parents := gittest.Parents(t, origin, "main"); len(parents) != 2 || parents[1] != head {
		t.Errorf("main's parents = %v, want a merge of %s", parents, head)
	}
	if got := gittest.ReadFile(t, origin, "main", "feature.txt"); got != "x" {
		t.Errorf("feature.txt on main = %q", got)
	}
	if i := s.Issue(linked); i.Open || i.StateReason != "completed" || !i.HasLabel("status:approved") {
		t.Errorf("linked issue open=%v reason=%q labels=%v, want closed as completed with its labels", i.Open, i.StateReason, i.Labels)
	}
	if i := s.Issue(other); !i.Open {
		t.Error("an issue the PR does not name was closed")
	}
	if p := s.Pull(pr); p.Open || !p.Merged {
		t.Errorf("PR open=%v merged=%v", p.Open, p.Merged)
	}
}

func TestLiveMergeOffDefaultBranchClosesNothing(t *testing.T) {
	t.Parallel()
	s, _, origin := newLive(t, "trunk")
	linked := s.AddIssue("PRD", "", "prd")
	gittest.Commit(t, origin, "feature", "main", map[string]string{"feature.txt": "x\n"})
	pr := s.OpenPull("feature", "main", "Feature", "Closes #"+strconv.Itoa(linked))

	s.MergePull(pr)

	if !s.Issue(linked).Open {
		t.Error("a merge into a branch other than the default one closed the issue")
	}
	if got := gittest.ReadFile(t, origin, "main", "feature.txt"); got != "x" {
		t.Errorf("feature.txt on main = %q", got)
	}
}

func TestLiveCommitTime(t *testing.T) {
	t.Parallel()
	_, client, origin := newLive(t, "main")
	sha := gittest.Head(origin, "main")
	want := strings.TrimSpace(gittest.Run(t, origin, "show", "-s", "--format=%cI", sha))

	got, err := client.CommitTime(t.Context(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format(time.RFC3339) != want {
		t.Errorf("commit time = %s, want the committer date %s", got.Format(time.RFC3339), want)
	}
}
