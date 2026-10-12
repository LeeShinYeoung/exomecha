package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/git"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// otherRepo is another repository PRDs may change, with a GitHub and an origin of its own.
type otherRepo struct {
	*Repo
	gh     *githubtest.Server
	origin string
}

// addRepo adds owner/<name> to the repositories PRDs may change.
func (e *env) addRepo(name string, files map[string]string) otherRepo {
	e.t.Helper()
	origin := gittest.NewOrigin(e.t, files)
	s := githubtest.New(e.t)
	s.Owner, s.Repo = "owner", name
	s.HeadSHA = func(branch string) string { return gittest.Head(origin, branch) }
	client, err := github.NewAt(s.URL, s.Owner, s.Repo, testToken)
	if err != nil {
		e.t.Fatal(err)
	}
	r := &Repo{Name: "owner/" + name, GitHub: client, Git: git.NewAt(e.t.TempDir(), origin, "main", testToken)}
	e.deps.Repos = append(e.deps.Repos, r)
	e.newRuns()
	return otherRepo{r, s, origin}
}

func TestPRDRepos(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	for _, c := range []struct {
		name, body string
		want       []string
		why        string
	}{
		{"one", "## Repositories\n\n- owner/repo\n\n## Background\n\nwhy\n", []string{"owner/repo"}, ""},
		{"in order, with text and backticks", "## Repositories\n\nThese two:\n\n* `owner/api`\n- Owner/Repo\n\n## To do\n\n- owner/nope\n", []string{"owner/api", "owner/repo"}, ""},
		{"no section", "## Background\n\n- owner/repo\n", nil, "has no `## Repositories` section"},
		{"empty", "## Repositories\n\n## Background\n", nil, "lists no repository"},
		{"not owner/name", "## Repositories\n\n- repo\n", nil, "is not `- owner/name`: `- repo`"},
		{"not one exomecha may change", "## Repositories\n\n- owner/web\n", nil, "lists `owner/web`, which is not one of the repositories exomecha may change (owner/repo, owner/api)"},
		{"twice", "## Repositories\n\n- owner/api\n- owner/API\n", nil, "lists `owner/API` twice"},
	} {
		repos, why := e.deps.prdRepos(github.Issue{Body: c.body})
		var got []string
		for _, r := range repos {
			got = append(got, r.Name)
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") || !strings.Contains(why, c.why) || (c.why == "") != (why == "") {
			t.Errorf("%s: repos %q, why %q, want %q and a reason with %q", c.name, got, why, c.want, c.why)
		}
	}
}

// A PRD that names no repository exomecha may change goes to a person before anything starts.
func TestBuilderStuckOnAWrongRepositoriesSection(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", "## To do\n\nit\n", label.PRD, string(label.Ready))
	e.step(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "has no `## Repositories` section", label.Ready)
	if e.sbx.exists(e.builderBox(prd)) {
		t.Error("a run started")
	}
}

func TestReviewerStuckOnAWrongRepositoriesSection(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", "## Repositories\n\n- owner/web\n", label.PRD, string(label.Review))
	picked, err := e.reviewer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "reviewer Pick", picked, []int{prd})
	if picked, err := e.resolver().Pick(t.Context()); err != nil || len(picked) != 0 {
		t.Errorf("resolver Pick = %v, %v, want nothing", picked, err)
	}
	e.review(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "lists `owner/web`", label.Review)
}

// commitIn returns work that commits files in the sandbox's folder of a repository.
func commitIn(t *testing.T, folder, message string, files map[string]string) func(string) {
	return func(repo string) {
		t.Helper()
		commit(t, message, files)(filepath.Join(repo, folder))
	}
}

func both(works ...func(string)) func(string) {
	return func(repo string) {
		for _, w := range works {
			w(repo)
		}
	}
}

