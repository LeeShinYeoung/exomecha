package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestList(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".exomecha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("acme.yaml", "repos: [acme/api, acme/web]\nprd_repo: acme/plans\nbase_branch: main\ngithub_token: secret\n")
	write("side.yaml", "repos: [me/side]\nbase_branch: main\ngithub_token: secret\n")
	write("broken.yaml", "repos: [me/broken]\n")
	root := projectRoot(home, target{owner: "acme", name: "plans"})
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := lock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	var out strings.Builder
	if err := list(home, &out); err != nil {
		t.Fatal(err)
	}
	var rows [][]string
	for line := range strings.Lines(out.String()) {
		rows = append(rows, strings.Fields(line))
	}
	want := [][]string{
		{"NAME", "PRD", "REPO", "RUNNING"},
		{"acme", "acme/plans", "yes"},
		{"broken", "-", "-"},
		{"side", "me/side", "no"},
	}
	if len(rows) != len(want)+1 || !slices.EqualFunc(rows[:len(want)], want, slices.Equal) || !strings.Contains(out.String(), "broken.yaml: repos, base_branch and github_token") {
		t.Errorf("list printed:\n%s", out.String())
	}
	if _, err := os.Stat(projectRoot(home, target{owner: "me", name: "side"})); !os.IsNotExist(err) {
		t.Errorf("list made a folder for side: %v", err)
	}
}

func TestListWithNoProjects(t *testing.T) {
	var out strings.Builder
	if err := list(t.TempDir(), &out); err != nil || !strings.Contains(out.String(), "exomecha init") {
		t.Errorf("list = %q, %v", out.String(), err)
	}
}
