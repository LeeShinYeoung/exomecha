package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/git"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/label"
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
	// Drop the token from the environment so child processes such as claude don't inherit it.
	if err := os.Unsetenv("COWORKER_GITHUB_TOKEN"); err != nil {
		return err
	}

	// SIGHUP too: closing the terminal must stop a running setup script, which has its own process group.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx = github.WaitForRateLimit(ctx)

	gh, err := github.New(owner, name, token)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	g := git.New(filepath.Join(home, ".coworker", owner, name), owner, name, base, token)

	created, err := gh.EnsureLabels(ctx)
	if err != nil {
		return fmt.Errorf("ensure labels: %w", err)
	}
	for _, l := range created {
		slog.Info("created label", "label", l)
	}

	// Only one coworker runs at a time, so a Task still in working was left by a run that stopped mid-task.
	working, err := gh.ListOpen(ctx, label.Task, string(label.Working))
	if err != nil {
		return fmt.Errorf("list working tasks: %w", err)
	}
	for _, t := range working {
		if err := gh.SetStatus(ctx, t.Number, label.Working, label.Queued); err != nil {
			return fmt.Errorf("requeue task #%d: %w", t.Number, err)
		}
		slog.Info("requeued task left working by a previous run", "task", t.Number)
	}

	slog.Info("coworker started", "repo", repo, "base", base, "five_hour_limit", fiveHour, "weekly_limit", weekly)
	gate := &worker.Gate{Check: agent.Claude{}.Usage, FiveHour: fiveHour, Weekly: weekly}
	worker.Run(ctx, worker.Deps{GitHub: gh, Git: g, Agent: agent.Claude{}}, gate)
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
