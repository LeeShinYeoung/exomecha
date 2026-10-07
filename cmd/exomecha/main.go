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
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
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

// config is what one exomecha runs with. main reads it from the environment. Only the e2e tests
// set the fields after the tier models.
type config struct {
	owner, name, base, token string
	fiveHour, weekly         float64
	projectLimit, readLimit  int
	light, normal, heavy     agent.Model

	apiURL, gitURL, root string // empty: GitHub's API, github.com, ~/.exomecha/<owner>/<name>
	interval, ciGrace    time.Duration
	usage                func(context.Context) (agent.Usage, error) // nil: claude's own reading
	started              func()                                     // called just before the workers start
}

func defaults() config {
	return config{
		fiveHour:     0.9,
		weekly:       0.8,
		projectLimit: 2,
		readLimit:    2,
		light:        agent.Model{Name: "opus", Effort: "low"},
		normal:       agent.Model{Name: "opus", Effort: "high"},
		heavy:        agent.Model{Name: "opus", Effort: "xhigh"},
	}
}

func configFromEnv() (config, error) {
	c := defaults()
	repo := os.Getenv("EXOMECHA_REPO")
	c.base = os.Getenv("EXOMECHA_BASE_BRANCH")
	c.token = os.Getenv("EXOMECHA_GITHUB_TOKEN")
	if repo == "" || c.base == "" || c.token == "" {
		return config{}, errors.New("EXOMECHA_REPO, EXOMECHA_BASE_BRANCH and EXOMECHA_GITHUB_TOKEN must all be set")
	}
	var ok bool
	c.owner, c.name, ok = strings.Cut(repo, "/")
	if !ok || c.owner == "" || c.name == "" || strings.Contains(c.name, "/") {
		return config{}, fmt.Errorf("EXOMECHA_REPO must be owner/name: %q", repo)
	}
	var err error
	if c.fiveHour, err = usageLimit("EXOMECHA_FIVE_HOUR_LIMIT", c.fiveHour); err != nil {
		return config{}, err
	}
	if c.weekly, err = usageLimit("EXOMECHA_WEEKLY_LIMIT", c.weekly); err != nil {
		return config{}, err
	}
	if c.projectLimit, err = limit("EXOMECHA_SANDBOX_LIMIT", c.projectLimit); err != nil {
		return config{}, err
	}
	if c.readLimit, err = limit("EXOMECHA_READ_SANDBOX_LIMIT", c.readLimit); err != nil {
		return config{}, err
	}
	if c.light, err = tierModel("EXOMECHA_TIER_LIGHT", c.light); err != nil {
		return config{}, err
	}
	if c.normal, err = tierModel("EXOMECHA_TIER_NORMAL", c.normal); err != nil {
		return config{}, err
	}
	if c.heavy, err = tierModel("EXOMECHA_TIER_HEAVY", c.heavy); err != nil {
		return config{}, err
	}
	return c, nil
}

func run() error {
	c, err := configFromEnv()
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
	return serve(ctx, c)
}

// serve runs exomecha until ctx is done.
func serve(ctx context.Context, c config) error {
	ctx = github.WaitForRateLimit(ctx)

	if _, err := exec.LookPath("sbx"); err != nil {
		return errors.New("sbx is not installed. Every worker runs claude in Docker Sandboxes")
	}
	sbx := sandbox.CLI{}
	if has, err := sbx.GlobalGitHubSecret(ctx); err != nil {
		return fmt.Errorf("check sbx secrets: %w", err)
	} else if has {
		// The sandbox proxy would hand it to every request the AI makes, so the AI could push past exomecha's checks.
		return errors.New("sbx has a global github secret, which every sandbox could use. Remove it with: sbx secret rm github --force")
	}
	// Once per model: the check finds a model's problems, and claude reports nothing about the effort.
	checked := map[string]bool{}
	for _, t := range []struct {
		env   string
		model agent.Model
	}{{"EXOMECHA_TIER_LIGHT", c.light}, {"EXOMECHA_TIER_NORMAL", c.normal}, {"EXOMECHA_TIER_HEAVY", c.heavy}} {
		if checked[t.model.Name] {
			continue
		}
		checked[t.model.Name] = true
		if err := (agent.Claude{}).Check(ctx, t.model); err != nil {
			return fmt.Errorf("%s: %w", t.env, err)
		}
	}

	var gh *github.Client
	var err error
	if c.apiURL != "" {
		gh, err = github.NewAt(c.apiURL, c.owner, c.name, c.token)
	} else {
		gh, err = github.New(c.owner, c.name, c.token)
	}
	if err != nil {
		return err
	}
	author, email, err := gh.Identity(ctx)
	if err != nil {
		return fmt.Errorf("read the token's GitHub user: %w", err)
	}
	root := c.root
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		root = filepath.Join(home, ".exomecha", c.owner, c.name)
	}
	g := git.New(root, c.owner, c.name, c.base, c.token)
	if c.gitURL != "" {
		g = git.NewAt(root, c.gitURL, c.base, c.token)
	}
	deps := worker.Deps{
		GitHub:        gh,
		Git:           g,
		Sandboxes:     sbx,
		Environment:   template.New(g, sbx, c.owner, c.name),
		SandboxDir:    filepath.Join(root, "sandboxes"),
		SandboxPrefix: sandbox.Prefix(c.owner, c.name),
		ProjectLimit:  c.projectLimit,
		ReadLimit:     c.readLimit,
		GitName:       author,
		GitEmail:      email,
		Models:        map[label.Tier]agent.Model{label.Light: c.light, label.Normal: c.normal, label.Heavy: c.heavy},
		Interval:      c.interval,
		CIGrace:       c.ciGrace,
	}

	created, err := gh.EnsureLabels(ctx)
	if err != nil {
		return fmt.Errorf("ensure labels: %w", err)
	}
	for _, l := range created {
		slog.Info("created label", "label", l)
	}

	slog.Info("exomecha started", "repo", c.owner+"/"+c.name, "base", c.base, "five_hour_limit", c.fiveHour,
		"weekly_limit", c.weekly, "sandbox_limit", c.projectLimit, "read_sandbox_limit", c.readLimit,
		"tier_light", c.light, "tier_normal", c.normal, "tier_heavy", c.heavy)
	check := agent.Claude{}.Usage
	if c.usage != nil {
		check = c.usage
	}
	gate := &worker.Gate{Check: check, FiveHour: c.fiveHour, Weekly: c.weekly}
	if c.started != nil {
		c.started()
	}
	worker.Run(ctx, deps, gate)
	slog.Info("exomecha stopped")
	return nil
}

// usageLimit reads a percentage of a Claude plan limit, at which exomecha stops starting new work.
func usageLimit(name string, fallback float64) (float64, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 100 {
		return 0, fmt.Errorf("%s must be a whole number from 1 to 100: %q", name, v)
	}
	return float64(n) / 100, nil
}

func tierModel(name string, fallback agent.Model) (agent.Model, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	m, err := agent.ParseModel(v)
	if err != nil {
		return agent.Model{}, fmt.Errorf("%s must be model:effort, such as sonnet:low: %w: %q", name, err, v)
	}
	return m, nil
}

func limit(name string, fallback int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a whole number of at least 1: %q", name, v)
	}
	return n, nil
}
