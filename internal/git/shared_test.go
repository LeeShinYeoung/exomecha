package git_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/coworker/internal/git/gittest"
)

// sandboxRepo stands in for the repository inside a sandbox. Tests reach it by its path.
func sandboxRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	gittest.Run(t, "", "init", "-q", dir)
	return dir
}

func (f fixture) rev(t *testing.T, ref string) string {
	t.Helper()
	commit, ok, err := f.git.Rev(t.Context(), ref)
	if err != nil || !ok {
		t.Fatalf("Rev(%s) = %q, %v, %v", ref, commit, ok, err)
	}
	return commit
}

func TestSendToAndFetchFrom(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	if err := f.git.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	base := f.rev(t, "origin/main")
	box := sandboxRepo(t)

	if err := f.git.SendTo(t.Context(), box, base, "coworker/task-1", base); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/remotes/origin/main", "refs/heads/coworker/task-1"} {
		if got := gittest.Run(t, box, "rev-parse", ref); got != base {
			t.Errorf("%s in the sandbox = %q, want %s", ref, got, base)
		}
	}
	gittest.Run(t, box, "checkout", "-q", "coworker/task-1")
	writeFile(t, filepath.Join(box, "new.txt"), "new\n")
	gittest.CommitAll(t, box, "add new.txt")

	head, err := f.git.FetchFrom(t.Context(), box, "coworker-task-x-1-builder")
	if err != nil {
		t.Fatal(err)
	}
	if want := gittest.Run(t, box, "rev-parse", "HEAD"); head != want {
		t.Errorf("FetchFrom = %s, want the sandbox HEAD %s", head, want)
	}
	if yes, err := f.git.IsAncestor(t.Context(), base, head); err != nil || !yes {
		t.Errorf("IsAncestor(base, head) = %v, %v, want true", yes, err)
	}
	if yes, err := f.git.IsAncestor(t.Context(), head, base); err != nil || yes {
		t.Errorf("IsAncestor(head, base) = %v, %v, want false", yes, err)
	}
	if got, ok, err := f.git.Show(t.Context(), head, "new.txt"); err != nil || !ok || got != "new\n" {
		t.Errorf("Show(new.txt) = %q, %v, %v", got, ok, err)
	}
	if got, ok, err := f.git.Show(t.Context(), base, "new.txt"); err != nil || ok {
		t.Errorf("Show(new.txt) on base = %q, %v, %v, want no file", got, ok, err)
	}

	if err := f.git.DropRef(t.Context(), "coworker-task-x-1-builder"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.git.Rev(t.Context(), "refs/coworker/coworker-task-x-1-builder"); err != nil || ok {
		t.Errorf("ref left after DropRef: %v, %v", ok, err)
	}
}

// The sandbox side of a transfer is a command git runs from the URL. It must not find the token
// in its environment.
func TestSandboxTransfersCarryNoAuth(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	if err := f.git.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	base := f.rev(t, "origin/main")
	box := sandboxRepo(t)
	dump := filepath.Join(t.TempDir(), "env")
	command := filepath.Join(t.TempDir(), "sandbox-git")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nenv >> '"+dump+"'\nexec git \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	url := "ext::" + command + " %s " + box

	if err := f.git.SendTo(t.Context(), url, base, "coworker/task-1", base); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, box, "checkout", "-q", "coworker/task-1")
	if _, err := f.git.FetchFrom(t.Context(), url, "box"); err != nil {
		t.Fatal(err)
	}

	env := readFile(t, dump)
	if strings.Count(env, "PATH=") < 2 {
		t.Fatalf("the URL's command did not run twice:\n%s", env)
	}
	if strings.Contains(env, "AUTHORIZATION") || strings.Contains(env, "extraheader") {
		t.Errorf("the sandbox side of a transfer saw the auth header:\n%s", env)
	}
}

func TestPushCommit(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	old := gittest.Commit(t, f.origin, "coworker/task-1", "main", map[string]string{"old.txt": "old\n"})
	if err := f.git.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	base := f.rev(t, "origin/main")
	box := sandboxRepo(t)
	if err := f.git.SendTo(t.Context(), box, base, "coworker/task-1", base); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, box, "checkout", "-q", "coworker/task-1")
	writeFile(t, filepath.Join(box, "new.txt"), "new\n")
	gittest.CommitAll(t, box, "new work")
	head, err := f.git.FetchFrom(t.Context(), box, "box")
	if err != nil {
		t.Fatal(err)
	}

	if err := f.git.PushCommit(t.Context(), head, "coworker/task-1", false); err == nil {
		t.Error("PushCommit without force replaced a branch that does not hold the commit")
	}
	if got := gittest.Head(f.origin, "coworker/task-1"); got != old {
		t.Fatalf("branch = %s, want it left at %s", got, old)
	}
	if err := f.git.PushCommit(t.Context(), head, "coworker/task-1", true); err != nil {
		t.Fatal(err)
	}
	if got := gittest.Parents(t, f.origin, "coworker/task-1"); !slices.Equal(got, []string{base}) {
		t.Errorf("parents = %v, want only main %s (old branch replaced)", got, base)
	}
}

// coworker's own push carries the token in its environment, so it must not run the project's hooks.
func TestPushCommitDoesNotRunProjectHooks(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	if err := f.git.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, filepath.Join(f.root, "repo"), "config", "core.hooksPath", hooks)

	if err := f.git.PushCommit(t.Context(), f.rev(t, "origin/main"), "coworker/task-1", false); err != nil {
		t.Fatalf("PushCommit ran the project's pre-push hook: %v", err)
	}
}

func TestConflictMarkers(t *testing.T) {
	f := setup(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	commit := gittest.Commit(t, f.origin, "main", "", map[string]string{
		"한글.txt": "<<<<<<< HEAD\nmine\n=======\ntheirs\n>>>>>>> origin/main\n",
		"b.txt":  "resolved\n",
	})
	if err := f.git.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}

	left, err := f.git.ConflictMarkers(t.Context(), commit, []string{"한글.txt", "b.txt", "gone.txt"})
	if err != nil || !slices.Equal(left, []string{"한글.txt"}) {
		t.Errorf("ConflictMarkers = %q, %v, want only 한글.txt", left, err)
	}
}

func TestOpenBranch(t *testing.T) {
	f := setup(t, map[string]string{"README.md": "hello\n"})
	gittest.Commit(t, f.origin, "feature", "main", map[string]string{"a.txt": "a\n"})

	wt, err := f.git.OpenBranch(t.Context(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Remove()
	if got := readFile(t, filepath.Join(wt.Dir, "a.txt")); got != "a\n" {
		t.Errorf("worktree is not on the feature branch: a.txt = %q", got)
	}
}
