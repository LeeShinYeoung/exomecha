package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
	"github.com/LeeShinYeoung/exomecha/internal/template"
)

const builderDone = `{"result": "done", "summary": "did it", "stuck_reason": ""}`

func (e *env) builder() *builder { return &builder{e.deps, e.runs} }

func (e *env) step(n int) {
	e.t.Helper()
	if err := e.builder().Handle(e.t.Context(), n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) builderBox(n int) string { return e.runs.name(builderRole, n) }

// play acts as the AI in a started sandbox: work changes the sandbox's repository, then claude
// ends with the structured result.
func (e *env) play(name string, work func(repo string), result string) {
	e.t.Helper()
	if len(e.sbx.box(name).starts) == 0 {
		e.t.Fatalf("claude was never started in %s", name)
	}
	if work != nil {
		work(e.sbx.repo(name))
	}
	e.writeCtl(name, "stream.jsonl", streamOf(result))
	e.writeCtl(name, "exit", "0\n")
}

// commit returns work that writes files and commits them, the way the AI commits its work.
func commit(t *testing.T, message string, files map[string]string) func(string) {
	return func(repo string) {
		t.Helper()
		for name, content := range files {
			writeFile(t, repo, name, content)
		}
		gittest.CommitAll(t, repo, message)
	}
}

func (e *env) wantRemoved(name string) {
	e.t.Helper()
	if e.sbx.exists(name) {
		e.t.Errorf("sandbox %s still exists", name)
	}
	if _, err := os.Stat(e.runs.dir(name)); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("directory of %s still exists: %v", name, err)
	}
}

// kept returns the directories that keep what the runs of sandbox name printed.
func (e *env) kept(name string) []string {
	e.t.Helper()
	n, role, ok := sandbox.ParseName(e.deps.SandboxPrefix, name)
	if !ok {
		e.t.Fatalf("%s is no run's sandbox", name)
	}
	dirs, err := filepath.Glob(filepath.Join(e.deps.OutputDir, strconv.Itoa(n), "*-"+role))
	if err != nil {
		e.t.Fatal(err)
	}
	return dirs
}

// wantKept checks that the one kept output of the runs of sandbox name holds file, and returns its content.
func (e *env) wantKept(name, file string) string {
	e.t.Helper()
	dirs := e.kept(name)
	if len(dirs) != 1 {
		e.t.Fatalf("kept outputs of %s = %q, want one", name, dirs)
	}
	b, err := os.ReadFile(filepath.Join(dirs[0], file))
	if err != nil {
		e.t.Errorf("the run's %s was not kept: %v", file, err)
	}
	return string(b)
}

// wantAuthor checks that the token owner from Deps made the commit at ref in origin.
func (e *env) wantAuthor(ref string) {
	e.t.Helper()
	want := e.deps.GitName + " <" + e.deps.GitEmail + ">"
	if got := gittest.Run(e.t, e.origin, "log", "-1", "--format=%an <%ae>%n%cn <%ce>", ref); got != want+"\n"+want {
		e.t.Errorf("author and committer of %s = %q, want %q for both", ref, got, want)
	}
}

func TestBuilderPicks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	blocker := e.addPRD("blocker", "", label.PRD, string(label.Ready))
	waiting := e.addPRD("waiting", "", label.PRD, string(label.Ready))
	e.gh.AddBlockedBy(waiting, blocker)
	done := e.addPRD("done", "", label.PRD)
	e.gh.CloseIssue(done)
	free := e.addPRD("free", "", label.PRD, string(label.Ready))
	e.gh.AddBlockedBy(free, done)
	working := e.addPRD("working", "", label.PRD, string(label.Working))
	reviewed := e.addPRD("in review with a leftover sandbox", "", label.PRD, string(label.Review))
	e.addPRD("no status", "", label.PRD)
	e.addPRD("draft", "", label.PRD, string(label.Draft))
	e.addPRD("stuck", "", label.PRD, string(label.Stuck))
	e.sbx.add(e.builderBox(reviewed))
	e.sbx.add(e.runs.name(resolverRole, waiting))

	picked, err := e.builder().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{blocker, free, working, reviewed})
}

func TestBuilderIgnoresOutsiderPRD(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.gh.AddOutsiderIssue("PRD from an outsider", "", label.PRD, string(label.Ready))
	mine := e.addPRD("PRD", "", label.PRD, string(label.Ready))

	picked, err := e.builder().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{mine})
}

