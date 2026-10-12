package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

func TestMain(m *testing.M) {
	if e2eOn() {
		setUpE2E()
	}
	gittest.Main(m)
}

func TestHostMatches(t *testing.T) {
	for _, c := range []struct {
		pattern, host string
		want          bool
	}{
		{"github.com", "github.com", true},
		{"npm.pkg.github.com", "api.github.com", false},
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "github.com", false},
		{"**.github.com", "github.com", true},
		{"*.com", "github.com", true},
		{"*", "github.com", false},
		{"**", "api.github.com", true},
	} {
		if got := hostMatches(c.pattern, c.host); got != c.want {
			t.Errorf("hostMatches(%q, %q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

func TestParseConfig(t *testing.T) {
	const required = "repos: [acme/app]\nbase_branch: main\ngithub_token: secret\n"
	for _, c := range []struct {
		name    string
		yaml    string
		wantErr string
		check   func(t *testing.T, c config)
	}{
		{name: "empty file", yaml: "", wantErr: "repos, base_branch and github_token"},
		{name: "repo without owner", yaml: "repos: [app]\nbase_branch: main\ngithub_token: secret\n", wantErr: "repos must hold owner/name"},
		{name: "empty entry", yaml: "repos: [acme/app, '']\nbase_branch: main\ngithub_token: secret\n", wantErr: "repos must hold owner/name"},
		{name: "unknown key", yaml: required + "limits:\n  weekley: 50\n", wantErr: "line 5: exomecha has no setting weekley"},
		{name: "defaults", yaml: required, check: func(t *testing.T, c config) {
			d := defaults()
			if c.prd.full() != "acme/app" || len(c.repos) != 1 || c.repos[0] != c.prd || c.base != "main" || c.token != "secret" {
				t.Errorf("PRD repository %s, repositories %v on %s, token %q", c.prd.full(), c.repos, c.base, c.token)
			}
			if c.fiveHour != d.fiveHour || c.weekly != d.weekly || c.projectLimit != d.projectLimit || c.readLimit != d.readLimit || c.reviewLimit != d.reviewLimit {
				t.Errorf("limits = %v %v %d %d %d, want the defaults", c.fiveHour, c.weekly, c.projectLimit, c.readLimit, c.reviewLimit)
			}
			if c.light != d.light || c.normal != d.normal || c.heavy != d.heavy || c.resolver != d.resolver {
				t.Errorf("models = %v %v %v %v, want the defaults", c.light, c.normal, c.heavy, c.resolver)
			}
			opus := func(effort string) agent.Model { return agent.Model{Name: "opus", Effort: effort} }
			if d.light != opus("low") || d.normal != opus("high") || d.heavy != opus("xhigh") || d.resolver != opus("medium") || d.projectLimit != 2 || d.reviewLimit != 5 {
				t.Errorf("defaults: models %v %v %v %v, sandbox limit %d, review limit %d", d.light, d.normal, d.heavy, d.resolver, d.projectLimit, d.reviewLimit)
			}
		}},
		{name: "set values", yaml: required + "limits:\n  five_hour: 50\n  sandboxes: 3\n  reviews: 2\nmodels:\n  light: sonnet:low\n  resolver: sonnet:high\n", check: func(t *testing.T, c config) {
			if c.fiveHour != 0.5 || c.projectLimit != 3 || c.light != (agent.Model{Name: "sonnet", Effort: "low"}) {
				t.Errorf("five-hour %v, sandbox limit %d, light %v", c.fiveHour, c.projectLimit, c.light)
			}
			if c.resolver != (agent.Model{Name: "sonnet", Effort: "high"}) || c.reviewLimit != 2 {
				t.Errorf("resolver %v, review limit %d", c.resolver, c.reviewLimit)
			}
		}},
		{name: "several repositories", yaml: "repos: [acme/api, acme/web]\nprd_repo: acme/plans\nbase_branch: main\ngithub_token: secret\n", check: func(t *testing.T, c config) {
			if c.prd.full() != "acme/plans" || len(c.repos) != 2 || c.repos[0].full() != "acme/api" || c.repos[1].full() != "acme/web" {
				t.Errorf("PRD repository %s, repositories %v", c.prd.full(), c.repos)
			}
		}},
		{name: "PRD repository among the repositories", yaml: "repos: [acme/API, acme/web]\nprd_repo: acme/api\nbase_branch: main\ngithub_token: secret\n", check: func(t *testing.T, c config) {
			if c.prd != c.repos[0] {
				t.Errorf("PRD repository %v, want the first repository %v", c.prd, c.repos[0])
			}
		}},
		{name: "several repositories without a PRD repository", yaml: "repos: [acme/api, acme/web]\nbase_branch: main\ngithub_token: secret\n", wantErr: "prd_repo must be set"},
		{name: "a repository twice", yaml: "repos: [acme/api, Acme/Api]\nprd_repo: acme/plans\nbase_branch: main\ngithub_token: secret\n", wantErr: "lists Acme/Api twice"},
		{name: "two repositories of one name", yaml: "repos: [acme/api, other/api]\nprd_repo: acme/plans\nbase_branch: main\ngithub_token: secret\n", wantErr: "two repositories named api"},
		{name: "PRD repository without owner", yaml: required + "prd_repo: plans\n", wantErr: "prd_repo must hold owner/name"},
		{name: "secrets", yaml: required + `secrets:
  NPM_TOKEN:
    hosts: [npm.pkg.github.com, "*.npm.example.com"]
    command: /usr/local/bin/npm-token --org acme
  A:
    hosts: [a.example.com]
    value: a-value
`, check: func(t *testing.T, c config) {
			want := []sandbox.Secret{
				{Env: "A", Hosts: []string{"a.example.com"}, Value: "a-value"},
				{Env: "NPM_TOKEN", Hosts: []string{"npm.pkg.github.com", "*.npm.example.com"}, Command: "/usr/local/bin/npm-token --org acme"},
			}
			if !slices.EqualFunc(c.secrets, want, func(a, b sandbox.Secret) bool {
				return a.Env == b.Env && a.Value == b.Value && a.Command == b.Command && slices.Equal(a.Hosts, b.Hosts)
			}) {
				t.Errorf("secrets = %+v, want %+v", c.secrets, want)
			}
		}},
		{name: "secret without a value or a command", yaml: required + "secrets:\n  X:\n    hosts: [x.example.com]\n", wantErr: "secrets.X needs value"},
		{name: "secret with a value and a command", yaml: required + "secrets:\n  X:\n    hosts: [x.example.com]\n    value: v\n    command: /bin/x\n", wantErr: "secrets.X takes value or command, not both"},
		{name: "secret without hosts", yaml: required + "secrets:\n  X:\n    command: /bin/x\n", wantErr: "secrets.X needs hosts"},
		{name: "secret named like no variable", yaml: required + "secrets:\n  NPM-TOKEN:\n    hosts: [x.example.com]\n    command: /bin/x\n", wantErr: "not an environment variable name"},
		{name: "secret for GitHub", yaml: required + "secrets:\n  X:\n    hosts: [api.github.com]\n    command: /bin/x\n", wantErr: "AI could use the secret on GitHub"},
		{name: "secret for every GitHub host", yaml: required + "secrets:\n  X:\n    hosts: ['*.github.com']\n    command: /bin/x\n", wantErr: "reaches api.github.com"},
		{name: "secret for every host", yaml: required + "secrets:\n  X:\n    hosts: ['**']\n    command: /bin/x\n", wantErr: "reaches github.com"},
		{name: "usage limit out of range", yaml: required + "limits:\n  weekly: 0\n", wantErr: "limits.weekly"},
		{name: "limit not a number", yaml: required + "limits:\n  sandboxes: two\n", wantErr: "two"},
		{name: "model without effort", yaml: required + "models:\n  heavy: opus\n", wantErr: "models.heavy"},
		{name: "resolver model without effort", yaml: required + "models:\n  resolver: opus\n", wantErr: "models.resolver"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseConfig([]byte(c.yaml))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, got)
		})
	}
}

func TestLoadConfigRefusesAFileOthersCanRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acme.yaml")
	if err := os.WriteFile(path, []byte("repos: [acme/app]\nbase_branch: main\ngithub_token: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("load with 600: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("load with 644 = %v, want a refusal that says chmod 600", err)
	}
}

func TestSettingsPath(t *testing.T) {
	if got, err := settingsPath("/home/me", "acme_api-2"); err != nil || got != "/home/me/.exomecha/acme_api-2.yaml" {
		t.Errorf("settingsPath = %q, %v", got, err)
	}
	for _, bad := range []string{"", "../acme", "acme.yaml", "a b", "claude", "Claude"} {
		if _, err := settingsPath("/home/me", bad); err == nil {
			t.Errorf("settingsPath(%q) took the name", bad)
		}
	}
}

func TestCheckPRDRepo(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".exomecha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, data string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	acme := write("acme.yaml", "repos: [acme/api, acme/web]\nprd_repo: acme/plans\nbase_branch: main\ngithub_token: secret\n")
	write("broken.yaml", "repos: [me/broken]\n")
	other := filepath.Join(dir, "other.yaml")

	if err := checkPRDRepo(home, other, target{owner: "Acme", name: "Plans"}); err == nil || !strings.Contains(err.Error(), "project acme") {
		t.Errorf("another project's PRD repository = %v, want a refusal that names acme", err)
	}
	for _, c := range []struct {
		path string
		prd  target
	}{
		{acme, target{owner: "acme", name: "plans"}},
		{other, target{owner: "acme", name: "api"}},
		{other, target{owner: "me", name: "broken"}},
	} {
		if err := checkPRDRepo(home, c.path, c.prd); err != nil {
			t.Errorf("%s with %s = %v", filepath.Base(c.path), c.prd.full(), err)
		}
	}
}

func TestSecondInterruptEndsAtOnce(t *testing.T) {
	if os.Getenv("EXOMECHA_TEST_SIGNAL") == "1" {
		ctx, stop := signalContext()
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("stopping")
		// A shutdown slower than the test waits for.
		time.Sleep(time.Minute)
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSecondInterruptEndsAtOnce$")
	// e2eEnv off, or the child's TestMain removes the sandboxes of e2e scenarios still running. The
	// killed child cannot remove the folder gittest.Main makes, so TMPDIR puts it in one the test removes.
	cmd.Env = append(os.Environ(), "EXOMECHA_TEST_SIGNAL=1", e2eEnv+"=", "TMPDIR="+t.TempDir())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdout)
	waitFor := func(want string) {
		t.Helper()
		for lines.Scan() {
			if lines.Text() == want {
				return
			}
		}
		t.Fatalf("the child ended before it printed %q", want)
	}
	waitFor("ready")
	cmd.Process.Signal(os.Interrupt)
	waitFor("stopping")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// The default comes back just after ctx ends, so an interrupt sent at once may still be caught.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-done:
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGINT {
				t.Errorf("the child ended with %v, want a second interrupt to end it", err)
			}
			return
		case <-tick.C:
		}
	}
}
