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

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
	"github.com/LeeShinYeoung/exomecha/internal/template"
	"github.com/LeeShinYeoung/exomecha/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("exomecha stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	repo := os.Getenv("EXOMECHA_REPO")
	base := os.Getenv("EXOMECHA_BASE_BRANCH")
	token := os.Getenv("EXOMECHA_GITHUB_TOKEN")
	if repo == "" || base == "" || token == "" {
		return errors.New("EXOMECHA_REPO, EXOMECHA_BASE_BRANCH and EXOMECHA_GITHUB_TOKEN must all be set")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("EXOMECHA_REPO must be owner/name: %q", repo)
	}
	fiveHour, err := usageLimit("EXOMECHA_FIVE_HOUR_LIMIT", 90)
	if err != nil {
		return err
	}
	weekly, err := usageLimit("EXOMECHA_WEEKLY_LIMIT", 80)
	if err != nil {
		return err
	}
	sandboxes, err := sandboxLimit()
	if err != nil {
		return err
	}
	// Drop the token from the environment so child processes such as claude don't inherit it.
	if err := os.Unsetenv("EXOMECHA_GITHUB_TOKEN"); err != nil {
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
		// The sandbox proxy would hand it to every request the AI makes, so the AI could push past exomecha's checks.
		return errors.New("sbx has a global github secret, which every sandbox could use. Remove it with: sbx secret rm github --force")
	}

	gh, err := github.New(owner, name, token)
	if err != nil {
		return err
	}
	author, email, err := gh.Identity(ctx)
	if err != nil {
		return fmt.Errorf("read the token's GitHub user: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root := filepath.Join(home, ".exomecha", owner, name)
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
		SandboxPrefix: "exomecha-task-" + template.Slug(owner, name, 32) + "-",
		SandboxLimit:  sandboxes,
		GitName:       author,
		GitEmail:      email,
	}

	created, err := gh.EnsureLabels(ctx)
	if err != nil {
		return fmt.Errorf("ensure labels: %w", err)
	}
	for _, l := range created {
		slog.Info("created label", "label", l)
	}

	slog.Info("exomecha started", "repo", repo, "base", base, "five_hour_limit", fiveHour, "weekly_limit", weekly, "sandbox_limit", sandboxes)
	gate := &worker.Gate{Check: agent.Claude{}.Usage, FiveHour: fiveHour, Weekly: weekly}
	worker.Run(ctx, deps, gate)
	slog.Info("exomecha stopped")
	return nil
}

// usageLimit reads a percentage of a Claude plan limit, at which exomecha stops starting new work.
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
	v := os.Getenv("EXOMECHA_SANDBOX_LIMIT")
	if v == "" {
		return 2, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("EXOMECHA_SANDBOX_LIMIT must be a whole number of at least 1: %q", v)
	}
	return n, nil
}
