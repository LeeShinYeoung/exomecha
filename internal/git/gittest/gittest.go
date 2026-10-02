package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Setup points git at a throwaway global config, so tests never pick up the user's own
// settings (signing, hooks) and always have a commit identity.
func Setup(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	const content = "[user]\n\tname = exomecha test\n\temail = test@example.com\n"
	if err := os.WriteFile(cfg, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func Run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func NewOrigin(t *testing.T, files map[string]string) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	Run(t, "", "init", "-q", "--bare", "-b", "main", origin)
	Commit(t, origin, "main", "", files)
	return origin
}

// Commit commits files to branch in origin and returns the new commit.
// A branch that does not exist yet starts from `from`.
func Commit(t *testing.T, origin, branch, from string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	Run(t, "", "clone", "-q", origin, dir)
	switch {
	case Head(origin, branch) != "":
		Run(t, dir, "checkout", "-q", "-B", branch, "origin/"+branch)
	case from != "":
		Run(t, dir, "checkout", "-q", "-B", branch, "origin/"+from)
	default:
		Run(t, dir, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Run(t, dir, "add", "-A")
	Run(t, dir, "commit", "-q", "-m", "commit on "+branch)
	Run(t, dir, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	return Run(t, dir, "rev-parse", "HEAD")
}

// CommitAll commits every change in the working tree at dir, the way the AI commits its work.
func CommitAll(t *testing.T, dir, message string) {
	t.Helper()
	Run(t, dir, "add", "-A")
	Run(t, dir, "commit", "-q", "-m", message)
}

// Head returns the commit branch points at in origin, or "" if there is no such branch.
// It never fails the test, so a fake server can call it from its own goroutine.
func Head(origin, branch string) string {
	out, err := run(origin, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return out
}

func Parents(t *testing.T, origin, ref string) []string {
	t.Helper()
	return strings.Fields(Run(t, origin, "rev-list", "--parents", "-n", "1", ref))[1:]
}

func ReadFile(t *testing.T, origin, ref, path string) string {
	t.Helper()
	return Run(t, origin, "show", ref+":"+path)
}
