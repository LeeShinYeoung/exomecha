package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

const setupScript = "git config core.hooksPath .githooks\nmkdir -p .cache\necho ran > .cache/setup-ran\n" +
	"docker run -d --restart unless-stopped --name e2e-svc busybox sleep 100000\n"

func TestE2ESetupAndCommitChecks(t *testing.T) {
	e2e(t, false)
	files := project(map[string]string{
		".exomecha/setup.sh": setupScript,
		".gitignore":         ".cache/\n",
		".githooks/commit-msg": "#!/bin/sh\nhead -n 1 \"$1\" | grep -q ' \\[e2e\\]$' && exit 0\n" +
			"echo 'commit-msg: the first line must end with \" [e2e]\"' >&2\nexit 1\n",
	})
	files["CLAUDE.md"] += "- The commit-msg hook in .githooks checks every commit message.\n"
	w := newWorld(t, "setup", files, func(c *config) { c.ciGrace = time.Hour })
	w.allow("setup script rejected", "sent to stuck")
	prd := w.filePRD(shoutPRD, string(label.Ready), string(label.Light))
	w.start()

	builder := w.box(prd, "builder")
	w.waitFor("builder", aStart, func() bool { return w.started(builder) })
	if out, err := w.execIn(builder, "cat", ".cache/setup-ran"); out != "ran" {
		t.Errorf("the setup script's file in the sandbox = %q (%v)", out, err)
	}
	if out, err := w.exec(builder, "docker", "inspect", "-f", "{{.State.Running}}", "e2e-svc"); strings.TrimSpace(out) != "true" {
		t.Errorf("the service the setup script started: %q (%v)", out, err)
	}
	if !exists(w.ctl(builder, "setup.log")) {
		t.Error("no setup.log for the run")
	}
	w.waitFor("the PR", aRun, func() bool { _, ok := w.openPR(prd); return ok })
	head := w.head(branchOf(prd))
	if w.file(head, ".cache/setup-ran") != "" {
		t.Error("the PR holds a file the setup script made")
	}
	c := w.commit(head)
	if first, _, _ := strings.Cut(c.message, "\n"); !strings.HasSuffix(first, " [e2e]") || !strings.HasSuffix(c.message, "PRD: #"+itoa(prd)) {
		t.Errorf("builder's commit message = %q, want it past the hook and ending with the PRD", c.message)
	}

	w.push("main", "", "refactor: print with printf [e2e]", mainRefactor)
	w.waitFor("resolver's merge", aStart+aRun, func() bool { return w.head(branchOf(prd)) != head })
	m := w.commit(w.head(branchOf(prd)))
	if first, _, _ := strings.Cut(m.message, "\n"); len(m.parents) != 2 || !strings.HasSuffix(first, " [e2e]") {
		t.Errorf("resolver's commit %q with parents %v, want a merge past the hook", m.message, m.parents)
	}
	w.gh.CloseIssue(prd)

	w.push("main", "", "chore: break the setup script [e2e]", map[string]string{".exomecha/setup.sh": "echo boom >&2\nexit 1\n" + setupScript})
	second := w.filePRD(farewellPRD, string(label.Ready), string(label.Light))
	w.waitFor("the PRD to go stuck on the setup script", aRound+aStart, func() bool { return w.status(second) == label.Stuck })
	if got := w.lastComment(second); !strings.Contains(got, "did not end cleanly (exit status 1)") || strings.Contains(got, "boom") {
		t.Errorf("stuck comment = %q, want the exit status and none of the script's output", got)
	}
	w.push("main", "", "chore: fix the setup script [e2e]", map[string]string{".exomecha/setup.sh": setupScript})
	w.relabel(second, label.Stuck, label.Ready)
	box := w.box(second, "builder")
	w.waitFor("builder once the script is fixed", aRound+aStart, func() bool { return w.started(box) })
	if out, err := w.execIn(box, "cat", ".cache/setup-ran"); out != "ran" {
		t.Errorf("the fixed setup script's file = %q (%v)", out, err)
	}
	w.gh.CloseIssue(second)
}

