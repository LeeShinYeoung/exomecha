package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/git"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/sandbox"
	"github.com/LeeShinYeoung/coworker/internal/template"
	"github.com/LeeShinYeoung/coworker/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("coworker stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	repo := os.Getenv("COWORKER_REPO")
	base := os.Getenv("COWORKER_BASE_BRANCH")
	token := os.Getenv("COWORKER_GITHUB_TOKEN")
	if repo == "" || base == "" || token == "" {
		return errors.New("COWORKER_REPO, COWORKER_BASE_BRANCH and COWORKER_GITHUB_TOKEN must all be set")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("COWORKER_REPO must be owner/name: %q", repo)
	}
	fiveHour, err := usageLimit("COWORKER_FIVE_HOUR_LIMIT", 90)
	if err != nil {
		return err
	}
	weekly, err := usageLimit("COWORKER_WEEKLY_LIMIT", 80)
	if err != nil {
		return err
	}
	sandboxes, err := sandboxLimit()
	if err != nil {
		return err
	}
	// Drop the token from the environment so child processes such as claude don't inherit it.
	if err := os.Unsetenv("COWORKER_GITHUB_TOKEN"); err != nil {
		return err
	}

	// Closing the terminal sends SIGHUP. Treat it like Ctrl-C, so a sandbox start in progress is
	// undone instead of cut off.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx = github.WaitForRateLimit(ctx)

	if _, err := exec.LookPath("sbx"); err != nil {
		return errors.New("sbx is not installed. builder and resolver run claude in Docker Sandboxes")
	}
	sbx := sandbox.CLI{}
	if has, err := sbx.GlobalGitHubSecret(ctx); err != nil {
		return fmt.Errorf("check sbx secrets: %w", err)
	} else if has {
		// The sandbox proxy would hand it to every request the AI makes, so the AI could push past coworker's checks.
		return errors.New("sbx has a global github secret, which every sandbox could use. Remove it with: sbx secret rm github")
	}

	gh, err := github.New(owner, name, token)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root := filepath.Join(home, ".coworker", owner, name)
	g := git.New(root, owner, name, base, token)
	deps := worker.Deps{
		GitHub:      gh,
		Git:         g,
		Agent:       agent.Claude{},
		Sandboxes:   sbx,
		Environment: template.New(g, owner, name),
		SandboxDir:  filepath.Join(root, "sandboxes"),
		// 32 leaves room for "9999999-resolver" in sbx's 63-character names, and is the max the
		// template package uses, so the build sandbox and templates carry the same slug.
		SandboxPrefix: "coworker-task-" + template.Slug(owner, name, 32) + "-",
		SandboxLimit:  sandboxes,
	}

	created, err := gh.EnsureLabels(ctx)
	if err != nil {
		return fmt.Errorf("ensure labels: %w", err)
	}
	for _, l := range created {
		slog.Info("created label", "label", l)
	}

	slog.Info("coworker started", "repo", repo, "base", base, "five_hour_limit", fiveHour, "weekly_limit", weekly, "sandbox_limit", sandboxes)
	gate := &worker.Gate{Check: agent.Claude{}.Usage, FiveHour: fiveHour, Weekly: weekly}
	worker.Run(ctx, deps, gate)
	slog.Info("coworker stopped")
	return nil
}

// usageLimit reads a percentage of a Claude plan limit, at which coworker stops starting new work.
func usageLimit(name string, fallback int) (float64, error) {
	v := os.Getenv(name)
	if v == "" {
		return float64(fallback) / 100, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 100 {
		return 0, fmt.Errorf("%s must be a whole number from 1 to 100: %q", name, v)
	}
	return float64(n) / 100, nil
}

// sandboxLimit reads how many task sandboxes may exist at once.
func sandboxLimit() (int, error) {
	v := os.Getenv("COWORKER_SANDBOX_LIMIT")
	if v == "" {
		return 2, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("COWORKER_SANDBOX_LIMIT must be a whole number of at least 1: %q", v)
	}
	return n, nil
}