func TestBuilderBuildsAcrossRepositories(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	prd := e.gh.AddIssue("Add feature", reposSection(e.repo, api.Repo)+"do it", label.PRD, string(label.Ready))
	e.step(prd)
	name := e.builderBox(prd)
	e.wantState(name, running)

	// Each repository is in a folder named after it, on the PRD's branch.
	for folder, file := range map[string]string{"repo": "app.txt", "api": "api.txt"} {
		dir := filepath.Join(e.sbx.repo(name), folder)
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Errorf("%s/ in the sandbox: %v", folder, err)
		}
		if got := gittest.Run(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != branchName(prd) {
			t.Errorf("%s/ is on %q, want %s", folder, got, branchName(prd))
		}
	}
	if start := e.sbx.box(name).starts[0]; start[0] != repoDir {
		t.Errorf("claude starts in %s, want %s", start[0], repoDir)
	}
	input := e.readCtl(name, "input.md")
	wantInOrder(t, "input", input, "This PRD changes several repositories.",
		"### `repo/`: owner/repo", "- No PR.", "`git -C repo diff origin/main...HEAD`", "end with `PRD: #1`",
		"### `api/`: owner/api", "`git -C api diff origin/main...HEAD`", "end with `PRD: owner/repo#1`")

	e.play(name, both(
		commitIn(t, "repo", "feat: app", map[string]string{"app.txt": "v2\n"}),
		commitIn(t, "api", "feat: api", map[string]string{"api.txt": "v2\n"}),
	), builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Review)
	e.wantRemoved(name)
	for _, c := range []struct {
		gh     *githubtest.Server
		origin string
		file   string
		body   string
	}{
		{e.gh, e.origin, "app.txt", "Part of #1\n\ndid it"},
		{api.gh, api.origin, "api.txt", "Part of owner/repo#1\n\ndid it"},
	} {
		if got := gittest.ReadFile(t, c.origin, branchName(prd), c.file); got != "v2" {
			t.Errorf("%s on the branch = %q", c.file, got)
		}
		pulls := c.gh.Pulls()
		if len(pulls) != 1 || pulls[0].Body != c.body || pulls[0].Title != "Add feature" {
			t.Fatalf("PRs = %+v, want one with body %q", pulls, c.body)
		}
		if comments := pulls[0].Comments; len(comments) != 1 || !strings.Contains(comments[0].Body, "did it") {
			t.Errorf("PR comments = %+v, want builder's summary", comments)
		}
	}
}

// A repository the PRD lists but the AI did not need to change gets no PR.
func TestBuilderOpensAPROnlyWhereItCommitted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", reposSection(e.repo, api.Repo), label.PRD, string(label.Ready))
	e.step(prd)
	e.play(e.builderBox(prd), commitIn(t, "api", "feat: api", map[string]string{"api.txt": "v2\n"}), builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Review)
	if n := len(e.gh.Pulls()); n != 0 {
		t.Errorf("%d PRs in owner/repo, want none", n)
	}
	if gittest.Head(e.origin, branchName(prd)) != "" {
		t.Error("the branch was pushed to owner/repo")
	}
	if n := len(api.gh.Pulls()); n != 1 {
		t.Errorf("%d PRs in owner/api, want one", n)
	}
}

// The PRD repository can hold only the PRDs, and the setup script still runs.
func TestPRDRepositoryOnlyHoldsThePRDs(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "printf '%s\\n' \"$EXOMECHA_REPOS\" > ../repos.txt\necho ran > "+setupMarker+"\n")
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n", ".gitignore": setupMarker + "\n"})
	e.deps.Repos = []*Repo{api.Repo}
	e.newRuns()
	prd := e.gh.AddIssue("PRD", reposSection(api.Repo), label.PRD, string(label.Ready))
	e.step(prd)
	name := e.builderBox(prd)
	e.wantState(name, running)

	// With one repository, it is the run's whole directory, as before.
	if _, err := os.Stat(filepath.Join(e.sbx.repo(name), "api.txt")); err != nil {
		t.Errorf("api.txt in the sandbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.sbx.repo(name), setupMarker)); err != nil {
		t.Errorf("the setup script did not run: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(e.sbx.disk(name), "repos.txt")); err != nil || string(b) != "owner/api "+repoDir+"\n" {
		t.Errorf("EXOMECHA_REPOS = %q, %v", b, err)
	}
	e.play(name, commit(t, "feat: api", map[string]string{"api.txt": "v2\n"}), builderDone)
	e.step(prd)

	e.wantStatus(prd, label.Review)
	if pulls := api.gh.Pulls(); len(pulls) != 1 || !strings.HasPrefix(pulls[0].Body, "Closes owner/repo#1\n") {
		t.Errorf("PRs = %+v, want one that closes the PRD in owner/repo", pulls)
	}
}

func TestSetupGetsEveryRepository(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "printf '%s\\n' \"$EXOMECHA_REPOS\" > repos.txt\n")
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", reposSection(e.repo, api.Repo), label.PRD, string(label.Ready))
	e.step(prd)
	name := e.builderBox(prd)
	e.wantState(name, running)

	// repos.txt is in the folder holding the repositories, which no git sees.
	want := "owner/repo " + repoDir + "/repo\nowner/api " + repoDir + "/api\n"
	if b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), "repos.txt")); err != nil || string(b) != want {
		t.Errorf("EXOMECHA_REPOS = %q, %v, want %q", b, err, want)
	}
}

