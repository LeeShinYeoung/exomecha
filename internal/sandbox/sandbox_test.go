package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSBX puts an sbx on PATH that records its arguments and stdin, and answers from files the
// test writes: out.<verb> on stdout, err.<verb> on stderr and exit 1.
type fakeSBX struct {
	t   *testing.T
	dir string
}

func newFakeSBX(t *testing.T) *fakeSBX {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
dir=$(dirname "$0")
# One write per call, since LoadTemplate runs two at once.
line=$(printf '%s\037' "$@")
printf '%s\n' "$line" >> "$dir/calls"
cat > "$dir/in.$1"
[ -f "$dir/out.$1" ] && cat "$dir/out.$1"
if [ -f "$dir/err.$1" ]; then cat "$dir/err.$1" >&2; exit 1; fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "sbx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &fakeSBX{t: t, dir: dir}
}

func (f *fakeSBX) answer(verb, stdout string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "out."+verb), []byte(stdout), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSBX) fail(verb, stderr string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "err."+verb), []byte(stderr), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSBX) stdin(verb string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "in."+verb))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *fakeSBX) calls() [][]string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var out [][]string
	for line := range strings.Lines(string(b)) {
		args := strings.Split(strings.TrimSuffix(line, "\n"), "\037")
		out = append(out, args[:len(args)-1])
	}
	return out
}

func (f *fakeSBX) wantCalls(want ...[]string) {
	f.t.Helper()
	got := f.calls()
	if !slices.EqualFunc(got, want, slices.Equal) {
		f.t.Errorf("sbx calls:\n got  %q\n want %q", got, want)
	}
}

func TestCreate(t *testing.T) {
	f := newFakeSBX(t)
	ctx := t.Context()
	if err := (CLI{}).Create(ctx, "box", "docker.io/library/exomecha-o-r:abc", "/ctl/box", []string{"ssh.github.com"}); err != nil {
		t.Fatal(err)
	}
	if err := (CLI{}).Create(ctx, "plain", "", "/ctl/plain", nil); err != nil {
		t.Fatal(err)
	}
	f.wantCalls(
		[]string{"run", "-d", "--name", "box", "--pull", "never", "-t", "docker.io/library/exomecha-o-r:abc", "--deny-network", "ssh.github.com", "claude", "/ctl/box"},
		[]string{"run", "-d", "--name", "plain", "claude", "/ctl/plain"},
	)
}

func TestList(t *testing.T) {
	f := newFakeSBX(t)
	// From sbx v0.46.0.
	f.answer("ls", `{
  "sandboxes": [
    {
      "name": "exomecha-run-o-r-12-builder",
      "id": "e6a2a78c-c537-45ef-b69d-54ad11643c2b",
      "agent": "claude",
      "status": "running",
      "last_used_at": "2026-10-01T09:19:41.677554Z",
      "workspaces": ["/Users/me/.exomecha/o/r/sandboxes/exomecha-run-o-r-12-builder/box"],
      "created_at": "2026-10-01T09:19:37Z"
    },
    {"name": "other", "status": "stopped"}
  ]
}`)
	boxes, err := (CLI{}).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Box{{Name: "exomecha-run-o-r-12-builder", Status: "running"}, {Name: "other", Status: "stopped"}}
	if !slices.Equal(boxes, want) {
		t.Errorf("List = %+v, want %+v", boxes, want)
	}
	if !boxes[0].Running() || boxes[1].Running() {
		t.Errorf("Running = %v, %v", boxes[0].Running(), boxes[1].Running())
	}
	f.wantCalls([]string{"ls", "--json"})
}

func TestExec(t *testing.T) {
	f := newFakeSBX(t)
	f.answer("exec", "out\n")
	out, err := (CLI{}).Exec(t.Context(), "box", "/home/agent/repo", []string{"EXOMECHA_PRD=3"}, "git", "status")
	if err != nil || string(out) != "out\n" {
		t.Errorf("Exec = %q, %v", out, err)
	}
	f.wantCalls([]string{"exec", "-w", "/home/agent/repo", "-e", "EXOMECHA_PRD=3", "box", "git", "status"})
}

func TestExecFailure(t *testing.T) {
	f := newFakeSBX(t)
	f.fail("exec", "error: sandbox 'box' not found")
	if _, err := (CLI{}).Exec(t.Context(), "box", "", nil, "true"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Exec error = %v, want sbx's message", err)
	}
	f.wantCalls([]string{"exec", "box", "true"})
}

