package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSBX puts an sbx on PATH that records its arguments and answers from files the test writes:
// out.<verb> on stdout, err.<verb> on stderr and exit 1.
type fakeSBX struct {
	t   *testing.T
	dir string
}

func newFakeSBX(t *testing.T) *fakeSBX {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
dir=$(dirname "$0")
printf '%s\037' "$@" >> "$dir/calls"
printf '\n' >> "$dir/calls"
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
      "name": "exomecha-task-o-r-12-builder",
      "id": "e6a2a78c-c537-45ef-b69d-54ad11643c2b",
      "agent": "claude",
      "status": "running",
      "last_used_at": "2026-10-01T09:19:41.677554Z",
      "workspaces": ["/Users/me/.exomecha/o/r/sandboxes/exomecha-task-o-r-12-builder/box"],
      "created_at": "2026-10-01T09:19:37Z"
    },
    {"name": "other", "status": "stopped"}
  ]
}`)
	boxes, err := (CLI{}).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Box{{Name: "exomecha-task-o-r-12-builder", Status: "running"}, {Name: "other", Status: "stopped"}}
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
	out, err := (CLI{}).Exec(t.Context(), "box", "/home/agent/repo", []string{"EXOMECHA_TASK=3"}, "git", "status")
	if err != nil || string(out) != "out\n" {
		t.Errorf("Exec = %q, %v", out, err)
	}
	f.wantCalls([]string{"exec", "-w", "/home/agent/repo", "-e", "EXOMECHA_TASK=3", "box", "git", "status"})
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
	if err := (CLI{}).Start(t.Context(), "box", "/home/agent/repo", "sh", "/ctl/run.sh", "claude", "-p"); err != nil {
		t.Fatal(err)
	}
	f.wantCalls([]string{"exec", "-w", "/home/agent/repo", "box", "sh", "-c", `setsid -f "$@" </dev/null >/dev/null 2>&1`, "sh", "sh", "/ctl/run.sh", "claude", "-p"})
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
	if got := Prefix("LeeShinYeoung", "jazzing"); got != "exomecha-task-leeshinyeoung-jazzing-" {
		t.Errorf("Prefix = %q", got)
	}
}

func TestNameFits63(t *testing.T) {
	prefix := Prefix("some-organization", strings.Repeat("long-repository-name-", 3))
	for _, role := range []string{"builder", "resolver"} {
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
		{prefix + "9-splitter", 9, "splitter", true},
		{prefix + "10-sequencer", 10, "sequencer", true},
		{Name(prefix, 9999999, "resolver"), 9999999, "resolver", true},
		{prefix + "012-builder", 0, "", false},
		{prefix + "0-builder", 0, "", false},
		{prefix + "12-", 0, "", false},
		{prefix + "12-Builder", 0, "", false},
		{prefix + "12-builder-x", 0, "", false},
		{prefix + "x-12-builder", 0, "", false},
		{Name(Prefix("o", "r2"), 12, "builder"), 0, "", false},
		{Name(Prefix("o", "r-2"), 12, "builder"), 0, "", false},
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
