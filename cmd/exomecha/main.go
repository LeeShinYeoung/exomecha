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
	"github.com/LeeShinYeoung/exomecha/internal/logging"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
	"github.com/LeeShinYeoung/exomecha/internal/template"
	"github.com/LeeShinYeoung/exomecha/internal/worker"
)

func main() {
	// Until run opens the log file, lines go to the terminal alone, in the same form.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("exomecha could not start", logging.Err, err)
		os.Exit(1)
	}
}

// target is a repository exomecha works with.
type target struct {
	owner, name string
	// Empty: GitHub's API, github.com. Only the e2e tests set them.
	apiURL, gitURL string
}

func (t target) full() string { return t.owner + "/" + t.name }

// config is what one exomecha runs with. main reads it from the environment. run sets the fields
// after the models to the operator's own, and the e2e tests set them to theirs.
type config struct {
	prd                     target   // whose issues are the PRDs
	repos                   []target // the repositories PRDs may change
	base, token             string
	fiveHour, weekly        float64
	projectLimit, readLimit int
	reviewLimit             int
	light, normal, heavy    agent.Model
	resolver                agent.Model

	root              string // what exomecha keeps of the PRD repository
	claudeDir         string // empty: no default claude settings
	interval, ciGrace time.Duration
	usage             func(context.Context) (agent.Usage, error) // nil: claude's own reading
	started           func()                                     // called just before the workers start
}

func defaults() config {
	return config{
		fiveHour:     0.9,
		weekly:       0.8,
		projectLimit: 2,
		readLimit:    2,
		reviewLimit:  5,
		light:        agent.Model{Name: "opus", Effort: "low"},
		normal:       agent.Model{Name: "opus", Effort: "high"},
		heavy:        agent.Model{Name: "opus", Effort: "xhigh"},
		resolver:     agent.Model{Name: "opus", Effort: "medium"},
	}
}

func configFromEnv() (config, error) {
	c := defaults()
	repos := os.Getenv("EXOMECHA_REPOS")
	c.base = os.Getenv("EXOMECHA_BASE_BRANCH")
	c.token = os.Getenv("EXOMECHA_GITHUB_TOKEN")
	if repos == "" || c.base == "" || c.token == "" {
		return config{}, errors.New("EXOMECHA_REPOS, EXOMECHA_BASE_BRANCH and EXOMECHA_GITHUB_TOKEN must all be set")
	}
	for r := range strings.SplitSeq(repos, ",") {
		t, err := parseTarget("EXOMECHA_REPOS", strings.TrimSpace(r))
		if err != nil {
			return config{}, err
		}
		for _, o := range c.repos {
			if strings.EqualFold(o.full(), t.full()) {
				return config{}, fmt.Errorf("EXOMECHA_REPOS lists %s twice", t.full())
			}
			// A sandbox with several repositories gives each a folder named after it.
			if strings.EqualFold(o.name, t.name) {
				return config{}, fmt.Errorf("EXOMECHA_REPOS lists two repositories named %s: %s and %s", t.name, o.full(), t.full())
			}
		}
		c.repos = append(c.repos, t)
	}
	switch prd := os.Getenv("EXOMECHA_PRD_REPO"); {
	case prd != "":
		t, err := parseTarget("EXOMECHA_PRD_REPO", prd)
		if err != nil {
			return config{}, err
		}
		c.prd = t
	case len(c.repos) == 1:
		c.prd = c.repos[0]
	default:
		return config{}, errors.New("EXOMECHA_PRD_REPO must be set when EXOMECHA_REPOS lists several repositories")
	}
	for _, t := range c.repos {
		// The same repository under the case GitHub has it, so the PRD repository's clone is shared.
		if strings.EqualFold(t.full(), c.prd.full()) {
			c.prd = t
		}
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
	if c.reviewLimit, err = limit("EXOMECHA_REVIEW_LIMIT", c.reviewLimit); err != nil {
		return config{}, err
	}
	if c.light, err = envModel("EXOMECHA_TIER_LIGHT", c.light); err != nil {
		return config{}, err
	}
	if c.normal, err = envModel("EXOMECHA_TIER_NORMAL", c.normal); err != nil {
		return config{}, err
	}
	if c.heavy, err = envModel("EXOMECHA_TIER_HEAVY", c.heavy); err != nil {
		return config{}, err
	}
	if c.resolver, err = envModel("EXOMECHA_RESOLVER_MODEL", c.resolver); err != nil {
		return config{}, err
	}
	return c, nil
}

func parseTarget(env, repo string) (target, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return target{}, fmt.Errorf("%s must hold owner/name: %q", env, repo)
	}
	return target{owner: owner, name: name}, nil
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

	// Without this a write to stderr after whatever reads it has gone, such as tee, ends exomecha.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	c.root = filepath.Join(home, ".exomecha", c.prd.owner, c.prd.name)
	// Only here, so the e2e runs, which set their own root, leave the operator's defaults alone.
	c.claudeDir = filepath.Join(home, ".exomecha", "claude")
	if err := os.MkdirAll(c.root, 0o755); err != nil {
		return err
	}
	held, err := lock(c.root)
	if err != nil {
		return err
	}
	defer held.Close()
	if err := logging.Open(filepath.Join(c.root, "logs"), os.Stderr); err != nil {
		return err
	}

	// Closing the terminal sends SIGHUP. Treat it like Ctrl-C, so a sandbox start in progress is
	// undone instead of cut off.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return serve(ctx, c)
}

