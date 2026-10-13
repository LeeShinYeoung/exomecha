package worker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

func (e *env) rev(ref string) string {
	e.t.Helper()
	commit, ok, err := e.git.Rev(e.t.Context(), ref)
	if err != nil || !ok {
		e.t.Fatalf("Rev(%s) = %q, %v, %v", ref, commit, ok, err)
	}
	return commit
}

// testSpec is a builder run for PRD 1 on a new branch off main, and the name of its sandbox.
func (e *env) testSpec() (spec, string) {
	e.t.Helper()
	if err := e.git.Sync(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
	base := e.rev("origin/main")
	return spec{repos: []repoSpec{{repo: e.repo, branch: branchName(1), base: base, head: base}}, input: "the PRD\n\n", tier: label.Normal}, e.runs.name(builderRole, 1)
}

func (e *env) open(s spec) *problem {
	e.t.Helper()
	p, err := e.runs.open(e.t.Context(), builderRole, 1, s)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) startedModel(name string) agent.Model {
	e.t.Helper()
	starts := e.sbx.box(name).starts
	if len(starts) == 0 {
		e.t.Fatalf("%s started no claude", name)
	}
	arg := func(flag string) string {
		i := slices.Index(starts[0], flag)
		if i < 0 || i+1 >= len(starts[0]) {
			return ""
		}
		return starts[0][i+1]
	}
	return agent.Model{Name: arg("--model"), Effort: arg("--effort")}
}

func (e *env) readCtl(name, file string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.runs.ctl(name), file))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func TestOpenStartsClaudeInASandbox(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.envr.ref = "docker.io/library/exomecha-owner-repo:0123456789ab"
	e.envr.hosts = []string{"registry.npmjs.org", "*.pypi.org"}
	s, name := e.testSpec()

	if p := e.open(s); p != nil {
		t.Fatalf("open problem: %s", p.why)
	}

	box := e.sbx.box(name)
	if box.template != e.envr.ref || box.workspace != e.runs.ctl(name) {
		t.Errorf("created with template %q, workspace %q", box.template, box.workspace)
	}
	if !slices.Equal(box.deny, []string{"ssh.github.com"}) || !slices.Equal(box.allowed, e.envr.hosts) {
		t.Errorf("network: deny %q, allowed %q", box.deny, box.allowed)
	}
	repo := e.sbx.repo(name)
	if got := gittest.Run(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != s.repos[0].branch {
		t.Errorf("sandbox repository is on %q, want %s", got, s.repos[0].branch)
	}
	if got := gittest.Run(t, repo, "rev-parse", "origin/main"); got != s.repos[0].base {
		t.Errorf("origin/main in the sandbox = %s, want %s", got, s.repos[0].base)
	}
	if got := gittest.Run(t, repo, "remote"); got != "" {
		t.Errorf("sandbox repository has remotes: %q", got)
	}
	for key, want := range map[string]string{"user.name": e.deps.GitName, "user.email": e.deps.GitEmail} {
		if got := gittest.Run(t, repo, "config", "--local", key); got != want {
			t.Errorf("%s of the sandbox repository = %q, want %q", key, got, want)
		}
	}

	if len(box.starts) != 1 {
		t.Fatalf("starts = %q, want one", box.starts)
	}
	ctl := e.runs.ctl(name)
	start := box.starts[0]
	if !slices.Equal(start[:5], []string{repoDir, "sh", filepath.Join(ctl, "run.sh"), ctl, "claude"}) {
		t.Errorf("start = %q", start)
	}
	for flag, want := range map[string]string{"--permission-mode": "auto", "--append-system-prompt": prompt("builder"), "--json-schema": builderSchema,
		"--model": "normal-builder", "--effort": "medium"} {
		if i := slices.Index(start, flag); i < 0 || start[i+1] != want {
			t.Errorf("claude does not get %s %.40q: %.200q", flag, want, start)
		}
	}
	for _, a := range start {
		if strings.Contains(a, "dangerously") || strings.Contains(a, "bypass") {
			t.Errorf("start args contain %q", a)
		}
	}
	if got := e.readCtl(name, "input.md"); got != s.input {
		t.Errorf("input.md = %q", got)
	}
	if got := e.readCtl(name, "run.sh"); got != runScript {
		t.Errorf("run.sh = %q", got)
	}
	r, err := e.runs.read(t.Context(), builderRole, 1)
	if err != nil || r.state != running || r.in.Repos[0].Head != s.repos[0].head {
		t.Errorf("after open: %+v, %v", r, err)
	}
}

// The token can push past every check exomecha makes, so nothing a run can read may hold it.
func TestRunsNeverSeeTheToken(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "echo ran > "+setupMarker+"\n")
	s, builder := e.testSpec()
	if p := e.open(s); p != nil {
		t.Fatalf("open problem: %s", p.why)
	}
	reader := e.runs.name(reviewerRole, 1)
	s.repos[0].branch = ""
	if p, err := e.runs.open(t.Context(), reviewerRole, 1, s); p != nil || err != nil {
		t.Fatalf("reviewer open = %+v, %v", p, err)
	}

	holds := func(ss []string) bool {
		return slices.ContainsFunc(ss, func(s string) bool { return strings.Contains(s, testToken) })
	}
	for _, name := range []string{builder, reader} {
		box := e.sbx.box(name)
		for _, args := range slices.Concat(box.execs, box.envs, box.starts) {
			if holds(args) {
				t.Errorf("%s got the token in %q", name, args)
			}
		}
		// The ctl directory is the sandbox's workspace.
		err := filepath.WalkDir(e.runs.ctl(name), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(b), testToken) {
				t.Errorf("%s holds the token", path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), ".git", "config")); err != nil || strings.Contains(string(b), testToken) {
			t.Errorf("git config of %s: %v, holds the token %v", name, err, strings.Contains(string(b), testToken))
		}
	}
}