func TestBuilderPlace(t *testing.T) {
	t.Parallel()
	status := func(ss ...label.Status) []string {
		out := []string{label.PRD}
		for _, s := range ss {
			out = append(out, string(s))
		}
		return out
	}
	for _, c := range []struct {
		open   bool
		labels []string
		want   place
	}{
		{true, status(label.Ready), ready},
		{true, status(label.Working), working},
		// exomecha moves a status in one call, so two are a person's doing.
		{true, status(label.Ready, label.Working), away},
		{true, status(label.Working, label.Stuck), away},
		{true, status(label.Draft, label.Ready), away},
		{true, status(label.Draft), away},
		{true, status(), away},
		{true, status(label.Review), away},
		{true, status(label.Approved), away},
		{true, status(label.Stuck), away},
		{true, []string{string(label.Working)}, away},
		{false, status(label.Working), away},
	} {
		if got := (&builder{}).place(github.Issue{Open: c.open, Labels: c.labels}); got != c.want {
			t.Errorf("place of an issue open %v with %q = %s, want %s", c.open, c.labels, placeNames[got], placeNames[c.want])
		}
	}
}

func TestBuilderPushesAndOpensPR(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("Add feature", "do it", label.PRD, string(label.Ready))
	name := e.builderBox(prd)
	message := fmt.Sprintf("feat: add feature\n\nPRD: #%d", prd)

	e.step(prd)
	e.wantStatus(prd, label.Working)
	e.wantState(name, running)
	input := e.readCtl(name, "input.md")
	wantContains(t, "claude input", input, "# PRD #", "do it")
	wantNotContains(t, "claude input", input, "## Existing PR", "## Setup")

	e.step(prd)
	e.wantState(name, running)
	e.play(name, commit(t, message, map[string]string{"feature.txt": "hello\n"}), builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Review)
	e.wantRemoved(name)
	branch := branchName(prd)
	if got := gittest.ReadFile(t, e.origin, branch, "feature.txt"); got != "hello" {
		t.Errorf("feature.txt on %s = %q", branch, got)
	}
	if got := gittest.Parents(t, e.origin, branch); len(got) != 1 || got[0] != gittest.Head(e.origin, "main") {
		t.Errorf("%s parents = %v, want the main head", branch, got)
	}
	if got := gittest.Run(t, e.origin, "log", "-1", "--format=%B", branch); got != message {
		t.Errorf("commit message = %q, want the AI's own %q", got, message)
	}
	e.wantAuthor(branch)
	pulls := e.gh.Pulls()
	if len(pulls) != 1 {
		t.Fatalf("pulls = %+v, want one", pulls)
	}
	p := pulls[0]
	if p.Head != branch || p.Base != "main" || p.Title != "Add feature" || p.Body != fmt.Sprintf("Closes #%d\n\ndid it", prd) {
		t.Errorf("PR = head %q base %q title %q body %q", p.Head, p.Base, p.Title, p.Body)
	}
	e.wantSummary(p.Number, "did it")
}