// reviewPRDs sets up a PRD in review with a mergeable PR in owner/repo and in owner/api.
func (e *env) reviewPRDs(api otherRepo) (prd, pr, apiPR int) {
	e.t.Helper()
	prd = e.gh.AddIssue("PRD", reposSection(e.repo, api.Repo)+"PRD body", label.PRD, string(label.Review))
	gittest.Commit(e.t, e.origin, branchName(prd), "main", map[string]string{"feature.txt": "x\n"})
	gittest.Commit(e.t, api.origin, branchName(prd), "main", map[string]string{"api.txt": "v2\n"})
	pr = e.gh.AddPull(branchName(prd), "main")
	e.gh.SetMergeable(pr, new(true))
	apiPR = api.gh.AddPull(branchName(prd), "main")
	api.gh.SetMergeable(apiPR, new(true))
	return prd, pr, apiPR
}

func TestReviewerApprovesThePRsTogetherAndClosesThePRDAfterBothMerge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	prd, pr, apiPR := e.reviewPRDs(api)
	e.review(prd)
	name := e.reviewerBox(prd)
	e.wantState(name, running)
	for folder, file := range map[string]string{"repo": "feature.txt", "api": "api.txt"} {
		if _, err := os.Stat(filepath.Join(e.sbx.repo(name), folder, file)); err != nil {
			t.Errorf("%s/%s in the reviewer's sandbox: %v", folder, file, err)
		}
	}
	wantContains(t, "input", e.readCtl(name, "input.md"), "- PR #2, on branch", "- PR owner/api#1, on branch")
	e.play(name, nil, approve)
	e.review(prd)

	e.wantStatus(prd, label.Approved)
	for _, p := range []githubtest.Pull{e.gh.Pull(pr), api.gh.Pull(apiPR)} {
		if len(p.Reviews) != 1 {
			t.Fatalf("PR #%d reviews = %+v, want one approval", p.Number, p.Reviews)
		}
		if n, _, _ := github.ParseNote(p.Reviews[0].Body); n.Kind != github.KindApproved {
			t.Errorf("PR #%d review = %+v, want an approval", p.Number, p.Reviews[0])
		}
	}

	api.gh.MergePull(apiPR)
	if picked, err := e.reviewer().Pick(t.Context()); err != nil || len(picked) != 0 {
		t.Errorf("Pick after one merge = %v, %v, want none while the other PR is open", picked, err)
	}
	e.review(prd)
	if !e.gh.Issue(prd).Open {
		t.Fatal("the PRD closed with one PR still open")
	}
	e.wantStatus(prd, label.Approved)

	e.gh.MergePull(pr)
	e.review(prd)
	if i := e.gh.Issue(prd); i.Open {
		t.Error("the PRD is still open after both PRs merged")
	}
	e.wantStatus(prd, label.None)
}