// A reader's run does not run the project, so none of the project's environment goes in.
func TestOpenReaderGetsOnlyTheCode(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "echo ran > "+setupMarker+"\n")
	e.envr.err = errors.New("read the project's environment")
	e.sbx.fail["allow"] = errors.New("allow hosts")
	s, _ := e.testSpec()
	s.repos[0].branch = ""
	name := e.runs.name(reviewerRole, 1)

	if p, err := e.runs.open(t.Context(), reviewerRole, 1, s); p != nil || err != nil {
		t.Fatalf("open = %+v, %v", p, err)
	}

	box := e.sbx.box(name)
	if box.template != "" || !slices.Equal(box.deny, []string{"ssh.github.com"}) {
		t.Errorf("created with template %q, deny %q, want sbx's own template with ssh.github.com denied", box.template, box.deny)
	}
	repo := e.sbx.repo(name)
	if got := gittest.Run(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Errorf("sandbox repository is on %q, want a detached HEAD", got)
	}
	if got := gittest.Run(t, repo, "rev-parse", "HEAD"); got != s.repos[0].base {
		t.Errorf("sandbox repository is at %s, want the base %s", got, s.repos[0].base)
	}
	if got := gittest.Run(t, repo, "for-each-ref", "--format=%(refname)"); got != "refs/remotes/origin/main" {
		t.Errorf("sandbox refs = %q, want only origin/main", got)
	}
	if got := gittest.Run(t, repo, "config", "--local", "--list"); strings.Contains(got, "user.") {
		t.Errorf("sandbox repository has a commit identity: %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, setupMarker)); err == nil {
		t.Error("the setup script ran")
	}
	if _, err := os.Stat(filepath.Join(e.runs.ctl(name), "setup.sh")); err == nil {
		t.Error("the setup script went into the run's directory")
	}
	if got := e.readCtl(name, "input.md"); got != s.input {
		t.Errorf("input.md = %q, want the input alone", got)
	}
	if start := box.starts; len(start) != 1 || !slices.Contains(start[0], prompt("reviewer")) {
		t.Errorf("starts = %.200q, want one with reviewer's prompt", start)
	}
	e.wantState(name, running)
}

func TestOpenEnvironmentNeedsAPerson(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.envr.err = fmt.Errorf("build template: %w", personError{why: "The Dockerfile does not build.",
		detail: "build failed", output: "step 3/5: a secret in the build output"})
	s, name := e.testSpec()

	p := e.open(s)
	if p == nil || p.why != "The Dockerfile does not build." {
		t.Fatalf("problem = %+v, want only the reason", p)
	}
	wantContains(t, "kept build output", p.outputs["build.log"], "a secret in the build output")
	if e.sbx.exists(name) {
		t.Error("a sandbox was created")
	}
}

func TestOpenRunsTheRolesModelOfTheTier(t *testing.T) {
	t.Parallel()
	for _, ro := range []role{builderRole, reviewerRole, resolverRole} {
		for _, tier := range []label.Tier{label.Light, label.Normal, label.Heavy} {
			t.Run(ro.name+" with "+string(tier), func(t *testing.T) {
				t.Parallel()
				e := newEnv(t, map[string]string{"app.txt": "v1\n"})
				s, _ := e.testSpec()
				if !ro.project {
					s.repos[0].branch = ""
				}
				s.tier = tier
				if p, err := e.runs.open(t.Context(), ro, 1, s); p != nil || err != nil {
					t.Fatalf("open = %+v, %v", p, err)
				}
				want := strings.TrimPrefix(string(tier), "tier:") + "-" + ro.name
				if got := e.startedModel(e.runs.name(ro, 1)); got.Name != want {
					t.Errorf("claude got %v, want %s", got, want)
				}
			})
		}
	}
}

func TestOpenWithoutAModelCreatesNoSandbox(t *testing.T) {
	t.Parallel()
	for _, ro := range []role{builderRole, resolverRole} {
		t.Run(ro.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, map[string]string{"app.txt": "v1\n"})
			delete(e.runs.Models, label.Normal)
			s, _ := e.testSpec()
			if p, err := e.runs.open(t.Context(), ro, 1, s); p != nil || err == nil {
				t.Errorf("open = %+v, %v, want an error", p, err)
			}
			if name := e.runs.name(ro, 1); e.sbx.exists(name) {
				t.Error("a sandbox was created")
			}
		})
	}
}

