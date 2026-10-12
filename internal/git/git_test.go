package git_test

import (
	"os"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/git"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
)

type fixture struct {
	git    *git.Git
	root   string
	origin string
}

func TestMain(m *testing.M) { gittest.Main(m) }

func setup(t *testing.T, files map[string]string) fixture {
	t.Helper()
	origin := gittest.NewOrigin(t, files)
	root := t.TempDir()
	return fixture{git: git.NewAt(root, origin, "main", "token"), root: root, origin: origin}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShowBase(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n", "conf/a.txt": "base\n"})
	gittest.Commit(t, f.origin, "feature", "main", map[string]string{"conf/a.txt": "feature\n", "conf/b.txt": "feature\n"})
	if err := f.git.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got, ok, err := f.git.Show(t.Context(), f.git.BaseRef(), "conf/a.txt"); err != nil || !ok || got != "base\n" {
		t.Errorf("Show(conf/a.txt) = %q, %v, %v, want the base branch's copy", got, ok, err)
	}
	if got, ok, err := f.git.Show(t.Context(), f.git.BaseRef(), "conf/b.txt"); err != nil || ok {
		t.Errorf("Show(conf/b.txt) = %q, %v, %v, want nothing for a file only another branch has", got, ok, err)
	}
	if got, ok, err := f.git.Show(t.Context(), f.git.BaseRef(), "conf/missing.txt"); err != nil || ok {
		t.Errorf("Show(conf/missing.txt) = %q, %v, %v, want nothing for a missing file", got, ok, err)
	}
	if got, ok, err := f.git.Show(t.Context(), f.git.BaseRef(), "conf"); err != nil || ok {
		t.Errorf("Show(conf) = %q, %v, %v, want nothing for a directory", got, ok, err)
	}
}
