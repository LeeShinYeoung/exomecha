package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
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

// testLaunch is a builder run for Task 1 on a new branch off main. Its input says whether setup ran.
func (e *env) testLaunch() launch {
	e.t.Helper()
	if err := e.deps.Git.Sync(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
	base := e.rev("origin/main")
	return launch{
		task:    1,
		name:    e.runs.name(1, "builder"),
		branch:  branchName(1),
		in:      inputs{Head: base, Base: base},
		request: agent.Request{Instructions: "the builder prompt", Schema: builderSchema},
		input: func(_ context.Context, ranSetup bool, _ *inputs) (string, string, error) {
			if ranSetup {
				return "setup ran", "", nil
			}
			return "no setup", "", nil
		},
	}
}

func (e *env) open(l launch) string {
	e.t.Helper()
	problem, err := e.runs.open(e.t.Context(), l)
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
	l := e.testLaunch()

	if problem := e.open(l); problem != "" {
		t.Fatalf("open problem: %s", problem)
	}

	box := e.sbx.box(l.name)
	if box.template != e.envr.ref || box.workspace != e.runs.ctl(l.name) {
		t.Errorf("created with template %q, workspace %q", box.template, box.workspace)
	}
	if !slices.Equal(box.deny, []string{"ssh.github.com"}) || !slices.Equal(box.allowed, e.envr.hosts) {
		t.Errorf("network: deny %q, allowed %q", box.deny, box.allowed)
	}
	repo := e.sbx.repo(l.name)
	if got := gittest.Run(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != l.branch {
		t.Errorf("sandbox repository is on %q, want %s", got, l.branch)
	}
	if got := gittest.Run(t, repo, "rev-parse", "origin/main"); got != l.in.Base {
		t.Errorf("origin/main in the sandbox = %s, want %s", got, l.in.Base)
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
	ctl := e.runs.ctl(l.name)
	start := box.starts[0]
	if !slices.Equal(start[:5], []string{repoDir, "sh", filepath.Join(ctl, "run.sh"), ctl, "claude"}) {
		t.Errorf("start = %q", start)
	}
	if i := slices.Index(start, "--permission-mode"); i < 0 || start[i+1] != "auto" {
		t.Errorf("claude does not run in auto mode: %q", start)
	}
	for _, a := range start {
		if strings.Contains(a, "dangerously") || strings.Contains(a, "bypass") {
			t.Errorf("start args contain %q", a)
		}
	}
	if got := e.readCtl(l.name, "input.md"); got != "no setup" {
		t.Errorf("input.md = %q", got)
	}
	if got := e.readCtl(l.name, "run.sh"); got != runScript {
		t.Errorf("run.sh = %q", got)
	}
	r, err := e.runs.read(t.Context(), l.name)
	if err != nil || r.state != running || r.in.Head != l.in.Head {
		t.Errorf("after open: %+v, %v", r, err)
	}
}

func TestOpenWithoutDockerfileUsesTheDefaultTemplate(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	l := e.testLaunch()
	e.open(l)
	if got := e.sbx.box(l.name).template; got != "" {
		t.Errorf("template = %q, want sbx's own", got)
	}
}

func TestOpenEnvironmentNeedsAPerson(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.envr.err = fmt.Errorf("build template: %w", personError{why: "The Dockerfile on the base branch does not build.", detail: "step 3/5: a secret in the build output"})
	l := e.testLaunch()

	if problem := e.open(l); problem != "The Dockerfile on the base branch does not build." {
		t.Errorf("problem = %q, want only the reason", problem)
	}
	if e.sbx.exists(l.name) {
		t.Error("a sandbox was created")
	}
	wantContains(t, "log", log.String(), "a secret in the build output")
}

func TestOpenEnvironmentError(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.envr.err = errors.New("sbx template ls: daemon not running")
	if problem, err := e.runs.open(t.Context(), e.testLaunch()); problem != "" || err == nil {
		t.Errorf("open = %q, %v, want an error and no problem", problem, err)
	}
}

func TestOpenWhenTheLastSlotIsTaken(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	e.runs.SandboxLimit = 1
	e.sbx.add(e.deps.SandboxPrefix + "9-resolver")
	l := e.testLaunch()
	if _, err := e.runs.open(t.Context(), l); !errors.Is(err, errNoSlot) {
		t.Errorf("open = %v, want errNoSlot", err)
	}
	if e.sbx.exists(l.name) {
		t.Error("a sandbox was created over the limit")
	}
}

func TestSetupRunsInTheSandboxBeforeClaude(t *testing.T) {
	e := newEnv(t, setupRepo(`echo "task $EXOMECHA_TASK" > `+setupMarker+"\n"))
	l := e.testLaunch()

	if problem := e.open(l); problem != "" {
		t.Fatalf("open problem: %s", problem)
	}
	b, err := os.ReadFile(filepath.Join(e.sbx.repo(l.name), setupMarker))
	if err != nil || string(b) != "task 1\n" {
		t.Errorf("setup marker = %q, %v", b, err)
	}
	if got := e.readCtl(l.name, "input.md"); got != "setup ran" {
		t.Errorf("input.md = %q", got)
	}
}

// wantSetupProblem opens a run whose setup script breaks a rule, and checks that claude never started.
func (e *env) wantOpenProblem(l launch, subs ...string) string {
	e.t.Helper()
	problem := e.open(l)
	if problem == "" {
		e.t.Fatal("open succeeded, want a problem with the setup script")
	}
	wantContains(e.t, "setup problem", problem, subs...)
	if starts := e.sbx.box(l.name).starts; len(starts) != 0 {
		e.t.Errorf("claude started after a rejected setup script: %q", starts)
	}
	return problem
}

func TestSetupFailure(t *testing.T) {
	log := captureLog(t)
	// exit 0 is never reached: the script stops at the first failing command.
	e := newEnv(t, setupRepo("echo 'a secret in the output' >&2\nfalse\nexit 0\n"))
	problem := e.wantOpenProblem(e.testLaunch(), "`"+setupScript+"` did not end cleanly (exit status 1)", "exomecha's log")
	wantNotContains(t, "setup problem", problem, "a secret in the output", "merge the base branch")
	wantContains(t, "log", log.String(), "a secret in the output")
}

func TestSetupUsesPipefail(t *testing.T) {
	e := newEnv(t, setupRepo("false | true\n"))
	e.wantOpenProblem(e.testLaunch(), "did not end cleanly (exit status 1)")
}

func TestSetupTimesOut(t *testing.T) {
	// 124 is what timeout exits with when it stops the script.
	e := newEnv(t, setupRepo("exit 124\n"))
	e.wantOpenProblem(e.testLaunch(), "ran past 10 minutes")
}

func TestSetupLeavesChanges(t *testing.T) {
	log := captureLog(t)
	e := newEnv(t, setupRepo("echo stray > stray.txt\n"))
	e.wantOpenProblem(e.testLaunch(), "changed the working copy")
	wantContains(t, "log", log.String(), "?? stray.txt")
}

func TestSetupCommits(t *testing.T) {
	e := newEnv(t, setupRepo("git commit -q --allow-empty -m 'from setup'\n"))
	e.wantOpenProblem(e.testLaunch(), "changed the working copy")
}

func TestSetupGetsNoGitAuth(t *testing.T) {
	e := newEnv(t, setupRepo("env > "+setupMarker+"\n"))
	l := e.testLaunch()
	e.open(l)

	b, err := os.ReadFile(filepath.Join(e.sbx.repo(l.name), setupMarker))
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
	l := e.testLaunch()
	begin := time.Now()
	if problem := e.open(l); problem != "" {
		t.Fatalf("open problem: %s", problem)
	}
	if took := time.Since(begin); took > 4*time.Second {
		t.Errorf("open waited %v for the script's service", took)
	}
	if got := e.readCtl(l.name, "setup.log"); got != "started\n" {
		t.Errorf("setup.log = %q", got)
	}
}

// The script and the .gitignore entry it relies on reach the base branch after the PR branch
// started. Fixing the base branch cannot help that branch, so the problem must say to merge.
func TestSetupOnABranchOlderThanTheScript(t *testing.T) {
	e := newEnv(t, map[string]string{"app.txt": "v1\n"})
	old := gittest.Commit(t, e.origin, branchName(1), "main", map[string]string{"feature.txt": "first\n"})
	gittest.Commit(t, e.origin, "main", "", setupRepo("echo ran > "+setupMarker+"\n"))
	l := e.testLaunch()
	l.in.Head = old

	e.wantOpenProblem(l, "changed the working copy", "merge the base branch into `"+branchName(1)+"`")
}
