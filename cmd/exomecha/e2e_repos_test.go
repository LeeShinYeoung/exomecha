package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/github/githubtest"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// codeRepo is a repository PRDs may change, with a GitHub and an origin of its own, apart from the
// world's PRD repository.
type codeRepo struct {
	name   string
	gh     *githubtest.Server
	origin string
}

// addCodeRepo makes a repository PRDs may change. The world's own repository stays the PRD
// repository, and it is a code repository only while cfg.repos lists it.
func (w *world) addCodeRepo(short string, files map[string]string) *codeRepo {
	w.t.Helper()
	r := &codeRepo{name: short + "-" + randomHex(3), origin: filepath.Join(w.t.TempDir(), "origin.git")}
	w.git("", "init", "-q", "--bare", "-b", "main", r.origin)
	saved := w.origin
	w.origin = r.origin
	w.push("main", "", "Start the project", files)
	w.origin = saved
	r.gh = githubtest.New(w.t)
	r.gh.Owner, r.gh.Repo, r.gh.UserName = e2eOwner, r.name, "E2E Person"
	r.gh.Live(r.origin, "main")
	w.cfg.repos = append(w.cfg.repos, target{owner: e2eOwner, name: r.name, apiURL: r.gh.URL, gitURL: r.origin})
	w.mu.Lock()
	w.others = append(w.others, r)
	w.mu.Unlock()
	return r
}

func (r *codeRepo) full() string { return e2eOwner + "/" + r.name }

func (r *codeRepo) openPR(prd int) (githubtest.Pull, bool) {
	for _, p := range r.gh.Pulls() {
		if p.Head == branchOf(prd) && p.Open {
			return p, true
		}
	}
	return githubtest.Pull{}, false
}