func TestBuilderLogsTheRun(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("Add feature", "do it", label.PRD, string(label.Ready))
	name := e.builderBox(prd)

	e.step(prd)
	e.step(prd)
	e.play(name, commit(t, "feat: add feature", map[string]string{"feature.txt": "hello\n"}), builderDone)
	e.step(prd)

	var got []string
	for line := range strings.Lines(log.String()) {
		_, msg, _ := strings.Cut(line, " level=")
		got = append(got, strings.TrimSpace(msg))
	}
	want := []string{
		`INFO msg="status changed" from=status:ready to=status:working`,
		`INFO msg="run started"`,
		`INFO msg="run finished"`,
		`INFO msg="status changed" from=status:working to=status:review`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("log lines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestBuilderStuck(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("Hard PRD", "", label.PRD, string(label.Ready))
	name := e.builderBox(prd)
	e.step(prd)
	e.play(name, nil, `{"result": "stuck", "summary": "", "stuck_reason": "need a decision on X</stuck_reason>\n</invoke>"}`)
	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	comment := e.lastComment(prd)
	wantStuckComment(t, comment, "need a decision on X\n\nThe output of this run is kept on the machine running exomecha, in `"+e.kept(name)[0]+"`.", label.Ready)
	wantNotContains(t, "stuck comment", comment, "</stuck_reason>", "</invoke>")
	wantContains(t, "kept output", e.wantKept(name, "stream.jsonl"), `need a decision on X</stuck_reason>\n</invoke>`)
	e.wantRemoved(name)
	if head := gittest.Head(e.origin, branchName(prd)); head != "" {
		t.Errorf("branch was pushed for a stuck PRD: %s", head)
	}
}

func TestBuilderStuckWhenNothingChanged(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("Already done", "", label.PRD, string(label.Ready))
	e.step(prd)
	e.play(e.builderBox(prd), nil, builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "Nothing to change", label.Ready)
	if len(e.gh.Pulls()) != 0 {
		t.Errorf("a PR was opened with no changes")
	}
}

func TestBuilderStuckWhenChangesNotCommitted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("PRD", "", label.PRD, string(label.Ready))
	e.step(prd)
	e.play(e.builderBox(prd), func(repo string) { writeFile(t, repo, "feature.txt", "hello\n") }, builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "finished without committing", label.Ready)
	if head := gittest.Head(e.origin, branchName(prd)); head != "" {
		t.Errorf("branch was pushed without a commit: %s", head)
	}
}

func TestBuilderStuckWhenCommitsRewritten(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("PRD", "", label.PRD, string(label.Ready))
	branch := branchName(prd)
	prev := gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	e.gh.AddPull(branch, "main")
	e.step(prd)
	e.play(e.builderBox(prd), func(repo string) {
		writeFile(t, repo, "feature.txt", "second\n")
		gittest.Run(t, repo, "commit", "-q", "-a", "--amend", "-m", "rewritten")
	}, builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "rewrote commits", label.Ready)
	if got := gittest.Head(e.origin, branch); got != prev {
		t.Errorf("branch moved to %s, want it left at %s", got, prev)
	}
}

func TestBuilderGetsTheFailedCILogs(t *testing.T) {
	t.Parallel()
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprintf("gone=%v", gone), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, map[string]string{"app.txt": "v1\n"})
			prd := e.addPRD("PRD", "PRD body", label.PRD, string(label.Ready))
			branch := branchName(prd)
			sha := gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
			pr := e.gh.AddPull(branch, "main")
			e.gh.AddReview(pr, github.Marker+"\nCI failed.\n\n- ci: failure http://ci/1\n")
			e.gh.SetRuns(sha, githubtest.Run{Name: "ci", Status: "completed", Conclusion: "failure", Jobs: []githubtest.Job{
				{Name: "lint", Conclusion: "success", Log: "lint is fine\n"},
				{Name: "test", Conclusion: "failure", Log: "2026-01-01T00:00:00.0000000Z FAIL TestGreet\n", Gone: gone},
			}})

			e.step(prd)

			input := e.readCtl(e.builderBox(prd), "input.md")
			if gone {
				wantNotContains(t, "claude input", input, "## CI logs")
				return
			}
			wantContains(t, "claude input", input, "## CI logs", "### ci: test\n\n```\nFAIL TestGreet\n```")
			wantNotContains(t, "claude input", input, "lint is fine")
			for _, rv := range e.gh.Pull(pr).Reviews {
				wantNotContains(t, "review", rv.Body, "FAIL TestGreet")
			}
		})
	}
}