const marker = "FROM docker/sandbox-templates:claude-code-docker\nUSER root\nRUN echo from-template > /etc/e2e-template\nUSER agent\n"

func TestE2ESandboxEnvironment(t *testing.T) {
	e2e(t, true)
	w := newWorld(t, "sandboxenv", project(map[string]string{
		".exomecha/Dockerfile":    marker,
		".exomecha/allowed-hosts": "example.com\n",
		".exomecha/setup.sh":      "mkdir -p .cache\necho ran > .cache/setup-ran\n",
		".gitignore":              ".cache/\n",
	}), func(c *config) { c.ciGrace = 10 * time.Second; c.projectLimit = 2; c.readLimit = 1 })
	light := string(label.Light)
	r1 := w.filePRD(shoutPRD, string(label.Ready), light)
	r2 := w.filePRD(farewellPRD, string(label.Ready), light)
	r3 := w.filePRD(wavePRD, string(label.Ready), light)
	v1, _ := w.seedReview(shoutPRD, shoutDone)
	v2, _ := w.seedReview(farewellPRD, farewellDone)
	peak := w.watchPeaks(r1, r2, r3)
	w.start()

	// Readers need no template, so the first review starts while the template builds.
	rv1, rv2 := w.box(v1, "reviewer"), w.box(v2, "reviewer")
	reader := w.holdAsTheyStart(10*time.Second+aStart, 1, rv1, rv2)[0]
	readerPRD, otherPRD, other := v1, v2, rv2
	if reader == rv2 {
		readerPRD, otherPRD, other = v2, v1, rv1
	}
	builders := map[string]int{w.box(r1, "builder"): r1, w.box(r2, "builder"): r2, w.box(r3, "builder"): r3}
	held := w.holdAsTheyStart(aBuild+aStart, 2, slices.Collect(maps.Keys(builders))...)
	b1 := held[0]
	var b3 string
	for name := range builders {
		if !slices.Contains(held, name) {
			b3 = name
		}
	}

	images, _ := w.sbx.Templates(context.Background())
	var tags int
	for _, img := range images {
		if img.Repository == "docker.io/library/exomecha-"+w.slug() {
			tags++
		}
	}
	if tags != 1 {
		t.Errorf("%d templates of the project, want one", tags)
	}
	if all, _ := w.sbx.List(context.Background()); hasBox(all, sandbox.BuildName(e2eOwner, w.repo)) {
		t.Error("the template's build sandbox was left")
	}
	if n := logs.count(w, "building template"); n != 1 {
		t.Errorf("the template was built %d times, want once", n)
	}

	const email = "4242+e2e@users.noreply.github.com"
	for _, c := range []struct {
		what, want string
		got        func() (string, error)
	}{
		{"the template's marker", "from-template", func() (string, error) { return w.exec(b1, "cat", "/etc/e2e-template") }},
		{"the commit email", email, func() (string, error) { return w.execIn(b1, "git", "config", "user.email") }},
		{"the remotes", "", func() (string, error) { return w.execIn(b1, "git", "remote", "-v") }},
		{"the listed host", "200", func() (string, error) {
			return w.exec(b1, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "https://example.com")
		}},
	} {
		if got, err := c.got(); strings.TrimSpace(got) != c.want {
			t.Errorf("builder sandbox: %s = %q (%v), want %q", c.what, got, err, c.want)
		}
	}
	for _, c := range []struct{ box, host, want string }{
		{b1, "example.org", "Approval required for example.org:443"},
		{b1, "ssh.github.com", "Blocked by local rule for ssh.github.com:443"},
		{reader, "example.com", "Approval required for example.com:443"},
		{reader, "ssh.github.com", "Blocked by local rule for ssh.github.com:443"},
	} {
		if out, _ := w.exec(c.box, "curl", "-s", "https://"+c.host); !strings.Contains(out, c.want) {
			t.Errorf("curl %s in %s = %q, want %q", c.host, c.box, out, c.want)
		}
	}
	for _, box := range []string{b1, reader} {
		if env, _ := w.exec(box, "env"); strings.Contains(env, w.cfg.token) {
			t.Errorf("the GitHub token is in %s's environment", box)
		}
	}
	if policy, _ := w.sbxCmd("policy", "ls", b1, "--wide"); !strings.Contains(policy, "example.com") {
		t.Errorf("no policy rule lets %s reach example.com:\n%s", b1, policy)
	}
	if policy, _ := w.sbxCmd("policy", "ls", reader, "--wide"); strings.Contains(policy, "example.com") {
		t.Errorf("a policy rule lets the reader sandbox reach example.com:\n%s", policy)
	}
	if _, err := w.exec(reader, "cat", "/etc/e2e-template"); err == nil {
		t.Error("the reader sandbox runs on the project's template")
	}
	if exists(w.ctl(reader, "setup.sh")) {
		t.Error("the reader run got the setup script")
	}
	if out, _ := w.execIn(reader, "git", "status", "--short", "--ignored"); strings.Contains(out, ".cache") {
		t.Errorf("the setup script ran in the reader sandbox: %q", out)
	}
	for _, args := range [][]string{{"git", "config", "user.email"}, {"git", "remote", "-v"}} {
		if out, _ := w.execIn(reader, args...); out != "" {
			t.Errorf("reader sandbox: %v = %q, want nothing", args, out)
		}
	}

	w.holds("one reader and two builders at a time", 3*w.cfg.interval, func() bool {
		return !w.exists(other) && !w.exists(b3)
	})
	w.gh.CloseIssue(readerPRD)
	w.waitFor("the second review once the first is gone", 2*aRound+aStart, func() bool { return w.started(other) })
	w.gh.CloseIssue(otherPRD)
	w.gh.CloseIssue(builders[b1])
	w.waitFor("the third builder once a slot is free", 2*aRound+aStart, func() bool { return w.started(b3) })
	for _, n := range []int{r1, r2, r3} {
		w.gh.CloseIssue(n)
	}

	peak.stop()
	if peak.builders > 2 || peak.working > 2 || peak.readers > 1 || !peak.builderAndReaderAtOnce {
		t.Errorf("at most %d project and %d reader sandboxes and %d working PRDs at once, both kinds together %v; want 2, 1, 2 and true",
			peak.builders, peak.readers, peak.working, peak.builderAndReaderAtOnce)
	}
}