func TestOpenEnvironmentError(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.envr.err = errors.New("sbx template ls: daemon not running")
	s, _ := e.testSpec()
	if p, err := e.runs.open(t.Context(), builderRole, 1, s); p != nil || err == nil {
		t.Errorf("open = %+v, %v, want an error and no problem", p, err)
	}
}

func TestOpenWhenTheLastSlotIsTaken(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.ProjectLimit = 1
	e.sbx.add(e.deps.SandboxPrefix + "9-resolver")
	s, name := e.testSpec()
	if _, err := e.runs.open(t.Context(), builderRole, 1, s); !errors.Is(err, errNoSlot) {
		t.Errorf("open = %v, want errNoSlot", err)
	}
	if e.sbx.exists(name) {
		t.Error("a sandbox was created over the limit")
	}
}

func TestSetupRunsInTheSandboxBeforeClaude(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, `echo "prd $EXOMECHA_PRD" > `+setupMarker+"\n")
	s, name := e.testSpec()

	if p := e.open(s); p != nil {
		t.Fatalf("open problem: %s", p.why)
	}
	b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), setupMarker))
	if err != nil || string(b) != "prd 1\n" {
		t.Errorf("setup marker = %q, %v", b, err)
	}
	note := "## Setup\n\nexomecha ran the project's setup script (`" + filepath.Join(e.runs.ctl(name), setupScript) + "`) in this directory."
	if got, want := e.readCtl(name, "input.md"), s.input+note+" It ran before you started.\n\n"; got != want {
		t.Errorf("input.md = %q", got)
	}
}

// wantOpenProblem opens a run whose setup script breaks a rule, and checks that claude never started.
func (e *env) wantOpenProblem(s spec, subs ...string) *problem {
	e.t.Helper()
	name := e.runs.name(builderRole, 1)
	p := e.open(s)
	if p == nil {
		e.t.Fatal("open succeeded, want a problem with the setup script")
	}
	wantContains(e.t, "setup problem", p.why, subs...)
	if starts := e.sbx.box(name).starts; len(starts) != 0 {
		e.t.Errorf("claude started after a rejected setup script: %q", starts)
	}
	return p
}

func TestSetupFailure(t *testing.T) {
	t.Parallel()
	// exit 0 is never reached: the script stops at the first failing command.
	e := newSetupEnv(t, "echo 'a secret in the output' >&2\nfalse\nexit 0\n")
	s, name := e.testSpec()
	p := e.wantOpenProblem(s, setupScript+"` did not end cleanly (exit status 1)")
	wantNotContains(t, "setup problem", p.why, "a secret in the output", "merge the base branch")
	wantContains(t, "setup.log", e.readCtl(name, "setup.log"), "a secret in the output")
}

func TestSetupUsesPipefail(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "false | true\n")
	s, _ := e.testSpec()
	e.wantOpenProblem(s, "did not end cleanly (exit status 1)")
}

func TestSetupTimesOut(t *testing.T) {
	t.Parallel()
	// 124 is what timeout exits with when it stops the script.
	e := newSetupEnv(t, "exit 124\n")
	s, _ := e.testSpec()
	e.wantOpenProblem(s, "ran past 10 minutes")
}

func TestSetupLeavesChanges(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "echo stray > stray.txt\n")
	s, _ := e.testSpec()
	p := e.wantOpenProblem(s, "changed the working copy")
	wantContains(t, "kept git status", p.outputs["git-status.log"], "?? stray.txt")
}

func TestSetupCommits(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "git commit -q --allow-empty -m 'from setup'\n")
	s, _ := e.testSpec()
	e.wantOpenProblem(s, "changed the working copy")
}

func TestSetupGetsNoGitAuth(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "env > "+setupMarker+"\n")
	s, name := e.testSpec()
	e.open(s)

	b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), setupMarker))
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "script environment", string(b), "EXOMECHA_PRD=1")
	// What the git runner adds to carry the token.
	wantNotContains(t, "script environment", string(b), "GIT_CONFIG_COUNT", "GIT_CONFIG_VALUE", "AUTHORIZATION")
}

// A service the script starts keeps its output open. setup must still return, and the service may stay.
func TestSetupMayLeaveAServiceRunning(t *testing.T) {
	t.Parallel()
	e := newSetupEnv(t, "sleep 60 &\necho started\n")
	s, name := e.testSpec()
	begin := time.Now()
	if p := e.open(s); p != nil {
		t.Fatalf("open problem: %s", p.why)
	}
	if took := time.Since(begin); took > 30*time.Second {
		t.Errorf("open waited %v for the script's service", took)
	}
	if got := e.readCtl(name, "setup.log"); got != "started\n" {
		t.Errorf("setup.log = %q", got)
	}
}
