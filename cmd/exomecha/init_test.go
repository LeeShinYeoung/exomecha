package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

func TestAsk(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("acme/api,acme/api\nacme/api, https://github.com/acme/web.git\nplans\ngit@github.com:acme/plans.git\n\n"))
	var out strings.Builder
	tokens := []string{"", " github_pat_x "}
	a, err := ask(t.Context(), in, &out, settings{}, func() (string, error) {
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

func TestParseTargetTakesCloneURLs(t *testing.T) {
	for _, repo := range []string{"acme/app", "https://github.com/acme/app", "https://github.com/acme/app.git", "https://github.com/acme/app/",
		"git@github.com:acme/app.git", "ssh://git@github.com/acme/app.git"} {
		if got, err := parseTarget("repos", repo); err != nil || got.full() != "acme/app" {
			t.Errorf("parseTarget(%q) = %q, %v, want acme/app", repo, got.full(), err)
		}
	}
	// exomecha talks to github.com only.
	if got, err := parseTarget("repos", "https://gitlab.com/acme/app.git"); err == nil {
		t.Errorf("parseTarget of a gitlab.com URL = %q, want an error", got.full())
	}
}

func TestAskOneRepositoryDoesNotAskForThePRDRepository(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("acme/app\ndevelop\n"))
	var out strings.Builder
	a, err := ask(t.Context(), in, &out, settings{}, func() (string, error) { return "secret", nil }, anyPRD)
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
	a, err := ask(t.Context(), in, &out, settings{}, func() (string, error) { return "secret", nil }, func(prd target) error {
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
	if _, err := ask(t.Context(), bufio.NewReader(strings.NewReader("acme/app\n")), &strings.Builder{}, settings{}, nil, anyPRD); err == nil {
		t.Error("ask with no base branch answer = nil")
	}
}

func TestAskKeepsTheFilesValuesOnEmptyAnswers(t *testing.T) {
	current := settings{Repos: []string{"acme/api", "acme/web"}, PRDRepo: "acme/plans", BaseBranch: "develop", GitHubToken: "github_pat_abcdefgh1234"}
	var out strings.Builder
	a, err := ask(t.Context(), bufio.NewReader(strings.NewReader("\n\n\n")), &out, current, func() (string, error) { return "", nil }, anyPRD)
	if err != nil {
		t.Fatal(err)
	}
	if !a.same(current) {
		t.Errorf("answers = %+v, want the file's %+v", a, current)
	}
	for _, want := range []string{"[acme/api, acme/web]", "[acme/plans]", "[develop]", "[...1234]"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not show %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), current.GitHubToken) {
		t.Errorf("output %q shows the whole token", out.String())
	}
}

func TestAskKeepsThePRDRepositoryOfOneRepository(t *testing.T) {
	current := settings{Repos: []string{"acme/app"}, PRDRepo: "acme/plans", BaseBranch: "main", GitHubToken: "secret"}
	var checked []string
	a, err := ask(t.Context(), bufio.NewReader(strings.NewReader("\n\n\n")), &strings.Builder{}, current, func() (string, error) { return "", nil }, func(prd target) error {
		checked = append(checked, prd.full())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.prd != "acme/plans" || !slices.Equal(checked, []string{"acme/plans"}) {
		t.Errorf("PRD repository %q, checked %v, want acme/plans alone", a.prd, checked)
	}
}

func TestAskStopsWhenCtxEnds(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ask(ctx, bufio.NewReader(r), &strings.Builder{}, settings{}, nil, anyPRD); !errors.Is(err, context.Canceled) {
		t.Errorf("ask after ctx ended = %v, want context.Canceled", err)
	}
}

func TestUpdateSettingsKeepsTheRest(t *testing.T) {
	a := answers{repos: []target{{owner: "acme", name: "app"}}, base: "main", token: "old"}
	data := strings.Replace(string(settingsFile(a)), "# limits:\n#   five_hour: 90", "limits:\n  five_hour: 50", 1)
	a.repos = append(a.repos, target{owner: "acme", name: "web"})
	a.prd, a.base, a.token = "acme/plans", "1.0", "new"
	got, err := updateSettings([]byte(data), a)
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig(got)
	if err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	if !slices.Equal(fullNames(c.repos), []string{"acme/app", "acme/web"}) || c.prd.full() != "acme/plans" || c.base != "1.0" || c.token != "new" {
		t.Errorf("repositories %v, PRD repository %s, base %s, token %q", fullNames(c.repos), c.prd.full(), c.base, c.token)
	}
	if c.fiveHour != 0.5 || !strings.Contains(string(got), "# Optional settings.") {
		t.Errorf("the rest of the file did not stay:\n%s", got)
	}
	if i, j := strings.Index(string(got), "\nprd_repo:"), strings.Index(string(got), "\nbase_branch:"); i < 0 || i > j {
		t.Errorf("prd_repo is not before base_branch:\n%s", got)
	}
}

func TestUpdateSettingsOfAnEmptyFileWritesANewOne(t *testing.T) {
	a := answers{repos: []target{{owner: "acme", name: "app"}}, base: "main", token: "secret"}
	got, err := updateSettings(nil, a)
	if err != nil || string(got) != string(settingsFile(a)) {
		t.Errorf("updateSettings(nil) = %v, %s", err, got)
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
	for _, data := range []string{"first", "second"} {
		if err := writeSettings(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "second" {
		t.Errorf("file = %q, %v, want the second write", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, %v, want 600", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("the folder holds %d files, want the settings alone", len(entries))
	}
}