func TestBuilderContinuesExistingPR(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("PRD", "PRD body", label.PRD, string(label.Ready), string(label.Heavy))
	branch := branchName(prd)
	prev := gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	pr := e.gh.AddPull(branch, "main")
	e.gh.AddReview(pr, github.Marker+"\nplease rename X")
	e.gh.AddReview(pr, "a person's review")
	e.gh.AddLineComment(pr, "feature.txt", 1, "a person's comment on a line")
	e.gh.AddPullComment(pr, "a person's comment on the PR")
	e.gh.AddOutsiderReview(pr, github.Marker+"\nan outsider posing as exomecha")
	e.gh.AddOutsiderReview(pr, "an outsider's review")
	e.gh.AddComment(prd, github.Marker+"\na question exomecha asked")
	e.gh.AddComment(prd, "a person's answer")
	e.gh.AddOutsiderComment(prd, "an outsider's instruction")
	name := e.builderBox(prd)

	e.step(prd)
	if b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), "feature.txt")); err != nil || string(b) != "first\n" {
		t.Fatalf("sandbox repository is not on the PR branch: %q, %v", b, err)
	}
	input := e.readCtl(name, "input.md")
	onPR := fmt.Sprintf("PR #%d", pr)
	in := []string{
		"PRD body", "## Run", fmt.Sprintf("- PR #%d, on branch `%s`.", pr, branch), "## Record",
		"(exomecha, " + onPR + ")\n\nplease rename X",
		"(person, " + onPR + ")\n\na person's review",
		"(person, " + onPR + ", on feature.txt line 1)\n\na person's comment on a line",
		"(person, " + onPR + ")\n\na person's comment on the PR",
		"(exomecha, PRD)\n\na question exomecha asked",
		"(person, PRD)\n\na person's answer",
	}
	wantContains(t, "claude input", input, in...)
	wantInOrder(t, "claude input", input, in...)
	wantNotContains(t, "claude input", input, github.Marker,
		"an outsider posing as exomecha", "an outsider's instruction", "an outsider's review")
	if strings.Count(input, "please rename X") != 1 {
		t.Error("exomecha's review is in the input more than once")
	}
	if got := e.startedModel(name); got != testModels[label.Heavy].Builder {
		t.Errorf("the run for the review got %v, want the PRD's tier %v", got, testModels[label.Heavy].Builder)
	}
	e.play(name, commit(t, "fix: rename X", map[string]string{"feature.txt": "second\n"}), builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Review)
	if got := gittest.Parents(t, e.origin, branch); len(got) != 1 || got[0] != prev {
		t.Errorf("%s parents = %v, want the previous PR head %s", branch, got, prev)
	}
	if n := len(e.gh.Pulls()); n != 1 {
		t.Errorf("%d PRs, want the existing one only", n)
	}
	e.wantSummary(pr, "did it")
}

// wantSummary checks that builder's last run left its summary on the PR, where the next run reads it.
func (e *env) wantSummary(pr int, summary string) {
	e.t.Helper()
	comments := e.gh.Pull(pr).Comments
	want := github.NoteBody(github.Note{Worker: "builder", Kind: github.KindSummary}, summary)
	if len(comments) == 0 || comments[len(comments)-1].Body != want {
		e.t.Errorf("PR comments = %+v, want the last one to be builder's summary %q", comments, summary)
	}
}

// A PRD Blocked by another starts once that one merges, so a start must see main as it is now.
func TestBuilderStartsFromTheLatestMain(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	first := e.addPRD("First", "", label.PRD, string(label.Ready))
	second := e.addPRD("Second", "", label.PRD, string(label.Ready))
	e.step(first)
	moved := gittest.Commit(t, e.origin, "main", "", map[string]string{"app.txt": "v2\n"})

	e.step(second)

	if got := gittest.Run(t, e.sbx.repo(e.builderBox(second)), "rev-parse", "HEAD"); got != moved {
		t.Errorf("the second run starts at %s, want main after it moved, %s", got, moved)
	}
}

// A person closed the PR without merging and sent the PRD back to ready. Its branch is still there.
func TestBuilderStartsOverAfterAClosedPR(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("PRD", "", label.PRD, string(label.Ready))
	branch := branchName(prd)
	gittest.Commit(t, e.origin, branch, "main", map[string]string{"old.txt": "old\n"})
	old := e.gh.AddPull(branch, "main")
	e.gh.ClosePull(old)
	name := e.builderBox(prd)
	base := gittest.Head(e.origin, "main")

	e.step(prd)
	wantNotContains(t, "claude input", e.readCtl(name, "input.md"), "## Existing PR")
	if got := gittest.Run(t, e.sbx.repo(name), "rev-parse", "HEAD"); got != base {
		t.Errorf("the run starts at %s, want main %s", got, base)
	}
	e.play(name, commit(t, "feat: start over", map[string]string{"new.txt": "new\n"}), builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Review)
	if got := gittest.Parents(t, e.origin, branch); len(got) != 1 || got[0] != base {
		t.Errorf("%s parents = %v, want main %s", branch, got, base)
	}
	pulls := e.gh.Pulls()
	if len(pulls) != 2 || pulls[0].Open || !pulls[1].Open || pulls[1].Head != branch {
		t.Errorf("pulls = %+v, want the closed one and a new open one", pulls)
	}
}

