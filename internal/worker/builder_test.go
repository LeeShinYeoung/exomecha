package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/template"
)

const builderDone = `{"result": "done", "summary": "did it", "stuck_reason": ""}`

func (e *env) builder() *builder { return &builder{e.deps, e.runs} }

// step runs one builder round for the PRD and fails the test on an error.
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

// wantRemoved checks that the sandbox and its directory are gone.
func (e *env) wantRemoved(name string) {
	e.t.Helper()
	if e.sbx.exists(name) {
		e.t.Errorf("sandbox %s still exists", name)
	}
	if _, err := os.Stat(e.runs.dir(name)); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("directory of %s still exists: %v", name, err)
	}
}

// wantKept checks that the failed run's file was kept for a person to read.
func (e *env) wantKept(name, file string) {
	e.t.Helper()
	if _, err := os.Stat(filepath.Join(e.deps.SandboxDir, "failed", name, file)); err != nil {
		e.t.Errorf("the failed run's %s was not kept: %v", file, err)
	}
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
	blocker := e.gh.AddIssue("blocker", "", label.PRD, string(label.Ready))
	waiting := e.gh.AddIssue("waiting", "", label.PRD, string(label.Ready))
	e.gh.AddBlockedBy(waiting, blocker)
	done := e.gh.AddIssue("done", "", label.PRD)
	e.gh.CloseIssue(done)
	free := e.gh.AddIssue("free", "", label.PRD, string(label.Ready))
	e.gh.AddBlockedBy(free, done)
	working := e.gh.AddIssue("working", "", label.PRD, string(label.Working))
	reviewed := e.gh.AddIssue("in review with a leftover sandbox", "", label.PRD, string(label.Review))
	e.gh.AddIssue("no status", "", label.PRD)
	e.gh.AddIssue("draft", "", label.PRD, string(label.Draft))
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
	mine := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))

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
	prd := e.gh.AddIssue("Add feature", "do it", label.PRD, string(label.Ready))
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
}

func TestBuilderStuck(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("Hard PRD", "", label.PRD, string(label.Ready))
	name := e.builderBox(prd)
	e.step(prd)
	e.play(name, nil, `{"result": "stuck", "summary": "", "stuck_reason": "need a decision on X</stuck_reason>\n</invoke>"}`)
	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	comment := e.lastComment(prd)
	wantStuckComment(t, comment, "need a decision on X\n\nIf there is nothing left to do", label.Ready)
	wantNotContains(t, "stuck comment", comment, "</stuck_reason>", "</invoke>")
	wantContains(t, "log", log.String(), `need a decision on X</stuck_reason>\n</invoke>`)
	e.wantRemoved(name)
	if head := gittest.Head(e.origin, branchName(prd)); head != "" {
		t.Errorf("branch was pushed for a stuck PRD: %s", head)
	}
}

