package git_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

func TestNewBranchOverwritesOldRemoteBranch(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	gittest.Commit(t, f.origin, "coworker/task-1", "main", map[string]string{"old.txt": "old\n"})
	mainSHA := gittest.Head(f.origin, "main")

	wt, err := f.git.NewBranch(t.Context(), "coworker/task-1")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	writeFile(t, filepath.Join(wt.Dir, "new.txt"), "new\n")
	gittest.CommitAll(t, wt.Dir, "new work")
	if err := wt.Push(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := gittest.Parents(t, f.origin, "coworker/task-1"); !slices.Equal(got, []string{mainSHA}) {
		t.Errorf("parents = %v, want only main %s (old branch overwritten)", got, mainSHA)
	}
}

func TestOpenBranchPushStacks(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	prev := gittest.Commit(t, f.origin, "feature", "main", map[string]string{"a.txt": "a\n"})

	wt, err := f.git.OpenBranch(t.Context(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	if got := readFile(t, filepath.Join(wt.Dir, "a.txt")); got != "a\n" {
		t.Fatalf("worktree is not on the feature branch: a.txt = %q", got)
	}
	writeFile(t, filepath.Join(wt.Dir, "b.txt"), "b\n")
	gittest.CommitAll(t, wt.Dir, "more work")
	if err := wt.Push(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := gittest.Parents(t, f.origin, "feature"); !slices.Equal(got, []string{prev}) {
		t.Errorf("parents = %v, want the previous branch head %s", got, prev)
	}
}

func TestReadOnlyWorktreeRefusesPush(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	wt, err := f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	if err := wt.Push(t.Context()); err == nil {
		t.Error("Push succeeded on a read-only worktree")
	}
}

func TestHasChanges(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	wt, err := f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()

	if changed, err := wt.HasChanges(t.Context()); err != nil || changed {
		t.Fatalf("HasChanges on a fresh worktree = %v, %v", changed, err)
	}
	writeFile(t, filepath.Join(wt.Dir, "new.txt"), "new\n")
	if changed, err := wt.HasChanges(t.Context()); err != nil || !changed {
		t.Errorf("HasChanges after adding a file = %v, %v", changed, err)
	}
}

func TestHeadAndIsAncestor(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	wt, err := f.git.OpenBase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()

	before, err := wt.Head(t.Context())
	if err != nil || before != gittest.Head(f.origin, "main") {
		t.Fatalf("Head = %q, %v, want the main head", before, err)
	}
	writeFile(t, filepath.Join(wt.Dir, "new.txt"), "new\n")
	gittest.CommitAll(t, wt.Dir, "add new.txt")
	after, err := wt.Head(t.Context())
	if err != nil || after == before {
		t.Fatalf("Head after a commit = %q, %v", after, err)
	}
	if yes, err := wt.IsAncestor(t.Context(), before, after); err != nil || !yes {
		t.Errorf("IsAncestor(before, after) = %v, %v, want true", yes, err)
	}
	if yes, err := wt.IsAncestor(t.Context(), after, before); err != nil || yes {
		t.Errorf("IsAncestor(after, before) = %v, %v, want false", yes, err)
	}
}

func TestStartMergeConflict(t *testing.T) {
	f := setup(t, map[string]string{"한글.txt": "base\n", "gone.txt": "base\n"})
	gittest.Commit(t, f.origin, "feature", "main", map[string]string{"한글.txt": "feature\n", "gone.txt": "feature\n"})
	gittest.Commit(t, f.origin, "main", "", map[string]string{"한글.txt": "main\n", "gone.txt": "main\n"})

	wt, err := f.git.OpenBranch(t.Context(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	conflicts, err := wt.StartMerge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(conflicts)
	want := []string{"gone.txt", "한글.txt"}
	if !slices.Equal(conflicts, want) {
		t.Fatalf("conflicts = %q, want %q", conflicts, want)
	}
	if left, err := wt.ConflictMarkers(conflicts); err != nil || len(left) != 2 {
		t.Fatalf("ConflictMarkers before resolving = %q, %v", left, err)
	}

	writeFile(t, filepath.Join(wt.Dir, "한글.txt"), "main\nfeature\n")
	if err := os.Remove(filepath.Join(wt.Dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if left, err := wt.ConflictMarkers(conflicts); err != nil || len(left) != 0 {
		t.Fatalf("ConflictMarkers after resolving = %q, %v", left, err)
	}
	wantMerging(t, wt, true)
	gittest.Run(t, wt.Dir, "add", "-A")
	gittest.Run(t, wt.Dir, "commit", "-q", "--no-edit")
	wantMerging(t, wt, false)
	if err := wt.Push(t.Context()); err != nil {
		t.Fatal(err)
	}

	if n := len(gittest.Parents(t, f.origin, "feature")); n != 2 {
		t.Errorf("pushed head has %d parents, want a merge commit", n)
	}
	if got := gittest.ReadFile(t, f.origin, "feature", "한글.txt"); got != "main\nfeature" {
		t.Errorf("한글.txt on feature = %q", got)
	}
}

func TestStartMergeCleanStopsBeforeCommit(t *testing.T) {
	f := setup(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	head := gittest.Commit(t, f.origin, "feature", "main", map[string]string{"a.txt": "feature\n"})
	gittest.Commit(t, f.origin, "main", "", map[string]string{"b.txt": "main\n"})

	wt, err := f.git.OpenBranch(t.Context(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	conflicts, err := wt.StartMerge(t.Context())
	if err != nil || conflicts != nil {
		t.Fatalf("StartMerge = %q, %v, want a clean merge", conflicts, err)
	}
	wantMerging(t, wt, true)
	if got, err := wt.Head(t.Context()); err != nil || got != head {
		t.Errorf("Head after StartMerge = %q, %v, want no commit yet (%s)", got, err, head)
	}
}

func TestStartMergeNothingToMerge(t *testing.T) {
	f := setup(t, map[string]string{"a.txt": "a\n"})
	gittest.Commit(t, f.origin, "feature", "main", map[string]string{"a.txt": "feature\n"})

	wt, err := f.git.OpenBranch(t.Context(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	if conflicts, err := wt.StartMerge(t.Context()); err != nil || conflicts != nil {
		t.Fatalf("StartMerge = %q, %v", conflicts, err)
	}
	wantMerging(t, wt, false)
}

func wantMerging(t *testing.T, wt *git.Worktree, want bool) {
	t.Helper()
	if got, err := wt.MergeInProgress(t.Context()); err != nil || got != want {
		t.Errorf("MergeInProgress = %v, %v, want %v", got, err, want)
	}
}