// A person sent an approved PRD back with a review on the PR. exomecha's approval is no change to
// make.
// The record names each of exomecha's entries by its worker and kind, so builder can tell what was
// asked and settled before a person's latest request.
func TestBuilderReadsWhoWroteEachEntry(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("PRD", "PRD body", label.PRD, string(label.Ready))
	branch := branchName(prd)
	gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	pr := e.gh.AddPull(branch, "main")
	gh := e.deps.GitHub
	if err := gh.AddComment(t.Context(), pr, github.Note{Worker: "builder", Kind: github.KindSummary}, "added X"); err != nil {
		t.Fatal(err)
	}
	if err := gh.AddReview(t.Context(), pr, github.Note{Worker: "reviewer", Kind: github.KindChanges}, "please rename X"); err != nil {
		t.Fatal(err)
	}
	if err := gh.AddApproval(t.Context(), pr, "renames X"); err != nil {
		t.Fatal(err)
	}
	e.gh.AddReview(pr, "use a shorter name")

	e.step(prd)

	input := e.readCtl(e.builderBox(prd), "input.md")
	onPR := fmt.Sprintf("PR #%d", pr)
	in := []string{
		"(builder summary, " + onPR + ")\n\nadded X",
		"(reviewer changes, " + onPR + ")\n\nplease rename X",
		"(reviewer approved, " + onPR + ")\n\nexomecha's reviewer approved this PR.",
		"(person, " + onPR + ")\n\nuse a shorter name",
	}
	wantContains(t, "claude input", input, in...)
	wantInOrder(t, "claude input", input, in...)
	wantNotContains(t, "claude input", input, "**exomecha")
}

func TestBuilderRunsTheTasksTier(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		labels []string
		want   label.Tier
	}{
		{nil, label.Normal},
		{[]string{string(label.Light)}, label.Light},
		{[]string{string(label.Light), string(label.Heavy)}, label.Heavy},
	} {
		e := newEnv(t, map[string]string{"app.txt": "v1\n"})
		prd := e.addPRD("PRD", "", append([]string{label.PRD, string(label.Ready)}, c.labels...)...)
		e.step(prd)
		if got := e.startedModel(e.builderBox(prd)); got != testModels[c.want].Builder {
			t.Errorf("PRD with %q got %v, want %v", c.labels, got, testModels[c.want].Builder)
		}
	}
}

func TestBuilderRequeuesOnStartError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("PRD", "", label.PRD, string(label.Ready))
	e.sbx.fail["start"] = errors.New("sbx exec: daemon not running")

	if err := e.builder().Handle(t.Context(), prd); err == nil {
		t.Fatal("Handle returned nil for an sbx error")
	}

	e.wantStatus(prd, label.Ready)
	e.wantRemoved(e.builderBox(prd))
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("an sbx error left %d comments", n)
	}
}

func TestBuilderEnvironmentNeedsAPerson(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("PRD", "", label.PRD, string(label.Ready))
	e.envr.err = fmt.Errorf("build template: %w", personError{why: "The Dockerfile does not build.", detail: "step 3/5: a secret in the build output"})

	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	comment := e.lastComment(prd)
	wantStuckComment(t, comment, "The Dockerfile does not build.", label.Ready)
	wantNotContains(t, "stuck comment", comment, "a secret in the build output", "step 3/5")
}

// The environment main.go wires in. A bad allowed-hosts line fails before any sbx call.
func TestBuilderTemplateProblemGoesToStuck(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.writeSetup("allowed-hosts", "api.example.com\n**\n")
	e.deps.Environment = template.New(e.deps.SetupDir, nil, "owner", "repo")
	e.newRuns()
	prd := e.addPRD("PRD", "", label.PRD, string(label.Ready))

	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	reason := "allowed-hosts` line 2 (`**`) allows every host. List only the hosts the project needs. Fix it, or make it again with /exomecha:setup."
	wantStuckComment(t, e.lastComment(prd), reason, label.Ready)
	e.wantRemoved(e.builderBox(prd))
}

func (e *env) startedPRD() (int, string) {
	e.t.Helper()
	prd := e.addPRD("PRD", "", label.PRD, string(label.Ready))
	e.step(prd)
	name := e.builderBox(prd)
	e.wantState(name, running)
	return prd, name
}

func TestBuilderRunNotInAutoModeFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.startedPRD()
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
	e.writeCtl(name, "stream.jsonl", strings.Replace(streamOf(builderDone), `"auto"`, `"bypassPermissions"`, 1))

	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "claude did not run in auto permission mode", label.Ready)
	if head := gittest.Head(e.origin, branchName(prd)); head != "" {
		t.Errorf("work from a run outside auto mode was pushed: %s", head)
	}
}