// lock keeps a second exomecha off the repository: both would take the same PRDs and sandboxes and
// write the same log. The OS lets go of the lock when the process ends, however it ends.
func lock(root string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another exomecha is already running for this repository")
		}
		return nil, fmt.Errorf("lock %s: %w", root, err)
	}
	return f, nil
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
	}{
		{"EXOMECHA_TIER_LIGHT", c.light}, {"EXOMECHA_TIER_NORMAL", c.normal}, {"EXOMECHA_TIER_HEAVY", c.heavy},
		{"EXOMECHA_RESOLVER_MODEL", c.resolver},
	} {
		if checked[t.model.Name] {
			continue
		}
		checked[t.model.Name] = true
		if err := (agent.Claude{}).Check(ctx, t.model); err != nil {
			return fmt.Errorf("%s: %w", t.env, err)
		}
	}

	gh, err := client(c.prd, c.token)
	if err != nil {
		return err
	}
	author, email, err := gh.Identity(ctx)
	if err != nil {
		return fmt.Errorf("read the token's GitHub user: %w", err)
	}
	if c.claudeDir != "" {
		if err := worker.Seed(c.claudeDir); err != nil {
			return fmt.Errorf("write the default claude settings: %w", err)
		}
	}
	var repos []*worker.Repo
	var prep *git.Git
	for _, t := range c.repos {
		repoGH := gh
		if t != c.prd {
			if repoGH, err = client(t, c.token); err != nil {
				return err
			}
		}
		g := clone(c, t)
		if t == c.prd {
			prep = g
		}
		repos = append(repos, &worker.Repo{Name: t.full(), GitHub: repoGH, Git: g})
	}
	if prep == nil {
		prep = clone(c, c.prd)
	}
	deps := worker.Deps{
		GitHub:        gh,
		PRDRepo:       c.prd.full(),
		Repos:         repos,
		Prep:          prep,
		Sandboxes:     sbx,
		Environment:   template.New(prep, sbx, c.prd.owner, c.prd.name),
		SandboxDir:    filepath.Join(c.root, "sandboxes"),
		OutputDir:     filepath.Join(c.root, "outputs"),
		SandboxPrefix: sandbox.Prefix(c.prd.owner, c.prd.name),
		ClaudeDir:     c.claudeDir,
		ProjectLimit:  c.projectLimit,
		ReadLimit:     c.readLimit,
		ReviewLimit:   c.reviewLimit,
		GitName:       author,
		GitEmail:      email,
		Models:        map[label.Tier]agent.Model{label.Light: c.light, label.Normal: c.normal, label.Heavy: c.heavy},
		ResolverModel: c.resolver,
		Interval:      c.interval,
		CIGrace:       c.ciGrace,
	}

	created, err := gh.EnsureLabels(ctx)
	if err != nil {
		return fmt.Errorf("ensure labels: %w", err)
	}

	var names []string
	for _, t := range c.repos {
		names = append(names, t.full())
	}
	slog.Info("exomecha started", logging.Repo, c.prd.full(), logging.Repos, strings.Join(names, ","), logging.Base, c.base, logging.FiveHourLimit, c.fiveHour,
		logging.WeeklyLimit, c.weekly, logging.SandboxLimit, c.projectLimit, logging.ReadSandboxLimit, c.readLimit,
		logging.ReviewLimit, c.reviewLimit, logging.TierLight, c.light, logging.TierNormal, c.normal, logging.TierHeavy, c.heavy,
		logging.ResolverModel, c.resolver, logging.CreatedLabels, created)
	check := agent.Claude{}.Usage
	if c.usage != nil {
		check = c.usage
	}
	gate := &worker.Gate{Check: check, FiveHour: c.fiveHour, Weekly: c.weekly}
	if c.started != nil {
		c.started()
	}
	worker.Run(ctx, deps, gate)
	slog.Info("exomecha stopped", logging.Reason, context.Cause(ctx))
	return nil
}

func client(t target, token string) (*github.Client, error) {
	if t.apiURL != "" {
		return github.NewAt(t.apiURL, t.owner, t.name, token)
	}
	return github.New(t.owner, t.name, token)
}

// clone is exomecha's shared clone of t, under the PRD repository's root.
func clone(c config, t target) *git.Git {
	root := filepath.Join(c.root, "repos", t.owner, t.name)
	if t.gitURL != "" {
		return git.NewAt(root, t.gitURL, c.base, c.token)
	}
	return git.New(root, t.owner, t.name, c.base, c.token)
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

func envModel(name string, fallback agent.Model) (agent.Model, error) {
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