func TestStart(t *testing.T) {
	f := newFakeSBX(t)
	if err := (CLI{}).Start(t.Context(), "box", "/home/agent/repo", []string{"NPM_TOKEN=x"}, "sh", "/ctl/run.sh", "claude", "-p"); err != nil {
		t.Fatal(err)
	}
	f.wantCalls([]string{"exec", "-w", "/home/agent/repo", "-e", "NPM_TOKEN=x", "box", "sh", "-c", `setsid -f "$@" </dev/null >/dev/null 2>&1`, "sh", "sh", "/ctl/run.sh", "claude", "-p"})
}

func TestSetSecret(t *testing.T) {
	f := newFakeSBX(t)
	s := Secret{Env: "NPM_TOKEN", Hosts: []string{"npm.pkg.github.com", "*.npm.example.com"}, Command: "/usr/local/bin/npm-token --org acme"}
	if err := (CLI{}).SetSecret(t.Context(), "box", s); err != nil {
		t.Fatal(err)
	}
	f.wantCalls([]string{"secret", "set-custom", "--sandbox", "box", "--env", "NPM_TOKEN", "--placeholder", "exomecha-secret-npm_token",
		"--command", "/usr/local/bin/npm-token --org acme", "--host", "npm.pkg.github.com", "--host", "*.npm.example.com"})
}

func TestAllow(t *testing.T) {
	f := newFakeSBX(t)
	if err := (CLI{}).Allow(t.Context(), "box", nil); err != nil {
		t.Fatal(err)
	}
	if err := (CLI{}).Allow(t.Context(), "box", []string{"registry.npmjs.org", "*.pypi.org"}); err != nil {
		t.Fatal(err)
	}
	f.wantCalls([]string{"policy", "allow", "network", "--sandbox", "box", "registry.npmjs.org,*.pypi.org"})
}

func TestAllowRejectedPattern(t *testing.T) {
	newFakeSBX(t).fail("policy", "error: invalid network pattern \"a.example.com:x\": port must be 1-65535 or \"*\"\n  try: sbx policy allow network --help\n")
	err := (CLI{}).Allow(t.Context(), "box", []string{"a.example.com:x"})
	p, ok := errors.AsType[*PatternError](err)
	if want := `invalid network pattern "a.example.com:x": port must be 1-65535 or "*"`; !ok || p.Detail != want {
		t.Errorf("Allow = %v, want a PatternError with %q", err, want)
	}
}

func TestAllowFailure(t *testing.T) {
	newFakeSBX(t).fail("policy", "error: daemon not running\n")
	err := (CLI{}).Allow(t.Context(), "box", []string{"a.example.com"})
	if _, ok := errors.AsType[*PatternError](err); err == nil || ok {
		t.Errorf("Allow = %v, want a plain error", err)
	}
}

func TestCreateHelper(t *testing.T) {
	f := newFakeSBX(t)
	if err := (CLI{}).CreateHelper(t.Context(), "exomecha-build-o-r"); err != nil {
		t.Fatal(err)
	}
	f.wantCalls([]string{"create", "--name", "exomecha-build-o-r", "claude"})
}

func TestTemplates(t *testing.T) {
	f := newFakeSBX(t)
	// From sbx v0.46.0.
	f.answer("template", `{
  "images": [
    {
      "id": "549730947ed8",
      "repository": "docker.io/docker/sandbox-templates",
      "tag": "claude-code-docker",
      "flavor": "claude-code-docker",
      "created_at": "2026-09-23T18:15:37Z",
      "size": 920132475
    }
  ]
}`)
	got, err := (CLI{}).Templates(t.Context())
	want := []Template{{ID: "549730947ed8", Repository: "docker.io/docker/sandbox-templates", Tag: "claude-code-docker"}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("Templates = %+v, %v, want %+v", got, err, want)
	}
	f.wantCalls([]string{"template", "ls", "--json"})
}

func TestRemoveTemplate(t *testing.T) {
	f := newFakeSBX(t)
	if err := (CLI{}).RemoveTemplate(t.Context(), "docker.io/library/exomecha-o-r:abc"); err != nil {
		t.Fatal(err)
	}
	f.wantCalls([]string{"template", "rm", "--force", "docker.io/library/exomecha-o-r:abc"})
}

