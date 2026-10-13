package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
	"github.com/LeeShinYeoung/exomecha/internal/worker"
)

// item is one line of the machine check. run returns the line to show and the problems it found,
// each with how to fix it.
type item struct {
	group string
	doing string // shown while run runs
	// The group's other items need this one, so they are skipped when it finds a problem.
	gate bool
	// Only init checks it: run does not need it, and run, unlike init, fixes nothing.
	initOnly bool
	run      func(ctx context.Context) (line string, problems []error)
}

const (
	groupSBX    = "Docker Sandboxes (sbx)"
	groupClaude = "Claude Code"
	groupGitHub = "GitHub"
)

// check finds what would stop exomecha on this machine. It reports every problem it finds, each
// with how to fix it, so one start shows all of them.
func check(ctx context.Context, c config) error {
	var problems []error
	skip := ""
	for _, it := range checks(c) {
		if it.initOnly || it.group == skip {
			continue
		}
		_, found := it.run(ctx)
		if len(found) > 0 && it.gate {
			skip = it.group
		}
		problems = append(problems, found...)
	}
	return errors.Join(problems...)
}

func checks(c config) []item {
	sbx := sandbox.CLI{}
	claude := agent.Claude{}
	items := []item{
		{group: groupSBX, doing: "Checking sbx", gate: true, run: func(ctx context.Context) (string, []error) {
			if _, err := exec.LookPath("sbx"); err != nil {
				return "Not installed", []error{errors.New("sbx is not installed. Every worker runs claude in Docker Sandboxes")}
			}
			// sbx ls starts sbx's daemon when it is down, which diagnose would report as a failure.
			if _, err := sbx.List(ctx); err != nil {
				return "Installed, but the daemon does not respond", []error{err}
			}
			return "Installed and running", nil
		}},
		{group: groupSBX, doing: "Running sbx diagnose", run: func(ctx context.Context) (string, []error) {
			failed, err := sbx.Diagnose(ctx)
			var problems []error
			if err != nil {
				problems = append(problems, err)
			}
			for _, f := range failed {
				problems = append(problems, errors.New(strings.TrimSpace(fmt.Sprintf("sbx diagnose, %s: %s. %s", f.Name, f.Message, f.Hint))))
			}
			if len(problems) > 0 {
				return "sbx diagnose found problems", problems
			}
			return "sbx diagnose found no problems", nil
		}},
		{group: groupSBX, doing: "Checking for a GitHub secret shared with every sandbox", run: func(ctx context.Context) (string, []error) {
			has, err := sbx.HasGlobalGitHubSecret(ctx)
			switch {
			case err != nil:
				return "Could not read the sbx secrets", []error{fmt.Errorf("check sbx secrets: %w", err)}
			case has:
				// The sandbox proxy would hand it to every request the AI makes, so the AI could push past exomecha's checks.
				return "A GitHub secret is shared with every sandbox", []error{errors.New("sbx has a global github secret, which every sandbox could use. Remove it with: sbx secret rm github --force")}
			}
			return "No GitHub secret shared with every sandbox", nil
		}},
		{group: groupSBX, doing: "Checking the sandboxes' Claude login", run: func(ctx context.Context) (string, []error) {
			in, err := sbx.ClaudeSignedIn(ctx)
			switch {
			case err != nil:
				return "Could not read the sbx secrets", []error{fmt.Errorf("check sbx secrets: %w", err)}
			case !in:
				return "Sandboxes are not signed in to Claude", []error{fmt.Errorf("sbx has no Claude login, which every worker's claude runs with. Sign in with: exomecha init %s", c.project)}
			}
			return "Sandboxes are signed in to Claude", nil
		}},
		{group: groupClaude, doing: "Checking the Claude login on this machine", gate: true, run: func(ctx context.Context) (string, []error) {
			in, err := claude.SignedIn(ctx)
			switch {
			case err != nil:
				return "Could not read the login", []error{fmt.Errorf("read the claude login on this machine: %w", err)}
			case !in:
				return "Not signed in on this machine", []error{errors.New("claude on this machine is not signed in, and exomecha reads the usage limits with it. Sign in with: claude auth login")}
			}
			return "Signed in on this machine (used to read usage limits)", nil
		}},
	}
	items = append(items, modelItems(c)...)
	items = append(items, item{group: groupClaude, doing: "Checking the exomecha plugin (installs it if missing)", initOnly: true, run: func(ctx context.Context) (string, []error) {
		in, err := claude.PluginInstalled(ctx)
		if err == nil && in {
			return "exomecha plugin installed", nil
		}
		if err == nil {
			err = claude.InstallPlugin(ctx)
		}
		if err != nil {
			return "exomecha plugin not installed", []error{err}
		}
		return "exomecha plugin installed just now", nil
	}})

	// GitHub does not say what a fine-grained token may do in a repository, so the permissions
	// show only when a worker uses them.
	targets := c.repos
	if !slices.Contains(targets, c.prd) {
		targets = append([]target{c.prd}, targets...)
	}
	for _, t := range targets {
		items = append(items, item{group: groupGitHub, doing: "Checking the token's access to " + t.full(), run: func(ctx context.Context) (string, []error) {
			gh, err := client(t, c.token)
			if err == nil {
				err = gh.Reach(ctx)
			}
			if err != nil {
				return "Token cannot access " + t.full(), []error{fmt.Errorf("the GitHub token cannot reach %s: %w", t.full(), err)}
			}
			return "Token can access " + t.full(), nil
		}})
	}
	return items
}

// modelItems checks each model once, with the first key that uses it: the check finds a model's
// problems, and claude reports nothing about the effort.
func modelItems(c config) []item {
	type use struct {
		model agent.Model
		keys  []string
	}
	var uses []*use
	keys := 0
	for _, t := range []struct {
		tier   string
		models worker.Models
	}{
		{"light", c.light}, {"normal", c.normal}, {"heavy", c.heavy},
	} {
		for _, w := range []struct {
			name  string
			model agent.Model
		}{
			{"builder", t.models.Builder}, {"reviewer", t.models.Reviewer}, {"resolver", t.models.Resolver},
		} {
			i := slices.IndexFunc(uses, func(u *use) bool { return u.model.Name == w.model.Name })
			if i < 0 {
				uses = append(uses, &use{model: w.model})
				i = len(uses) - 1
			}
			uses[i].keys = append(uses[i].keys, t.tier+"."+w.name)
			keys++
		}
	}
	var items []item
	for _, u := range uses {
		users := "(used by every worker)"
		if len(u.keys) < keys {
			users = "(used by " + joinAnd(u.keys) + ")"
		}
		name := u.model.Name
		items = append(items, item{group: groupClaude, doing: "Checking " + name, run: func(ctx context.Context) (string, []error) {
			if err := (agent.Claude{}).Check(ctx, u.model); err != nil {
				return name + " is not available " + users, []error{fmt.Errorf("models.%s: %w", u.keys[0], err)}
			}
			return name + " is available " + users, nil
		}})
	}
	return items
}

func joinAnd(words []string) string {
	if len(words) == 1 {
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}
