package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
)

func (e *env) rev(ref string) string {
	e.t.Helper()
	commit, ok, err := e.deps.Git.Rev(e.t.Context(), ref)
	if err != nil || !ok {
		e.t.Fatalf("Rev(%s) = %q, %v, %v", ref, commit, ok, err)
	}
	return commit
}

// testSpec is a builder run for Task 1 on a new branch off main, and the name of its sandbox.
func (e *env) testSpec() (spec, string) {
	e.t.Helper()
	if err := e.deps.Git.Sync(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
	base := e.rev("origin/main")
	return spec{branch: branchName(1), base: base, head: base, input: "the Task\n\n"}, e.runs.name(builderRole, 1)
}

func (e *env) open(s spec) string {
	e.t.Helper()
	problem, err := e.runs.open(e.t.Context(), builderRole, 1, s)
	if err != nil {
		e.t.Fatal(err)
	}
	return problem
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
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.envr.ref = "docker.io/library/exomecha-owner-repo:0123456789ab"
	e.envr.hosts = []string{"registry.npmjs.org", "*.pypi.org"}
	s, name := e.testSpec()

	if problem := e.open(s); problem != "" {
		t.Fatalf("open problem: %s", problem)
	}

	box := e.sbx.box(name)
	if box.template != e.envr.ref || box.workspace != e.runs.ctl(name) {
		t.Errorf("created with template %q, workspace %q", box.template, box.workspace)
	}
	if !slices.Equal(box.deny, []string{"ssh.github.com"}) || !slices.Equal(box.allowed, e.envr.hosts) {
		t.Errorf("network: deny %q, allowed %q", box.deny, box.allowed)
	}
	repo := e.sbx.repo(name)
	if got := gittest.Run(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != s.branch {
		t.Errorf("sandbox repository is on %q, want %s", got, s.branch)
	}
	if got := gittest.Run(t, repo, "rev-parse", "origin/main"); got != s.base {
		t.Errorf("origin/main in the sandbox = %s, want %s", got, s.base)
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
	for flag, want := range map[string]string{"--permission-mode": "auto", "--append-system-prompt": prompt("builder"), "--json-schema": builderSchema} {
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
	r, err := e.runs.read(t.Context(), name)
	if err != nil || r.state != running || r.in.Head != s.head {
		t.Errorf("after open: %+v, %v", r, err)
	}
}

// A reader's run does not run the project, so none of the project's environment goes in.
func TestOpenReaderGetsOnlyTheCode(t *testing.T) {
	e := newEnv(t, setupRepo("echo ran > "+setupMarker+"\n"))
	e.envr.err = errors.New("read the project's environment")
	e.sbx.fail["allow"] = errors.New("allow hosts")
	s, _ := e.testSpec()
	s.branch = ""
	name := e.runs.name(reviewerRole, 1)

	if problem, err := e.runs.open(t.Context(), reviewerRole, 1, s); problem != "" || err != nil {
		t.Fatalf("open = %q, %v", problem, err)
	}

	box := e.sbx.box(name)
	if box.template != "" || !slices.Equal(box.deny, []string{"ssh.github.com"}) {
		t.Errorf("created with template %q, deny %q, want sbx's own template with ssh.github.com denied", box.template, box.deny)
	}
	repo := e.sbx.repo(name)
	if got := gittest.Run(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Errorf("sandbox repository is on %q, want a detached HEAD", got)
	}
	if got := gittest.Run(t, repo, "rev-parse", "HEAD"); got != s.base {
		t.Errorf("sandbox repository is at %s, want the base %s", got, s.base)
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

func TestOpenWithoutDockerfileUsesTheDefaultTemplate(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	s, name := e.testSpec()
	e.open(s)
	if got := e.sbx.box(name).template; got != "" {
		t.Errorf("template = %q, want sbx's own", got)
	}
}

func TestOpenEnvironmentNeedsAPerson(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.envr.err = fmt.Errorf("build template: %w", personError{why: "The Dockerfile on the base branch does not build.", detail: "step 3/5: a secret in the build output"})
	s, name := e.testSpec()

	if problem := e.open(s); problem != "The Dockerfile on the base branch does not build." {
		t.Errorf("problem = %q, want only the reason", problem)
	}
	if e.sbx.exists(name) {
		t.Error("a sandbox was created")
	}
	wantContains(t, "log", log.String(), "a secret in the build output")
}

func TestOpenEnvironmentError(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.envr.err = errors.New("sbx template ls: daemon not running")
	s, _ := e.testSpec()
	if problem, err := e.runs.open(t.Context(), builderRole, 1, s); problem != "" || err == nil {
		t.Errorf("open = %q, %v, want an error and no problem", problem, err)
	}
}

func TestOpenWhenTheLastSlotIsTaken(t *testing.T) {
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
	e := newEnv(t, setupRepo(`echo "task $EXOMECHA_TASK" > `+setupMarker+"\n"))
	s, name := e.testSpec()

	if problem := e.open(s); problem != "" {
		t.Fatalf("open problem: %s", problem)
	}
	b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), setupMarker))
	if err != nil || string(b) != "task 1\n" {
		t.Errorf("setup marker = %q, %v", b, err)
	}
	if got, want := e.readCtl(name, "input.md"), s.input+setupNote+" It ran before you started.\n\n"; got != want {
		t.Errorf("input.md = %q", got)
	}
}

// wantSetupProblem opens a run whose setup script breaks a rule, and checks that claude never started.
func (e *env) wantOpenProblem(s spec, subs ...string) string {
	e.t.Helper()
	name := e.runs.name(builderRole, 1)
	problem := e.open(s)
	if problem == "" {
		e.t.Fatal("open succeeded, want a problem with the setup script")
	}
	wantContains(e.t, "setup problem", problem, subs...)
	if starts := e.sbx.box(name).starts; len(starts) != 0 {
		e.t.Errorf("claude started after a rejected setup script: %q", starts)
	}
	return problem
}

func TestSetupFailure(t *testing.T) {
	log := captureLog(t)
	// exit 0 is never reached: the script stops at the first failing command.
	e := newEnv(t, setupRepo("echo 'a secret in the output' >&2\nfalse\nexit 0\n"))
	s, _ := e.testSpec()
	problem := e.wantOpenProblem(s, "`"+setupScript+"` did not end cleanly (exit status 1)", "exomecha's log")
	wantNotContains(t, "setup problem", problem, "a secret in the output", "merge the base branch")
	wantContains(t, "log", log.String(), "a secret in the output")
}

func TestSetupUsesPipefail(t *testing.T) {
	e := newEnv(t, setupRepo("false | true\n"))
	s, _ := e.testSpec()
	e.wantOpenProblem(s, "did not end cleanly (exit status 1)")
}

func TestSetupTimesOut(t *testing.T) {
	// 124 is what timeout exits with when it stops the script.
	e := newEnv(t, setupRepo("exit 124\n"))
	s, _ := e.testSpec()
	e.wantOpenProblem(s, "ran past 10 minutes")
}

func TestSetupLeavesChanges(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, setupRepo("echo stray > stray.txt\n"))
	s, _ := e.testSpec()
	e.wantOpenProblem(s, "changed the working copy")
	wantContains(t, "log", log.String(), "?? stray.txt")
}

func TestSetupCommits(t *testing.T) {
	e := newEnv(t, setupRepo("git commit -q --allow-empty -m 'from setup'\n"))
	s, _ := e.testSpec()
	e.wantOpenProblem(s, "changed the working copy")
}

func TestSetupGetsNoGitAuth(t *testing.T) {
	e := newEnv(t, setupRepo("env > "+setupMarker+"\n"))
	s, name := e.testSpec()
	e.open(s)

	b, err := os.ReadFile(filepath.Join(e.sbx.repo(name), setupMarker))
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, "script environment", string(b), "EXOMECHA_TASK=1")
	// What the git runner adds to carry the token.
	wantNotContains(t, "script environment", string(b), "GIT_CONFIG_COUNT", "GIT_CONFIG_VALUE", "AUTHORIZATION")
}

// A service the script starts keeps its output open. setup must still return, and the service may stay.
func TestSetupMayLeaveAServiceRunning(t *testing.T) {
	e := newEnv(t, setupRepo("sleep 5 &\necho started\n"))
	s, name := e.testSpec()
	begin := time.Now()
	if problem := e.open(s); problem != "" {
		t.Fatalf("open problem: %s", problem)
	}
	if took := time.Since(begin); took > 4*time.Second {
		t.Errorf("open waited %v for the script's service", took)
	}
	if got := e.readCtl(name, "setup.log"); got != "started\n" {
		t.Errorf("setup.log = %q", got)
	}
}

// The script and the .gitignore entry it relies on reach the base branch after the PR branch
// started. Fixing the base branch cannot help that branch, so the problem must say to merge.
func TestSetupOnABranchOlderThanTheScript(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	old := gittest.Commit(t, e.origin, branchName(1), "main", map[string]string{"feature.txt": "first\n"})
	gittest.Commit(t, e.origin, "main", "", setupRepo("echo ran > "+setupMarker+"\n"))
	s, _ := e.testSpec()
	s.head = old

	e.wantOpenProblem(s, "changed the working copy", "merge the base branch into `"+branchName(1)+"`")
}