func TestBuildImage(t *testing.T) {
	f := newFakeSBX(t)
	output, err := (CLI{}).BuildImage(t.Context(), "box", "repo:tag", "FROM x\n")
	if err != nil || output != "" {
		t.Errorf("BuildImage = %q, %v", output, err)
	}
	f.wantCalls([]string{"exec", "-i", "box", "docker", "build", "-t", "repo:tag", "-"})
	if got := f.stdin("exec"); got != "FROM x\n" {
		t.Errorf("build stdin = %q, want the Dockerfile", got)
	}
}

func TestBuildImageFailure(t *testing.T) {
	f := newFakeSBX(t)
	f.answer("exec", "#5 [2/2] RUN false\nTOKEN=s3cret\n")
	f.fail("exec", "ERROR: failed to build\n")
	output, err := (CLI{}).BuildImage(t.Context(), "box", "repo:tag", "FROM x\n")
	if err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "failed to build") {
		t.Errorf("BuildImage error = %v, want an error without the build output", err)
	}
	if !strings.Contains(output, "TOKEN=s3cret") || !strings.Contains(output, "ERROR: failed to build") {
		t.Errorf("BuildImage output = %q, want stdout and stderr", output)
	}
}

func TestLoadTemplate(t *testing.T) {
	f := newFakeSBX(t)
	f.answer("exec", "image bytes")
	if err := (CLI{}).LoadTemplate(t.Context(), "box", "repo:tag"); err != nil {
		t.Fatal(err)
	}
	if got := f.stdin("template"); got != "image bytes" {
		t.Errorf("template load stdin = %q, want what docker save printed", got)
	}
	calls := f.calls()
	slices.SortFunc(calls, slices.Compare)
	want := [][]string{{"exec", "box", "docker", "save", "repo:tag"}, {"template", "load", "/dev/stdin"}}
	if !slices.EqualFunc(calls, want, slices.Equal) {
		t.Errorf("sbx calls = %q, want %q", calls, want)
	}
}

func TestLoadTemplateSaveFailure(t *testing.T) {
	newFakeSBX(t).fail("exec", "Error: No such image: repo:tag\n")
	err := (CLI{}).LoadTemplate(t.Context(), "box", "repo:tag")
	if err == nil || !strings.Contains(err.Error(), "No such image") {
		t.Errorf("LoadTemplate = %v, want docker save's message", err)
	}
}

func TestRemove(t *testing.T) {
	f := newFakeSBX(t)
	if err := (CLI{}).Remove(t.Context(), "box"); err != nil {
		t.Fatal(err)
	}
	f.wantCalls([]string{"rm", "--force", "box"})
}

func TestRemoveGoneSandbox(t *testing.T) {
	f := newFakeSBX(t)
	f.fail("rm", "Error: sandbox 'box' not found\n  try: sbx ls")
	if err := (CLI{}).Remove(t.Context(), "box"); err != nil {
		t.Errorf("Remove of a gone sandbox = %v, want nil", err)
	}
	f.fail("rm", "Error: daemon not running")
	if err := (CLI{}).Remove(t.Context(), "box"); err == nil {
		t.Error("Remove hid a real failure")
	}
}

func TestGlobalGitHubSecret(t *testing.T) {
	f := newFakeSBX(t)
	f.answer("secret", `{"secrets": []}`)
	if has, err := (CLI{}).GlobalGitHubSecret(t.Context()); err != nil || has {
		t.Errorf("GlobalGitHubSecret with none = %v, %v", has, err)
	}
	f.answer("secret", `{"secrets": [{"scope": "global", "type": "service", "name": "github", "secret": "gh*****"}]}`)
	if has, err := (CLI{}).GlobalGitHubSecret(t.Context()); err != nil || !has {
		t.Errorf("GlobalGitHubSecret with one = %v, %v", has, err)
	}
	f.wantCalls(
		[]string{"secret", "ls", "--global", "--service", "github", "--json"},
		[]string{"secret", "ls", "--global", "--service", "github", "--json"},
	)
}