// reviewer acts on an approved PRD once a PR is closed without a merge, or none is open. Picking it
// sooner would leave it idle every round.
func TestReviewerPicksAnApprovedPRDOnlyWhenItActs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	oneMerged, _, mergedPR := e.reviewPRDs(api)
	oneClosed, _, closedPR := e.reviewPRDs(api)
	for _, prd := range []int{oneMerged, oneClosed} {
		e.gh.Relabel(prd, string(label.Review), string(label.Approved))
	}
	api.gh.MergePull(mergedPR)
	api.gh.ClosePull(closedPR)

	picked, err := e.reviewer().Pick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantInts(t, "Pick", picked, []int{oneClosed})
}

func TestReviewerStuckWhenOneOfThePRsIsClosed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	prd, _, apiPR := e.reviewPRDs(api)
	api.gh.ClosePull(apiPR)
	e.review(prd)

	e.wantStatus(prd, label.Stuck)
	wantStuckComment(t, e.lastComment(prd), "PR owner/api#1 on branch exomecha/prd-1 was closed without being merged.", label.Ready)
}

func TestReviewerSendsBackWhenCIFailsOnOnePR(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	prd, pr, apiPR := e.reviewPRDs(api)
	api.gh.SetRuns(gittest.Head(api.origin, branchName(prd)), githubtest.Run{Name: "test", Status: "completed", Conclusion: "failure"})
	e.review(prd)

	e.wantStatus(prd, label.Ready)
	for _, p := range []githubtest.Pull{e.gh.Pull(pr), api.gh.Pull(apiPR)} {
		if len(p.Reviews) != 1 || !strings.Contains(p.Reviews[0].Body, "- owner/api: test: failure") {
			t.Errorf("PR #%d reviews = %+v, want the failed run of owner/api", p.Number, p.Reviews)
		}
	}
}

func TestRecordShowsAnEntryOnEveryPROnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	prd, pr, apiPR := e.reviewPRDs(api)
	for _, p := range []struct {
		repo *Repo
		pr   int
	}{{e.repo, pr}, {api.Repo, apiPR}} {
		if err := p.repo.GitHub.AddComment(t.Context(), p.pr, github.Note{Worker: "builder", Kind: github.KindSummary}, "built both"); err != nil {
			t.Fatal(err)
		}
	}
	api.gh.AddPullComment(apiPR, "a person's comment on the API")
	got, err := sharedInput(t.Context(), e.deps, e.issue(prd), []repoWithPR{{e.repo, pr}, {api.Repo, apiPR}})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, "built both"); n != 1 {
		t.Errorf("builder's summary is in the record %d times, want once:\n%s", n, got)
	}
	wantContains(t, "record", got, "(builder summary, PR #2)", "(person, PR owner/api#1)\n\na person's comment on the API")
}

// An outsider can copy one of exomecha's entries, marker and all. The copy must not hide the entry.
func TestRecordKeepsAnEntryAnOutsiderCopied(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	prd, pr, _ := e.reviewPRD()
	note := github.Note{Worker: "reviewer", Kind: github.KindChanges}
	e.gh.AddOutsiderReview(pr, github.NoteBody(note, "rename it"))
	if err := e.deps.GitHub.AddReview(t.Context(), pr, note, "rename it"); err != nil {
		t.Fatal(err)
	}
	got, err := sharedInput(t.Context(), e.deps, e.issue(prd), []repoWithPR{{e.repo, pr}})
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "record", got, fmt.Sprintf("(reviewer changes, PR #%d)\n\nrename it", pr))
}

