package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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

const usage = `Usage:
  exomecha init <project>   ask for the project's settings, write ~/.exomecha/<project>.yaml and check this machine
  exomecha run <project>    run the project in this terminal
  exomecha list             list the projects and whether each is running
`

func main() {
	// Until run opens the log file, lines go to the terminal alone, in the same form.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "exomecha:", err)
		os.Exit(1)
	}
	args := os.Args[1:]
	switch {
	case len(args) == 2 && args[0] == "init":
		// Ctrl-C during the sign-in still removes its sandbox.
		ctx, stop := signalContext()
		err = initProject(ctx, home, args[1])
		stop()
	case len(args) == 2 && args[0] == "run":
		if err := run(home, args[1]); err != nil {
			errs := []error{err}
			// One line per problem the start check found, since a log line keeps no line breaks.
			if joined, ok := err.(interface{ Unwrap() []error }); ok {
				errs = joined.Unwrap()
			}
			for _, err := range errs {
				slog.Error("exomecha could not start", logging.Err, err)
			}
			os.Exit(1)
		}
	case len(args) == 1 && args[0] == "list":
		err = list(home, os.Stdout)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "exomecha:", err)
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

// config is what one exomecha runs with. run reads it from the project's file and sets the fields
// after the models to the operator's own, and the e2e tests set them to theirs.
type config struct {
	prd                     target   // whose issues are the PRDs
	repos                   []target // the repositories PRDs may change
	base, token             string
	secrets                 []sandbox.Secret
	fiveHour, weekly        float64
	projectLimit, readLimit int
	reviewLimit             int
	light, normal, heavy    agent.Model
	resolver                agent.Model

	project           string // the name of the project's file, which check's fixes name
	root              string // what exomecha keeps of the project
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

func run(home, project string) error {
	path, err := settingsPath(home, project)
	if err != nil {
		return err
	}
	c, err := loadConfig(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s has no settings. Make them with: exomecha init %s", project, project)
	}
	if err != nil {
		return err
	}
	c.project = project
	if err := checkPRDRepo(home, path, c.prd); err != nil {
		return err
	}

	// Without this a write to stderr after whatever reads it has gone, such as tee, ends exomecha.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)

	c.root = projectRoot(home, project)
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
	if err := logging.SetDefault(filepath.Join(c.root, "logs"), os.Stderr); err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()
	return serve(ctx, c)
}

// Closing the terminal sends SIGHUP. Treat it like Ctrl-C, so a sandbox start in progress is undone
// instead of cut off. After the first signal the default comes back, so a second Ctrl-C ends
// exomecha at once.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

func projectRoot(home, project string) string {
	return filepath.Join(home, ".exomecha", project)
}

func setupDir(root string) string { return filepath.Join(root, "setup") }

// lock keeps a second exomecha off the project: both would take the same PRDs and sandboxes and
// write the same log. The OS lets go of the lock when the process ends, however it ends.
func lock(root string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	// list holds the lock for a moment to see whether an exomecha runs, so wait about a second for it.
	for try := 0; try < 10 && errors.Is(err, syscall.EWOULDBLOCK); try++ {
		time.Sleep(100 * time.Millisecond)
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another exomecha is already running for this project")
		}
		return nil, fmt.Errorf("lock %s: %w", root, err)
	}
	return f, nil
}

func serve(ctx context.Context, c config) error {
	ctx = github.WaitForRateLimit(ctx)

	if err := check(ctx, c); err != nil {
		return err
	}
	sbx := sandbox.CLI{}

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
	for _, t := range c.repos {
		repoGH := gh
		if t != c.prd {
			if repoGH, err = client(t, c.token); err != nil {
				return err
			}
		}
		repos = append(repos, &worker.Repo{Name: t.full(), GitHub: repoGH, Git: newGit(c, t)})
	}
	deps := worker.Deps{
		GitHub:        gh,
		PRDRepo:       c.prd.full(),
		Repos:         repos,
		SetupDir:      setupDir(c.root),
		Secrets:       c.secrets,
		Sandboxes:     sbx,
		Environment:   template.New(setupDir(c.root), sbx, c.prd.owner, c.prd.name),
		SandboxDir:    filepath.Join(c.root, "sandboxes"),
		OutputDir:     filepath.Join(c.root, "outputs"),
		SandboxPrefix: sandbox.RunPrefix(c.prd.owner, c.prd.name),
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
	readUsage := agent.Claude{}.Usage
	if c.usage != nil {
		readUsage = c.usage
	}
	gate := &worker.Gate{Check: readUsage, FiveHour: c.fiveHour, Weekly: c.weekly}
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

// newGit is the Git of exomecha's shared clone of t, in the project's folder.
func newGit(c config, t target) *git.Git {
	root := filepath.Join(c.root, "repos", t.owner, t.name)
	if t.gitURL != "" {
		return git.NewAt(root, t.gitURL, c.base, c.token)
	}
	return git.New(root, t.owner, t.name, c.base, c.token)
}