func TestClaudeSignedIn(t *testing.T) {
	f := newFakeSBX(t)
	f.answer("secret", `{"secrets": []}`)
	if in, err := (CLI{}).ClaudeSignedIn(t.Context()); err != nil || in {
		t.Errorf("ClaudeSignedIn with none = %v, %v", in, err)
	}
	// From sbx v0.46.0.
	f.answer("secret", `{
  "secrets": [
    {
      "scope": "global",
      "type": "service",
      "name": "anthropic",
      "secret": "(oauth configured)"
    }
  ],
  "custom_secrets": [],
  "shadowed_services": [],
  "env_only_count": 1
}`)
	if in, err := (CLI{}).ClaudeSignedIn(t.Context()); err != nil || !in {
		t.Errorf("ClaudeSignedIn with a login = %v, %v", in, err)
	}
	f.wantCalls(
		[]string{"secret", "ls", "--global", "--service", "anthropic", "--json"},
		[]string{"secret", "ls", "--global", "--service", "anthropic", "--json"},
	)
}

func TestSignIn(t *testing.T) {
	f := newFakeSBX(t)
	terminal, err := os.CreateTemp(t.TempDir(), "terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if err := (CLI{}).SignIn(t.Context(), terminal); err != nil {
		t.Fatal(err)
	}
	f.wantCalls(
		[]string{"rm", "--force", "exomecha-login"},
		[]string{"create", "--name", "exomecha-login", "claude"},
		[]string{"exec", "-it", "exomecha-login", "claude", "auth", "login"},
		[]string{"rm", "--force", "exomecha-login"},
	)
}

func TestSignInRemovesTheSandboxWhenLoginFails(t *testing.T) {
	f := newFakeSBX(t)
	f.fail("exec", "login cancelled")
	terminal, err := os.CreateTemp(t.TempDir(), "terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if err := (CLI{}).SignIn(t.Context(), terminal); err == nil {
		t.Error("SignIn with a failed login = nil")
	}
	if calls := f.calls(); len(calls) != 4 || !slices.Equal(calls[3], []string{"rm", "--force", "exomecha-login"}) {
		t.Errorf("sbx calls = %q, want the sandbox removed last", calls)
	}
}

func TestReadsJSONBeforeAnUpdateNotice(t *testing.T) {
	f := newFakeSBX(t)
	// From sbx v0.46.0 in a new home folder, shortened.
	f.answer("secret", `{
  "secrets": []
}

╭──────────────────────────────────╮
│ Docker Sandboxes Update Available │
╰──────────────────────────────────╯
`)
	if in, err := (CLI{}).ClaudeSignedIn(t.Context()); err != nil || in {
		t.Errorf("ClaudeSignedIn = %v, %v", in, err)
	}
}

func TestDiagnose(t *testing.T) {
	f := newFakeSBX(t)
	// From sbx v0.46.0 with its daemon stopped, shortened.
	f.answer("diagnose", `{
 "version": "1.0",
 "checks": [
  {"name": "CLI binary", "status": "pass", "message": "found", "detail": "/opt/homebrew/bin/sbx", "hint": ""},
  {"name": "Binary version", "status": "warn", "message": "update available: v0.47.0 (running v0.46.0)", "detail": "", "hint": "Download the latest release from the link above"},
  {"name": "Daemon", "status": "fail", "message": "not reachable", "detail": "dial unix sandboxd.sock: connect: no such file or directory", "hint": "Run: sbx daemon start"},
  {"name": "Authentication", "status": "skip", "message": "Skipped: Daemon failed", "detail": "", "hint": ""}
 ],
 "summary": {"pass": 1, "warn": 1, "fail": 1, "skip": 1}
}`)
	f.fail("diagnose", "")
	got, err := (CLI{}).Diagnose(t.Context())
	want := []Finding{{Name: "Daemon", Message: "not reachable", Hint: "Run: sbx daemon start"}}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("Diagnose = %+v, %v, want %+v", got, err, want)
	}
	f.wantCalls([]string{"diagnose", "--json"})
}

func TestDiagnoseFailure(t *testing.T) {
	f := newFakeSBX(t)
	f.fail("diagnose", "unknown command")
	if _, err := (CLI{}).Diagnose(t.Context()); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("Diagnose = %v, want sbx's error", err)
	}
}

func TestGitURL(t *testing.T) {
	if got := (CLI{}).GitURL("box", "/home/agent/repo"); got != "ext::sbx exec -i box git %s /home/agent/repo" {
		t.Errorf("GitURL = %q", got)
	}
}