func TestResolverMergesOnlyTheRepositoryThatConflicts(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	api := e.addRepo("api", map[string]string{"api.txt": "v1\n"})
	prd := e.gh.AddIssue("PRD", reposSection(e.repo, api.Repo), label.PRD, string(label.Review))
	gittest.Commit(t, e.origin, branchName(prd), "main", map[string]string{"app.txt": "feature\n"})
	gittest.Commit(t, e.origin, "main", "", map[string]string{"app.txt": "main\n"})
	pr := e.gh.AddPull(branchName(prd), "main")
	e.gh.SetMergeable(pr, new(false))
	apiHead := gittest.Commit(t, api.origin, branchName(prd), "main", map[string]string{"api.txt": "v2\n"})
	apiPR := api.gh.AddPull(branchName(prd), "main")
	api.gh.SetMergeable(apiPR, new(true))

	e.resolve(prd)
	name := e.resolverBox(prd)
	e.wantState(name, running)
	if _, err := os.Stat(filepath.Join(e.sbx.repo(name), "repo", ".git", "MERGE_HEAD")); err != nil {
		t.Errorf("no merge in progress in repo/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.sbx.repo(name), "api", ".git", "MERGE_HEAD")); err == nil {
		t.Error("a merge started in api/, whose PR does not conflict")
	}
	wantContains(t, "input", e.readCtl(name, "input.md"), "## Conflicted files in `repo/` (merging `origin/main`)\n\n- app.txt\n")

	e.play(name, func(repo string) {
		resolveWith(t, "main\nfeature\n")(filepath.Join(repo, "repo"))
		commit(t, "feat: not resolver's", map[string]string{"api.txt": "v3\n"})(filepath.Join(repo, "api"))
	}, resolverDone)
	e.resolve(prd)

	e.wantStatus(prd, label.Review)
	if got := len(gittest.Parents(t, e.origin, branchName(prd))); got != 2 {
		t.Errorf("owner/repo's branch head has %d parents, want the merge", got)
	}
	if got := gittest.Head(api.origin, branchName(prd)); got != apiHead {
		t.Errorf("owner/api's branch moved to %s, want it left at %s", got, apiHead)
	}
	for _, p := range []githubtest.Pull{e.gh.Pull(pr), api.gh.Pull(apiPR)} {
		if len(p.Comments) != 1 || !strings.Contains(p.Comments[0].Body, "kept both lines") {
			t.Errorf("PR #%d comments = %+v, want how resolver resolved", p.Number, p.Comments)
		}
	}
}

func TestProjectRunsGetTheSecretsAsPlaceholders(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "printf '%s' \"$NPM_TOKEN\" > "+setupMarker+"\n")
	secret := sandbox.Secret{Env: "NPM_TOKEN", Hosts: []string{"npm.pkg.github.com"}, Command: "/bin/echo token"}
	e.deps.Secrets = []sandbox.Secret{secret}
	e.newRuns()
	prd, name := e.startedPRD()

	box := e.sbx.box(name)
	if len(box.secrets) != 1 || box.secrets[0].Env != "NPM_TOKEN" {
		t.Errorf("builder's sandbox secrets = %+v, want NPM_TOKEN", box.secrets)
	}
	if b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), setupMarker)); err != nil || string(b) != secret.Placeholder() {
		t.Errorf("setup saw NPM_TOKEN = %q, %v, want the placeholder", b, err)
	}
	if env := box.startEnvs[0]; len(env) != 1 || env[0] != "NPM_TOKEN="+secret.Placeholder() {
		t.Errorf("claude's environment = %q, want the placeholder", env)
	}

	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
	e.step(prd)
	e.gh.SetMergeable(e.gh.Pulls()[0].Number, new(true))
	e.review(prd)
	reviewer := e.sbx.box(e.reviewerBox(prd))
	if len(reviewer.secrets) != 0 || len(reviewer.startEnvs[0]) != 0 {
		t.Errorf("reviewer's sandbox got secrets %+v and environment %q, want none", reviewer.secrets, reviewer.startEnvs[0])
	}
}

func TestProjectRunsGetTheSandboxRules(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "echo ran > "+setupMarker+"\n")
	e.writeSetup(sandboxRules, "Run only the unit tests. Start no containers.\n")
	prd, name := e.startedPRD()
	wantInOrder(t, "builder's input", e.readCtl(name, "input.md"), "## Record", "## Sandbox rules",
		"Follow them where they differ from CLAUDE.md.\n\nRun only the unit tests. Start no containers.\n", "## Setup")

	e.play(name, commit(t, "feat: x", map[string]string{"x.txt": "x\n"}), builderDone)
	e.step(prd)
	e.gh.SetMergeable(e.gh.Pulls()[0].Number, new(true))
	e.review(prd)
	wantNotContains(t, "reviewer's input", e.readCtl(e.reviewerBox(prd), "input.md"), "## Sandbox rules")
}
