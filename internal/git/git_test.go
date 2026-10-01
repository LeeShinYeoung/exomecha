package git_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/coworker/internal/git"
	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
)

type fixture struct {
	git    *git.Git
	root   string
	origin string
}

func setup(t *testing.T, files map[string]string) fixture {
	t.Helper()
	gittest.Setup(t)
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

func TestOpenAndRemove(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	wt, err := f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(wt.Dir, "README.md")); got != "hello\n" {
		t.Errorf("README.md = %q, want %q", got, "hello\n")
	}

	wt.Remove()
	if _, err := os.Stat(wt.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("worktree dir still exists: %v", err)
	}
	list := gittest.Run(t, filepath.Join(f.root, "repo"), "worktree", "list", "--porcelain")
	if n := strings.Count(list, "worktree "); n != 1 {
		t.Errorf("git worktree list has %d entries, want only the main clone:\n%s", n, list)
	}
}

func TestOpenFetchesLatest(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	wt, err := f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wt.Remove()

	gittest.Commit(t, f.origin, "main", "", map[string]string{"new.txt": "new\n"})
	wt, err = f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	if got := readFile(t, filepath.Join(wt.Dir, "new.txt")); got != "new\n" {
		t.Errorf("new.txt = %q, want the commit pushed after the first open", got)
	}
}

func TestBaseFile(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n", "conf/a.txt": "base\n"})
	gittest.Commit(t, f.origin, "feature", "main", map[string]string{"conf/a.txt": "feature\n", "conf/b.txt": "feature\n"})

	wt, err := f.git.OpenBranch(t.Context(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	if got, ok, err := wt.BaseFile(t.Context(), "conf/a.txt"); err != nil || !ok || got != "base\n" {
		t.Errorf("BaseFile(conf/a.txt) = %q, %v, %v, want the base branch's copy", got, ok, err)
	}
	if got, ok, err := wt.BaseFile(t.Context(), "conf/b.txt"); err != nil || ok {
		t.Errorf("BaseFile(conf/b.txt) = %q, %v, %v, want nothing for a file only the branch has", got, ok, err)
	}
	if got, ok, err := wt.BaseFile(t.Context(), "conf/missing.txt"); err != nil || ok {
		t.Errorf("BaseFile(conf/missing.txt) = %q, %v, %v, want nothing for a file no branch has", got, ok, err)
	}
}

func TestBaseFileOfADirectoryIsNoFile(t *testing.T) {
	f := setup(t, map[string]string{"conf/a.txt": "base\n"})
	wt, err := f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	if got, ok, err := wt.BaseFile(t.Context(), "conf"); err != nil || ok {
		t.Errorf("BaseFile(conf) = %q, %v, %v, want nothing for a directory", got, ok, err)
	}
}

func TestGitBaseFile(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n", "conf/a.txt": "base\n"})
	gittest.Commit(t, f.origin, "feature", "main", map[string]string{"conf/b.txt": "feature\n"})
	wt, err := f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wt.Remove()

	if got, ok, err := f.git.BaseFile(t.Context(), "conf/a.txt"); err != nil || !ok || got != "base\n" {
		t.Errorf("BaseFile(conf/a.txt) = %q, %v, %v, want the base branch's copy", got, ok, err)
	}
	if got, ok, err := f.git.BaseFile(t.Context(), "conf/b.txt"); err != nil || ok {
		t.Errorf("BaseFile(conf/b.txt) = %q, %v, %v, want nothing for a file only another branch has", got, ok, err)
	}
	if got, ok, err := f.git.BaseFile(t.Context(), "conf/missing.txt"); err != nil || ok {
		t.Errorf("BaseFile(conf/missing.txt) = %q, %v, %v, want nothing for a missing file", got, ok, err)
	}
	if got, ok, err := f.git.BaseFile(t.Context(), "conf"); err != nil || ok {
		t.Errorf("BaseFile(conf) = %q, %v, %v, want nothing for a directory", got, ok, err)
	}
}

func TestGitBaseFileReadsTheLastFetch(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	wt, err := f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wt.Remove()
	gittest.Commit(t, f.origin, "main", "", map[string]string{"README.md": "changed\n"})

	if got, ok, err := f.git.BaseFile(t.Context(), "README.md"); err != nil || !ok || got != "hello\n" {
		t.Errorf("BaseFile before a fetch = %q, %v, %v, want the fetched copy", got, ok, err)
	}
	wt, err = f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wt.Remove()
	if got, ok, err := f.git.BaseFile(t.Context(), "README.md"); err != nil || !ok || got != "changed\n" {
		t.Errorf("BaseFile after a fetch = %q, %v, %v, want the new commit", got, ok, err)
	}
}