func TestSlug(t *testing.T) {
	for _, c := range []struct{ owner, repo, want string }{
		{"LeeShinYeoung", "jazzing", "leeshinyeoung-jazzing"},
		{"owner", "my.repo_name", "owner-my-repo-name"},
		{"owner", "a..--b", "owner-a-b"},
		{"-owner-", ".repo.", "owner-repo"},
		{"o", strings.Repeat("r", 30), "o-" + strings.Repeat("r", 30)},
	} {
		if got := Slug(c.owner, c.repo); got != c.want {
			t.Errorf("Slug(%q, %q) = %q, want %q", c.owner, c.repo, got, c.want)
		}
	}
}

func TestSlugTooLong(t *testing.T) {
	owner, repo := "o", strings.Repeat("r", 31)
	sum := sha256.Sum256([]byte("o/" + repo))
	hash := hex.EncodeToString(sum[:])[:8]

	got := Slug(owner, repo)
	if want := "o-" + strings.Repeat("r", 21) + "-" + hash; got != want {
		t.Errorf("Slug of 33 chars = %q, want %q", got, want)
	}
	if other := Slug(owner, repo+"x"); other == got {
		t.Errorf("two repos with the same first 23 chars share the slug %q", got)
	}
	if upper := Slug("O", strings.ToUpper(repo)); upper != got {
		t.Errorf("Slug in upper case = %q, want the same as lower case %q", upper, got)
	}
}

func TestSlugCutAtADash(t *testing.T) {
	// The cut lands right after the owner's dash, which must not leave "--" before the hash.
	owner := strings.Repeat("o", 22)
	got := Slug(owner, strings.Repeat("r", 30))
	if strings.Contains(got, "--") || !strings.HasPrefix(got, owner+"-") || len(got) != 31 {
		t.Errorf("Slug = %q, want %s-<hash>", got, owner)
	}
}

func TestPrefix(t *testing.T) {
	if got := Prefix("LeeShinYeoung", "jazzing"); got != "exomecha-run-leeshinyeoung-jazzing-" {
		t.Errorf("Prefix = %q", got)
	}
}

func TestBuildName(t *testing.T) {
	if got := BuildName("LeeShinYeoung", "jazzing"); got != "exomecha-build-leeshinyeoung-jazzing" {
		t.Errorf("BuildName = %q", got)
	}
}

func TestNameFits63(t *testing.T) {
	prefix := Prefix("some-organization", strings.Repeat("long-repository-name-", 3))
	for _, role := range []string{"builder", "resolver", "reviewer"} {
		if name := Name(prefix, 9999999, role); len(name) > 63 {
			t.Errorf("%s is %d chars, sbx allows 63", name, len(name))
		}
	}
}

func TestParse(t *testing.T) {
	prefix := Prefix("o", "r")
	for _, c := range []struct {
		name  string
		issue int
		role  string
		ok    bool
	}{
		{prefix + "12-builder", 12, "builder", true},
		{prefix + "7-resolver", 7, "resolver", true},
		{prefix + "8-reviewer", 8, "reviewer", true},
		{Name(prefix, 9999999, "resolver"), 9999999, "resolver", true},
		{prefix + "012-builder", 0, "", false},
		{prefix + "0-builder", 0, "", false},
		{prefix + "12-", 0, "", false},
		{prefix + "12-Builder", 0, "", false},
		{prefix + "12-builder-x", 0, "", false},
		{prefix + "x-12-builder", 0, "", false},
		{Name(Prefix("o", "r2"), 12, "builder"), 0, "", false},
		{Name(Prefix("o", "r-2"), 12, "builder"), 0, "", false},
		{"exomecha-prd-o-r-12-builder", 0, "", false},
		{"exomecha-build-o-r", 0, "", false},
		{"impl-run-12-builder", 0, "", false},
	} {
		issue, role, ok := Parse(prefix, c.name)
		if issue != c.issue || role != c.role || ok != c.ok {
			t.Errorf("Parse(%q) = %d, %q, %v, want %d, %q, %v", c.name, issue, role, ok, c.issue, c.role, c.ok)
		}
	}
	if _, _, ok := Parse("", "12-builder"); ok {
		t.Error("Parse accepted an empty prefix")
	}
}