func hasBox(boxes []sandbox.Box, name string) bool {
	for _, b := range boxes {
		if b.Name == name {
			return true
		}
	}
	return false
}

func TestE2ETemplateProblems(t *testing.T) {
	e2e(t, false)
	w := newWorld(t, "templateprob", project(map[string]string{
		".exomecha/Dockerfile": "FROM docker/sandbox-templates:claude-code-docker\nRUN echo exomecha-secret && false\n",
	}), func(c *config) { c.ciGrace = time.Hour })
	w.allow("template build failed", "sandbox environment needs a person", "sent to stuck")
	a := w.filePRD(shoutPRD, string(label.Ready), string(label.Light))
	w.start()

	w.waitFor("the PRD to go stuck on the Dockerfile", aBuild+aRound, func() bool { return w.status(a) == label.Stuck })
	if got := w.lastComment(a); !strings.Contains(got, ".exomecha/Dockerfile") || strings.Contains(got, "exomecha-secret") {
		t.Errorf("stuck comment = %q, want it to name the Dockerfile and hold none of the build output", got)
	}
	if all, _ := w.sbx.List(context.Background()); hasBox(all, sandbox.BuildName(e2eOwner, w.repo)) {
		t.Error("the failed build left its sandbox")
	}
	w.push("main", "", "chore: drop the Dockerfile", map[string]string{".exomecha/Dockerfile": gone})
	w.relabel(a, label.Stuck, label.Ready)
	w.waitFor("builder once the Dockerfile is gone", aRound+aStart, func() bool { return w.started(w.box(a, "builder")) })
	w.gh.CloseIssue(a)

	w.push("main", "", "chore: allow every host", map[string]string{".exomecha/allowed-hosts": "**\n"})
	b := w.filePRD(farewellPRD, string(label.Ready), string(label.Light))
	w.waitFor("the PRD to go stuck on allowed-hosts", aRound+aStart, func() bool { return w.status(b) == label.Stuck })
	if got := w.lastComment(b); !strings.Contains(got, "`**`") || !strings.Contains(got, "allows every host") {
		t.Errorf("stuck comment = %q, want it to quote the line", got)
	}
	w.push("main", "", "chore: drop allowed-hosts", map[string]string{".exomecha/allowed-hosts": gone})
	w.relabel(b, label.Stuck, label.Ready)
	w.waitFor("builder once allowed-hosts is fixed", aRound+aStart, func() bool { return w.started(w.box(b, "builder")) })
	w.gh.CloseIssue(b)
}