func TestBuilderFailedRunGoesStuck(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.startedPRD()
	e.writeCtl(name, "stream.jsonl", `{"type":"system","subtype":"init","permissionMode":"auto"}`+"\n"+
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"secret detail"}`+"\n")
	e.writeCtl(name, "exit", "1\n")

	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	comment := e.lastComment(prd)
	wantStuckComment(t, comment, "The AI's run failed because claude ended with an error", label.Ready)
	wantNotContains(t, "stuck comment", comment, "secret detail")
	e.wantRemoved(name)
	wantContains(t, "kept output", e.wantKept(name, "stream.jsonl"), "secret detail")
}

// The AI can write the files a run is read from, so a run exomecha cannot read goes to a person,
// and the comment says exomecha could not read it instead of blaming the AI.
func TestBuilderStuckWhenTheRunCannotBeRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.startedPRD()
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
	e.writeCtl(name, "exit", "not a number\n")

	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	comment := e.lastComment(prd)
	wantStuckComment(t, comment, "exomecha could not read the run's result: the file that records how claude ended holds no exit status.", label.Ready)
	wantNotContains(t, "stuck comment", comment, "The AI's run failed")
}

// A PRD can change several repositories, so a failed push says which one.
func TestPushErrorsNameTheRepository(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		worker  string
		arrange func(e *env) int
		handle  func(e *env, n int) error
	}{
		{"builder", func(e *env) int {
			prd, name := e.startedPRD()
			e.play(name, commit(e.t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
			return prd
		}, func(e *env, n int) error { return e.builder().Handle(e.t.Context(), n) }},
		{"resolver", func(e *env) int {
			prd, name, _ := e.conflicted()
			e.play(name, resolveWith(e.t, "main\nfeature\n"), resolverDone)
			return prd
		}, func(e *env, n int) error { return e.resolver().Handle(e.t.Context(), n) }},
	} {
		t.Run(c.worker, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, map[string]string{"app.txt": "v1\n"})
			n := c.arrange(e)
			e.refusePushes(true)

			err := c.handle(e, n)
			if err == nil || !strings.HasPrefix(err.Error(), e.repo.Name+": ") {
				t.Errorf("Handle = %v, want the push error to start with %s", err, e.repo.Name)
			}
		})
	}
}

func TestBuilderResultInAnotherFormFails(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, result string }{
		{"unknown result", `{"result": "maybe", "summary": "", "stuck_reason": ""}`},
		{"not an object", `["done"]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, map[string]string{"app.txt": "v1\n"})
			prd, name := e.startedPRD()
			e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), c.result)

			e.step(prd)

			e.wantStatus(prd, label.Stuck)
			wantStuckComment(t, e.lastComment(prd), "The AI's run failed because "+badResult, label.Ready)
			e.wantKept(name, "stream.jsonl")
			if head := gittest.Head(e.origin, branchName(prd)); head != "" {
				t.Errorf("work from a result in another form was pushed: %s", head)
			}
		})
	}
}