func TestBuilderStuckWhenNothingChanged(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("Already done", "", label.PRD, string(label.Ready))
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
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
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
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
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

func TestBuilderContinuesExistingPR(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", "PRD body", label.PRD, string(label.Ready), string(label.Heavy))
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
	wantContains(t, "claude input", input, "PRD body", "## Existing PR", "## Last review", "please rename X",
		"(exomecha)\n\na question exomecha asked", "(person)\n\na person's answer",
		"## Comments on the PR", "(person)\n\na person's review", "(person, on feature.txt line 1)\n\na person's comment on a line",
		"(person)\n\na person's comment on the PR")
	wantNotContains(t, "claude input", input, github.Marker,
		"an outsider posing as exomecha", "an outsider's instruction", "an outsider's review")
	if strings.Count(input, "please rename X") != 1 {
		t.Error("exomecha's review is in the input more than once")
	}
	if got := e.startedModel(name); got != testModels[label.Heavy] {
		t.Errorf("the run for the review got %v, want the PRD's tier %v", got, testModels[label.Heavy])
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
}

// A person sent an approved PRD back with a review on the PR. exomecha's approval is no change to
// make.
func TestBuilderLeavesOutTheApproval(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", "PRD body", label.PRD, string(label.Ready))
	branch := branchName(prd)
	gittest.Commit(t, e.origin, branch, "main", map[string]string{"feature.txt": "first\n"})
	pr := e.gh.AddPull(branch, "main")
	e.gh.AddReview(pr, github.Marker+"\nplease rename X")
	if err := e.deps.GitHub.AddApproval(t.Context(), pr, "renames X"); err != nil {
		t.Fatal(err)
	}
	e.gh.AddReview(pr, "use a shorter name")

	e.step(prd)

	input := e.readCtl(e.builderBox(prd), "input.md")
	wantContains(t, "claude input", input, "(person)\n\nuse a shorter name")
	wantNotContains(t, "claude input", input, "## Last review", "please rename X", "renames X")
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
		prd := e.gh.AddIssue("PRD", "", append([]string{label.PRD, string(label.Ready)}, c.labels...)...)
		e.step(prd)
		if got := e.startedModel(e.builderBox(prd)); got != testModels[c.want] {
			t.Errorf("PRD with %q got %v, want %v", c.labels, got, testModels[c.want])
		}
	}
}

func TestBuilderRequeuesOnStartError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
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
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
	e.envr.err = fmt.Errorf("build template: %w", personError{why: "The Dockerfile on the base branch does not build.", detail: "step 3/5: a secret in the build output"})

	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	comment := e.lastComment(prd)
	wantStuckComment(t, comment, "The Dockerfile on the base branch does not build.", label.Ready)
	wantNotContains(t, "stuck comment", comment, "a secret in the build output", "step 3/5")
}

// The environment main.go wires in. A bad allowed-hosts line fails before any sbx call.
func TestBuilderTemplateProblemGoesToStuck(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n", ".exomecha/allowed-hosts": "api.example.com\n**\n"})
	e.deps.Environment = template.New(e.deps.Git, nil, "owner", "repo")
	e.runs = newRuns(e.deps, nil)
	e.runs.now = e.clock.now
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))

	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	reason := "`.exomecha/allowed-hosts` line 2 (`**`) allows every host. List only the hosts the project needs. Fix it on the base branch."
	wantStuckComment(t, e.lastComment(prd), reason, label.Ready)
	e.wantRemoved(e.builderBox(prd))
}

// startedPRD starts a builder run for a new ready PRD and returns the PRD and its sandbox.
func (e *env) startedPRD() (int, string) {
	e.t.Helper()
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
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
	log := captureLog(t)
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
	wantContains(t, "log", log.String(), "secret detail")
	e.wantRemoved(name)
	e.wantKept(name, "stream.jsonl")
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
	f := newFakeUsage()
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

	f.usage.FiveHour = agent.Window{Used: 0, ResetsAt: f.clock.Add(5 * time.Hour)}
	f.clock = f.clock.Add(2 * time.Hour)
	e.step(prd)
	e.wantState(name, running)
}

func TestBuilderWaitsForAFreeSlot(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ProjectLimit = 1
	e.sbx.add(e.runs.name(resolverRole, 99))
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))

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
	first := e.gh.AddIssue("First", "", label.PRD, string(label.Ready))
	second := e.gh.AddIssue("Second", "", label.PRD, string(label.Ready))
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
	e.runs.gate = &Gate{
		Check: func(context.Context) (agent.Usage, error) {
			return agent.Usage{Known: true, FiveHour: agent.Window{Used: 0.99}}, nil
		},
		FiveHour: 0.9,
		Weekly:   0.8,
	}
	waiting := e.gh.AddIssue("Waiting", "", label.PRD, string(label.Ready))

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

	e.runs = newRuns(e.deps, nil)
	e.runs.now = e.clock.now
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
	prd := e.gh.AddIssue("PRD", "", label.PRD, string(label.Ready))
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
	if err := e.deps.Git.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deps.Git.FetchFrom(t.Context(), e.sbx.repo(name), name); err != nil {
		t.Fatal(err)
	}
	if err := e.deps.Git.PushCommit(t.Context(), head, branchName(prd), true); err != nil {
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