func TestE2ERunFailures(t *testing.T) {
	e2e(t, true)
	const stall = 2 * time.Minute
	w := newWorld(t, "runfail", project(), func(c *config) { c.ciGrace = 10 * time.Second; c.stallAfter = stall })
	w.allow("run failed", "sent to stuck")
	a := w.filePRD(shoutPRD, string(label.Ready), string(label.Light))
	b, _ := w.seedReview(farewellPRD, farewellDone)
	since := time.Now()
	w.start()

	builder, reviewer := w.box(a, "builder"), w.box(b, "reviewer")
	w.holdAsTheyStart(aStart, 2, builder, reviewer)

	// The same run name fails more than once here, so only files kept after since count.
	kept := func(what, name string) {
		t.Helper()
		w.waitFor(what+"'s files to be kept in failed/", aRound, func() bool {
			info, err := os.Stat(filepath.Join(w.root, "sandboxes", "failed", name, "input.md"))
			return err == nil && info.ModTime().After(since)
		})
	}
	wantStuck := func(n int, why string, back label.Status) {
		t.Helper()
		w.waitFor("PRD #"+itoa(n)+" to go stuck", 2*aRound, func() bool { return w.status(n) == label.Stuck })
		if got := w.lastComment(n); !strings.Contains(got, why) || !strings.Contains(got, "`"+string(back)+"`") {
			t.Errorf("stuck comment = %q, want %q and to relabel it %s", got, why, back)
		}
	}

	w.sbxCmd("stop", reviewer)
	wantStuck(b, "the sandbox stopped while claude was running", label.Review)
	kept("the stopped review", reviewer)
	first := w.startedAt(reviewer)
	w.relabel(b, label.Stuck, label.Review)
	w.waitFor("a new review", aRound+aStart, func() bool { at := w.startedAt(reviewer); return !at.IsZero() && at.After(first) })
	w.gh.CloseIssue(b)

	w.sbxCmd("stop", builder)
	wantStuck(a, "the sandbox stopped while claude was running", label.Ready)
	kept("the stopped run", builder)

	since = time.Now()
	w.relabel(a, label.Stuck, label.Ready)
	w.waitFor("builder again", aRound+aStart, func() bool { return w.started(builder) })
	w.hold(builder)
	w.sbxCmd("rm", "--force", builder)
	wantStuck(a, "the sandbox was removed while claude was running", label.Ready)

	since = time.Now()
	w.relabel(a, label.Stuck, label.Ready)
	w.waitFor("builder a third time", aRound+aStart, func() bool { return w.started(builder) })
	w.hold(builder)
	w.waitFor("the frozen run to count as stalled", stall+3*w.cfg.interval+aRound, func() bool { return w.status(a) == label.Stuck })
	if got := w.lastComment(a); !strings.Contains(got, "claude's output did not change for 2 minutes") {
		t.Errorf("stuck comment = %q, want the stall", got)
	}
	kept("the stalled run", builder)
}

func TestE2EStartRefusesBadModels(t *testing.T) {
	e2e(t, false)
	for _, c := range []struct{ model, want string }{
		{"haiku:low", "auto permission mode"},
		{"opsu:low", "opsu"},
	} {
		m, err := agent.ParseModel(c.model)
		if err != nil {
			t.Fatal(err)
		}
		cfg := defaults()
		cfg.light = m
		err = serve(t.Context(), cfg)
		if err == nil || !strings.Contains(err.Error(), "EXOMECHA_TIER_LIGHT") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("start with light %s: %v, want a refusal naming EXOMECHA_TIER_LIGHT and %q", c.model, err, c.want)
		}
	}
}
