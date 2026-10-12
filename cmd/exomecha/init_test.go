package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

func anyPRD(target) error { return nil }

func TestQuestionsStartFromTheFile(t *testing.T) {
	current := settings{Repos: []string{"acme/api", "acme/web"}, PRDRepo: "acme/plans", BaseBranch: "develop", GitHubToken: "github_pat_x"}
	if a := newQuestions(current, anyPRD).answers(); !a.same(current) {
		t.Errorf("answers = %+v, want the file's %+v", a, current)
	}
	if q := newQuestions(settings{}, anyPRD); q.base != "main" || q.repos != "" || q.token != "" {
		t.Errorf("questions without a file = %+v, want base main and nothing else", q)
	}
}

func TestQuestionsCheckRepos(t *testing.T) {
	var checked []string
	q := newQuestions(settings{}, func(prd target) error {
		checked = append(checked, prd.full())
		if prd.full() == "acme/plans" {
			return errors.New("project plans already has acme/plans")
		}
		return nil
	})
	for s, want := range map[string]string{
		"acme/api,acme/api": "repos lists acme/api twice",
		"plans":             "repos must hold owner/name",
		"acme/plans":        "project plans already has acme/plans",
		"":                  "repos must hold owner/name",
	} {
		if err := q.checkRepos(s); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("checkRepos(%q) = %v, want %q", s, err, want)
		}
	}
	checked = nil
	if err := q.checkRepos("acme/api, https://github.com/acme/web.git"); err != nil || checked != nil {
		t.Errorf("checkRepos of two repositories = %v, checked %v, want no PRD repository check", err, checked)
	}
	if err := q.checkPRDRepo("git@github.com:acme/plans.git"); err == nil {
		t.Error("checkPRDRepo of another project's repository = nil")
	}
}

// One repository holds the PRDs itself, unless the file gives it a PRD repository of its own.
func TestQuestionsAsksPRD(t *testing.T) {
	q := newQuestions(settings{}, anyPRD)
	q.repos = "acme/app"
	if q.asksPRD() {
		t.Error("asksPRD with one repository = true")
	}
	q.repos = "acme/api, acme/web"
	if !q.asksPRD() {
		t.Error("asksPRD with two repositories = false")
	}
	q = newQuestions(settings{Repos: []string{"acme/app"}, PRDRepo: "acme/plans"}, anyPRD)
	if !q.asksPRD() {
		t.Error("asksPRD with one repository and a PRD repository in the file = false")
	}
	var checked []string
	q.checkPRD = func(prd target) error {
		checked = append(checked, prd.full())
		return nil
	}
	if err := q.checkRepos("acme/app"); err != nil || checked != nil {
		t.Errorf("checkRepos = %v, checked %v, want the PRD repository question to check it", err, checked)
	}
}

func TestQuestionsAnswers(t *testing.T) {
	q := newQuestions(settings{}, anyPRD)
	q.repos, q.prd, q.base, q.token = "acme/api, https://github.com/acme/web.git", "git@github.com:acme/plans.git", " develop ", " github_pat_x "
	a := q.answers()
	if !slices.Equal(fullNames(a.repos), []string{"acme/api", "acme/web"}) || a.prd != "acme/plans" || a.base != "develop" || a.token != "github_pat_x" {
		t.Errorf("answers = %+v", a)
	}
	q.repos = "acme/app"
	if a := q.answers(); a.prd != "" {
		t.Errorf("answers with one repository has PRD repository %q, want none", a.prd)
	}
}

func TestReport(t *testing.T) {
	var ran []string
	pass := func(name string) func(context.Context) (string, []error) {
		return func(context.Context) (string, []error) {
			ran = append(ran, name)
			return name + " passed", nil
		}
	}
	fail := func(name string) func(context.Context) (string, []error) {
		return func(context.Context) (string, []error) {
			ran = append(ran, name)
			return name + " failed", []error{errors.New("fix " + name + "\nwith two lines")}
		}
	}
	var out strings.Builder
	ok, err := report(t.Context(), &out, false, []item{
		{group: "A", run: pass("a1")},
		{group: "A", run: fail("a2")},
		{group: "B", gate: true, run: fail("b1")},
		{group: "B", run: pass("b2")},
		{group: "C", run: pass("c1")},
	})
	if err != nil || ok {
		t.Errorf("report = %v, %v, want not ok", ok, err)
	}
	if !slices.Equal(ran, []string{"a1", "a2", "b1", "c1"}) {
		t.Errorf("ran %v, want b2 skipped after its group's gate failed", ran)
	}
	want := "\nA\n✓ a1 passed\n✗ a2 failed\n  fix a2\n  with two lines\n\nB\n✗ b1 failed\n  fix b1\n  with two lines\n\nC\n✓ c1 passed\n"
	if got := out.String(); got != want {
		t.Errorf("report printed\n%s\nwant\n%s", got, want)
	}

	if ok, err := report(t.Context(), &strings.Builder{}, false, []item{{group: "A", run: pass("a1")}}); err != nil || !ok {
		t.Errorf("report of passing items = %v, %v, want ok", ok, err)
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