func (r *codeRepo) commitMessage(ref string) string {
	out, err := exec.Command("git", "-C", r.origin, "log", "-1", "--format=%B", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const docsMD = `# docs

The greet tool's documentation, as plain Markdown. There is nothing to install, build or test.

- Keep USAGE.md in step with what greet.sh does.
`

func docs() map[string]string {
	return map[string]string{
		"CLAUDE.md": docsMD,
		"USAGE.md":  "# Usage\n\n- `sh greet.sh` prints `Hello, world!`.\n- `sh greet.sh <name>` prints `Hello, <name>!`.\n",
	}
}

func shoutAcrossPRD(greet, docs *codeRepo) prdText {
	return prdText{"Add a --shout flag and document it", fmt.Sprintf(`## Repositories

- %s
- %s

## To do

- In %s, `+"`sh greet.sh --shout`"+` prints the greeting in capital letters: `+"`HELLO, WORLD!`"+`. `+"`sh greet.sh --shout Ada`"+` prints `+"`HELLO, ADA!`"+`. Without the flag nothing changes.
- In %s, add a line to USAGE.md that says what `+"`--shout`"+` does.

## Acceptance criteria

- The two commands above print exactly those lines, `+"`tests/greet_test.sh`"+` tests both, and `+"`sh test.sh`"+` prints `+"`ok`"+`.
- USAGE.md mentions `+"`--shout`"+`.

## Files

- %s/greet.sh
- %s/tests/greet_test.sh
- %s/USAGE.md
`, greet.full(), docs.full(), greet.name, docs.name, greet.name, greet.name, docs.name)}
}

// A PRD changes two repositories from a PRD repository that holds only the PRDs and the sandbox's
// files. Each repository gets a PR, reviewer approves both, and the PRD closes after both merge.
func TestE2ETwoRepositories(t *testing.T) {
	e2e(t, false)
	const grace = 10 * time.Second
	setup := `printf '%s\n' "$EXOMECHA_REPOS" > /home/agent/repos.txt
printf '%s' "$E2E_TOKEN" > /home/agent/token.txt
`
	w := newWorld(t, "multi", map[string]string{
		"README.md":            "# plans\n\nThe PRDs of the greet project.\n",
		".exomecha/setup.sh":   setup,
		".exomecha/sandbox.md": "Run each repository's tests from its own folder.\n",
	}, func(c *config) { c.ciGrace = grace })
	secret := sandbox.Secret{Env: "E2E_TOKEN", Hosts: []string{"example.com"}, Command: "/bin/echo e2e-not-a-secret"}
	w.cfg.repos = nil
	w.cfg.secrets = []sandbox.Secret{secret}
	greet := w.addCodeRepo("greet", project())
	docs := w.addCodeRepo("docs", docs())
	w.start()

	p := shoutAcrossPRD(greet, docs)
	prd := w.gh.AddIssue(p.title, p.body, label.PRD, string(label.Ready))
	w.track(prd)
	builder := w.box(prd, "builder")
	w.waitFor("the builder to start claude", aStart, func() bool { return w.started(builder) })

	repos, err := w.exec(builder, "cat", "/home/agent/repos.txt")
	wantRepos := greet.full() + " /home/agent/repo/" + greet.name + "\n" + docs.full() + " /home/agent/repo/" + docs.name + "\n"
	if err != nil || repos != wantRepos {
		t.Errorf("setup's EXOMECHA_REPOS = %q, %v, want %q", repos, err, wantRepos)
	}
	if token, err := w.exec(builder, "cat", "/home/agent/token.txt"); err != nil || token != secret.Placeholder() {
		t.Errorf("setup's E2E_TOKEN = %q, %v, want the placeholder", token, err)
	}
	if !secretListed(t, builder, secret) {
		t.Errorf("sbx keeps no %s for %s", secret.Env, builder)
	}
	input := w.input(builder)
	for _, want := range []string{"## Sandbox rules", "Run each repository's tests from its own folder.", "### `" + greet.name + "/`: " + greet.full(),
		"end with `PRD: " + w.cfg.prd.full() + "#" + strconv.Itoa(prd) + "`"} {
		if !strings.Contains(input, want) {
			t.Errorf("builder's input has no %q:\n%s", want, input)
		}
	}

	w.waitFor("a PR in each repository", aRun, func() bool {
		_, a := greet.openPR(prd)
		_, b := docs.openPR(prd)
		return a && b
	})
	w.waitFor("the builder's sandbox to go", aRound, func() bool { return !w.exists(builder) })
	w.wantStatus(prd, label.Review)
	if secretListed(t, builder, secret) {
		t.Errorf("sbx still keeps %s after %s went", secret.Env, builder)
	}
	ref := w.cfg.prd.full() + "#" + strconv.Itoa(prd)
	for _, r := range []*codeRepo{greet, docs} {
		pr, _ := r.openPR(prd)
		if !strings.HasPrefix(pr.Body, "Part of "+ref+"\n") {
			t.Errorf("%s PR body = %q, want it to be part of %s", r.name, pr.Body, ref)
		}
		if msg := r.commitMessage(branchOf(prd)); !strings.HasSuffix(msg, "PRD: "+ref) {
			t.Errorf("%s commit message %q does not end with the PRD", r.name, msg)
		}
	}
	if !strings.Contains(fileAt(t, docs.origin, branchOf(prd), "USAGE.md"), "--shout") {
		t.Error("USAGE.md on the docs branch does not mention --shout")
	}

	reviewer := w.box(prd, "reviewer")
	w.waitFor("the reviewer to start claude", grace+aStart, func() bool { return w.started(reviewer) })
	w.waitFor("the review", aRun, func() bool { return w.status(prd) != label.Review })
	w.wantStatus(prd, label.Approved)
	greetPR, _ := greet.openPR(prd)
	docsPR, _ := docs.openPR(prd)
	for _, pr := range []githubtest.Pull{greetPR, docsPR} {
		if n := len(pr.Reviews); n != 1 {
			t.Fatalf("PR #%d reviews = %+v, want one approval", pr.Number, pr.Reviews)
		}
		if note, _, _ := github.ParseNote(pr.Reviews[0].Body); note.Kind != github.KindApproved {
			t.Errorf("PR #%d review = %q, want an approval", pr.Number, pr.Reviews[0].Body)
		}
	}

	docs.gh.MergePull(docsPR.Number)
	time.Sleep(2 * w.cfg.interval)
	w.check("the PRD to wait for the other PR")
	if !w.gh.Issue(prd).Open {
		t.Fatal("the PRD closed with a PR still open")
	}
	greet.gh.MergePull(greetPR.Number)
	w.waitFor("the PRD to close", aRound, func() bool { return !w.gh.Issue(prd).Open })
	w.wantStatus(prd, label.None)
}

// secretListed reports whether sbx keeps s for the sandbox name.
func secretListed(t *testing.T, name string, s sandbox.Secret) bool {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "sbx", "secret", "ls", "--sandbox", name, "--json").Output()
	if err != nil {
		// sbx refuses to list for a sandbox that is gone.
		return false
	}
	return strings.Contains(string(out), s.Placeholder())
}

func fileAt(t *testing.T, origin, ref, path string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", origin, "show", ref+":"+path).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
