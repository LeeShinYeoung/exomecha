package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/git"
	"github.com/LeeShinYeoung/exomecha/internal/git/gittest"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// TestRealSandbox runs the whole round trip against the sbx on this machine: create, send code
// in, run a detached command that commits, fetch the commit out, remove. It needs sbx signed in
// and takes about 15 seconds, so it runs only with EXOMECHA_TEST_SBX=1.
func TestRealSandbox(t *testing.T) {
	if os.Getenv("EXOMECHA_TEST_SBX") != "1" {
		t.Skip("set EXOMECHA_TEST_SBX=1 to run against the real sbx")
	}
	gittest.Setup(t)
	sbx := sandbox.CLI{}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	name := fmt.Sprintf("impl-run-test-%d", time.Now().UnixNano()%1_000_000)
	workspace := t.TempDir()
	t.Cleanup(func() {
		if err := sbx.Remove(context.Background(), name); err != nil {
			t.Errorf("remove %s: %v", name, err)
		}
	})

	if err := sbx.Create(ctx, name, "", workspace, []string{"ssh.github.com"}); err != nil {
		t.Fatal(err)
	}
	const repo = "/home/agent/repo"
	if _, err := sbx.Exec(ctx, name, "", nil, "git", "init", "-q", repo); err != nil {
		t.Fatal(err)
	}
	origin := gittest.NewOrigin(t, map[string]string{"README.md": "hello\n"})
	g := git.NewAt(t.TempDir(), origin, "main", "token")
	if err := g.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	base, _, err := g.Rev(ctx, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	url := sbx.GitURL(name, repo)
	if err := g.SendTo(ctx, url, base, "exomecha/prd-1", base); err != nil {
		t.Fatal(err)
	}
	if _, err := sbx.Exec(ctx, name, repo, nil, "git", "checkout", "-q", "exomecha/prd-1"); err != nil {
		t.Fatal(err)
	}
	if out, err := sbx.Exec(ctx, name, repo, nil, "git", "remote", "-v"); err != nil || len(out) != 0 {
		t.Errorf("git remote -v in the sandbox = %q, %v, want no remote", out, err)
	}

	exit := filepath.Join(workspace, "exit")
	script := "sleep 2; (echo new > new.txt && git add new.txt && git -c user.name=test -c user.email=test@example.com commit -qm work) > " + exit + ".log 2>&1; echo $? > " + exit
	if err := sbx.Start(ctx, name, repo, "sh", "-c", script); err != nil {
		t.Fatal(err)
	}
	for {
		b, err := os.ReadFile(exit)
		if err == nil && strings.TrimSpace(string(b)) == "0" {
			break
		}
		if err == nil {
			log, _ := os.ReadFile(exit + ".log")
			t.Fatalf("the detached command failed: exit %s: %s", b, log)
		}
		select {
		case <-ctx.Done():
			t.Fatal("the detached command never wrote its exit file")
		case <-time.After(200 * time.Millisecond):
		}
	}

	head, err := g.FetchFrom(ctx, url, name)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := g.Show(ctx, head, "new.txt"); err != nil || !ok || got != "new\n" {
		t.Errorf("new.txt in the fetched commit = %q, %v, %v", got, ok, err)
	}
	if yes, err := g.IsAncestor(ctx, base, head); err != nil || !yes {
		t.Errorf("fetched commit does not build on base: %v, %v", yes, err)
	}

	code, err := sbx.Exec(ctx, name, "", nil, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "https://ssh.github.com/")
	if err != nil || string(code) != "403" {
		t.Errorf("ssh.github.com from the sandbox = %q, %v, want 403", code, err)
	}

	if err := sbx.Remove(ctx, name); err != nil {
		t.Fatal(err)
	}
	boxes, err := sbx.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range boxes {
		if b.Name == name {
			t.Errorf("%s still listed after Remove: %+v", name, b)
		}
	}
}