// The reading the start passed missed the limit the run hit. The next start reads usage again and
// waits for the reset, instead of starting a run that would hit the limit too.
func TestBuilderUsageLimitEndStartsOverAfterTheReset(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	f := newFakeUsage(e.clock)
	e.runs.gate = f.gate
	prd, name := e.startedPRD()
	e.writeCtl(name, "stream.jsonl", `{"type":"system","subtype":"init","permissionMode":"auto"}`+"\n"+
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`+"\n"+
		`{"type":"result","subtype":"success","is_error":true,"result":"You've hit your limit"}`+"\n")
	e.writeCtl(name, "exit", "1\n")
	f.usage.FiveHour.Used = 1

	e.step(prd)

	e.wantStatus(prd, label.Ready)
	e.wantRemoved(name)
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("a usage limit end left %d comments", n)
	}
	e.step(prd)
	f.wantChecks(t, 2)
	if e.sbx.exists(name) {
		t.Fatal("started again over the limit")
	}

	f.usage.FiveHour = agent.Window{Used: 0, ResetsAt: e.clock.now().Add(5 * time.Hour)}
	e.clock.advance(2 * time.Hour)
	e.step(prd)
	e.wantState(name, running)
}

func TestBuilderWaitsForAFreeSlot(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ProjectLimit = 1
	e.sbx.add(e.runs.name(resolverRole, 99))
	prd := e.addPRD("PRD", "", label.PRD, string(label.Ready))

	e.step(prd)

	e.wantStatus(prd, label.Ready)
	if e.sbx.exists(e.builderBox(prd)) {
		t.Error("started a sandbox over the limit")
	}
}

// A template build takes minutes before the sandbox exists. The start must hold its slot all along.
func TestBuilderHoldsItsSlotWhileTheTemplateBuilds(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ProjectLimit = 1
	e.envr.wait = make(chan chan struct{})
	first := e.addPRD("First", "", label.PRD, string(label.Ready))
	second := e.addPRD("Second", "", label.PRD, string(label.Ready))
	done := make(chan error, 1)
	go func() { done <- e.builder().Handle(t.Context(), first) }()
	var built chan struct{}
	select {
	case built = <-e.envr.wait:
	case err := <-done:
		t.Fatalf("the first start ended before its template build: %v", err)
	}

	// A second start that got past the check would wait on the build too.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := e.builder().Handle(ctx, second); err != nil {
		t.Errorf("the second start: %v", err)
	}

	e.wantStatus(second, label.Ready)
	if e.sbx.exists(e.builderBox(second)) {
		t.Error("started a sandbox over the limit")
	}
	close(built)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.wantStatus(first, label.Working)
	e.wantState(e.builderBox(first), running)
}

// Over the usage limit, nothing new starts, and a run already going is still finished.
func TestBuilderHarvestsWhileTheGateIsClosed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	running, name := e.startedPRD()
	f := newFakeUsage(e.clock)
	f.usage.FiveHour.Used = 0.99
	e.runs.gate = f.gate
	waiting := e.addPRD("Waiting", "", label.PRD, string(label.Ready))

	e.step(waiting)
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
	e.step(running)

	e.wantStatus(waiting, label.Ready)
	if e.sbx.exists(e.builderBox(waiting)) {
		t.Error("a run started while the gate was closed")
	}
	e.wantStatus(running, label.Review)
}

// A new builder remembers nothing, and still finishes a run a stopped exomecha left behind.
func TestBuilderFinishesARunAfterRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.startedPRD()
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)

	e.newRuns()
	picked, err := e.builder().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{prd})
	e.step(prd)

	e.wantStatus(prd, label.Review)
	e.wantRemoved(name)
}

func TestBuilderStartsOverWhenTheBranchChangedDuringTheRun(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.addPRD("PRD", "", label.PRD, string(label.Ready))
	branch := branchName(prd)
	gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	e.gh.AddPull(branch, "main")
	e.step(prd)
	name := e.builderBox(prd)
	pushed := gittest.Commit(t, e.origin, branch, "", map[string]string{"feature.txt": "a person's fix\n"})
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)

	e.step(prd)

	e.wantStatus(prd, label.Ready)
	e.wantRemoved(name)
	if got := gittest.Head(e.origin, branch); got != pushed {
		t.Errorf("branch = %s, want the person's push %s left alone", got, pushed)
	}
	if n := len(e.gh.Issue(prd).Comments); n != 0 {
		t.Errorf("a branch change left %d comments", n)
	}
}

// After a push, a later step can fail. The next try must not take exomecha's own push for a change on GitHub.
func TestBuilderRetriesAfterItsOwnPush(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.startedPRD()
	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
	head := gittest.Run(t, e.sbx.repo(name), "rev-parse", "HEAD")
	if err := e.git.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.git.FetchFrom(t.Context(), e.sbx.repo(name), name); err != nil {
		t.Fatal(err)
	}
	if err := e.git.PushCommit(t.Context(), head, branchName(prd), true); err != nil {
		t.Fatal(err)
	}

	e.step(prd)

	e.wantStatus(prd, label.Review)
	if n := len(e.gh.Pulls()); n != 1 {
		t.Errorf("%d PRs, want one", n)
	}
}

func TestBuilderStuckWhenNoCommitIsCheckedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, name := e.startedPRD()
	e.play(name, func(repo string) { gittest.Run(t, repo, "checkout", "-q", "--orphan", "empty") }, builderDone)

	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "no commit checked out", label.Ready)
	e.wantRemoved(name)
}
