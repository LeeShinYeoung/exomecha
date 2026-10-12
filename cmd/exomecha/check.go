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
)

// check finds what would stop exomecha on this machine. It reports every problem it finds, each
// with how to fix it, so one run of init or run shows all of them.
func check(ctx context.Context, c config) error {
	var problems []error
	if _, err := exec.LookPath("sbx"); err != nil {
		problems = append(problems, errors.New("sbx is not installed. Every worker runs claude in Docker Sandboxes"))
	} else {
		problems = append(problems, checkSBX(ctx, c.project)...)
	}
	problems = append(problems, checkClaude(ctx, c)...)
	problems = append(problems, checkGitHub(ctx, c)...)
	return errors.Join(problems...)
}

func checkSBX(ctx context.Context, project string) []error {
	sbx := sandbox.CLI{}
	// sbx ls starts sbx's daemon when it is down, which diagnose would report as a failure.
	if _, err := sbx.List(ctx); err != nil {
		return []error{err}
	}
	var problems []error
	failed, err := sbx.Diagnose(ctx)
	if err != nil {
		problems = append(problems, err)
	}
	for _, f := range failed {
		problems = append(problems, errors.New(strings.TrimSpace(fmt.Sprintf("sbx diagnose, %s: %s. %s", f.Name, f.Message, f.Hint))))
	}
	if has, err := sbx.GlobalGitHubSecret(ctx); err != nil {
		problems = append(problems, fmt.Errorf("check sbx secrets: %w", err))
	} else if has {
		// The sandbox proxy would hand it to every request the AI makes, so the AI could push past exomecha's checks.
		problems = append(problems, errors.New("sbx has a global github secret, which every sandbox could use. Remove it with: sbx secret rm github --force"))
	}
	if in, err := sbx.ClaudeSignedIn(ctx); err != nil {
		problems = append(problems, fmt.Errorf("check sbx secrets: %w", err))
	} else if !in {
		problems = append(problems, fmt.Errorf("sbx has no Claude login, which every worker's claude runs with. Sign in with: exomecha init %s", project))
	}
	return problems
}

func checkClaude(ctx context.Context, c config) []error {
	in, err := agent.Claude{}.SignedIn(ctx)
	if err != nil {
		return []error{fmt.Errorf("read the claude login on this machine: %w", err)}
	}
	if !in {
		return []error{errors.New("claude on this machine is not signed in, and exomecha reads the usage limits with it. Sign in with: claude auth login")}
	}
	var problems []error
	// Once per model: the check finds a model's problems, and claude reports nothing about the effort.
	checked := map[string]bool{}
	for _, t := range []struct {
		key   string
		model agent.Model
	}{
		{"models.light", c.light}, {"models.normal", c.normal}, {"models.heavy", c.heavy}, {"models.resolver", c.resolver},
	} {
		if checked[t.model.Name] {
			continue
		}
		checked[t.model.Name] = true
		if err := (agent.Claude{}).Check(ctx, t.model); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", t.key, err))
		}
	}
	return problems
}

// checkGitHub checks that the token reaches every repository. GitHub does not say what a
// fine-grained token may do there, so the permissions show only when a worker uses them.
func checkGitHub(ctx context.Context, c config) []error {
	targets := c.repos
	if !slices.Contains(targets, c.prd) {
		targets = append([]target{c.prd}, targets...)
	}
	var problems []error
	for _, t := range targets {
		gh, err := client(t, c.token)
		if err == nil {
			err = gh.Reach(ctx)
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("the GitHub token cannot reach %s: %w", t.full(), err))
		}
	}
	return problems
}
