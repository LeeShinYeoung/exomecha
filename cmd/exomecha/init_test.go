package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

func TestAsk(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("acme/api,acme/api\nacme/api, acme/web\nplans\nacme/plans\n\n"))
	var out strings.Builder
	tokens := []string{"", " github_pat_x "}
	a, err := ask(in, &out, func() (string, error) {
		token := tokens[0]
		tokens = tokens[1:]
		return token, nil
	}, anyPRD)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.repos) != 2 || a.repos[0].full() != "acme/api" || a.repos[1].full() != "acme/web" || a.prd != "acme/plans" || a.base != "main" || a.token != "github_pat_x" {
		t.Errorf("answers = %+v", a)
	}
	for _, want := range []string{"repos lists acme/api twice", "prd_repo must hold owner/name"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not say %q", out.String(), want)
		}
	}
}

func TestAskOneRepositoryDoesNotAskForThePRDRepository(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("acme/app\ndevelop\n"))
	var out strings.Builder
	a, err := ask(in, &out, func() (string, error) { return "secret", nil }, anyPRD)
	if err != nil {
		t.Fatal(err)
	}
	if a.prd != "" || a.base != "develop" || strings.Contains(out.String(), "PRD repository") {
		t.Errorf("answers = %+v, output %q", a, out.String())
	}
}

func anyPRD(target) error { return nil }

func TestAskAsksAgainForAPRDRepositoryOfAnotherProject(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("acme/plans\nacme/api, acme/web\nacme/plans\nacme/web\n\n"))
	var out strings.Builder
	a, err := ask(in, &out, func() (string, error) { return "secret", nil }, func(prd target) error {
		if prd.full() == "acme/plans" {
			return errors.New("project plans already has acme/plans")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.repos) != 2 || a.prd != "acme/web" {
		t.Errorf("answers = %+v", a)
	}
	if n := strings.Count(out.String(), "project plans already has acme/plans"); n != 2 {
		t.Errorf("output says the repository is taken %d times, want 2 (repos and prd_repo):\n%s", n, out.String())
	}
}

func TestAskStopsWhenTheAnswersEnd(t *testing.T) {
	if _, err := ask(bufio.NewReader(strings.NewReader("acme/app\n")), &strings.Builder{}, nil, anyPRD); err == nil {
		t.Error("ask with no base branch answer = nil")
	}
}

func TestSettingsFileReadsBack(t *testing.T) {
	a := answers{repos: []target{{owner: "acme", name: "api"}, {owner: "acme", name: "web"}}, prd: "acme/plans", base: "1.0", token: "github_pat_x"}
	data := settingsFile(a)
	c, err := parseConfig(data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	d := defaults()
	if c.prd.full() != "acme/plans" || len(c.repos) != 2 || c.base != "1.0" || c.token != "github_pat_x" {
		t.Errorf("PRD repository %s, repositories %v on %s, token %q", c.prd.full(), c.repos, c.base, c.token)
	}
	if c.fiveHour != d.fiveHour || c.light != d.light || len(c.secrets) != 0 {
		t.Errorf("five-hour %v, light %v, secrets %v, want the defaults", c.fiveHour, c.light, c.secrets)
	}

	// The commented-out settings must read as the defaults once uncommented.
	var b strings.Builder
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "# "); ok && (strings.HasPrefix(rest, "  ") || strings.HasSuffix(rest, ":\n")) {
			line = rest
		}
		b.WriteString(line)
	}
	c, err = parseConfig([]byte(b.String()))
	if err != nil {
		t.Fatalf("uncommented: %v\n%s", err, b.String())
	}
	if c.fiveHour != d.fiveHour || c.weekly != d.weekly || c.projectLimit != d.projectLimit || c.readLimit != d.readLimit || c.reviewLimit != d.reviewLimit {
		t.Errorf("uncommented limits = %v %v %d %d %d, want the defaults", c.fiveHour, c.weekly, c.projectLimit, c.readLimit, c.reviewLimit)
	}
	if c.light != d.light || c.normal != d.normal || c.heavy != d.heavy || c.resolver != d.resolver {
		t.Errorf("uncommented models = %v %v %v %v, want the defaults", c.light, c.normal, c.heavy, c.resolver)
	}
	want := []sandbox.Secret{{Env: "NPM_TOKEN", Hosts: []string{"npm.pkg.github.com"}, Value: "npm_..."}}
	if !slices.EqualFunc(c.secrets, want, func(a, b sandbox.Secret) bool {
		return a.Env == b.Env && a.Value == b.Value && a.Command == b.Command && slices.Equal(a.Hosts, b.Hosts)
	}) {
		t.Errorf("uncommented secrets = %+v, want %+v", c.secrets, want)
	}
}

func TestSettingsFileWithOneRepositoryHasNoPRDRepository(t *testing.T) {
	data := settingsFile(answers{repos: []target{{owner: "acme", name: "app"}}, base: "main", token: "secret"})
	if strings.Contains(string(data), "\nprd_repo:") {
		t.Errorf("file has prd_repo:\n%s", data)
	}
}

func TestWriteSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".exomecha", "acme.yaml")
	a := answers{repos: []target{{owner: "acme", name: "app"}}, base: "main", token: "secret"}
	if err := writeSettings(path, a); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, %v, want 600", info.Mode(), err)
	}
	if err := writeSettings(path, a); err == nil {
		t.Error("a second write replaced the file")
	}
}
