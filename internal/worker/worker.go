package worker

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/git"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

type Worker interface {
	Name() string
	// Pick returns issue numbers in ascending order.
	Pick(ctx context.Context) ([]int, error)
	// Handle sends a Task to stuck and returns nil when the Task itself is the problem. Any other error
	// is an environment error: leave the Task where the next check can retry it, and return the error.
	// The loop puts off the next try after an error, longer each time in a row, so a Task that only
	// has to wait returns nil.
	Handle(ctx context.Context, number int) error
}

type Sweeper interface {
	Sweep(ctx context.Context) error
}

type Deps struct {
	GitHub *github.Client
	Git    *git.Git

	// Every worker runs claude in sandboxes.
	Sandboxes   Sandboxes
	Environment Environment
	// SandboxDir holds one directory per run, shared with the run's sandbox.
	SandboxDir string
	// SandboxPrefix starts the name of every run's sandbox of this repository and nothing else.
	SandboxPrefix string
	// How many sandboxes of project runs, and of reader runs, may exist at once.
	ProjectLimit int
	ReadLimit    int
	// The commit identity for project runs, the token owner's. A sandbox has none of its own.
	GitName  string
	GitEmail string
}

type Sandboxes interface {
	List(ctx context.Context) ([]sandbox.Box, error)
	// Create makes a sandbox that keeps running with no exec session open. An empty template
	// is sbx's own claude template. Hosts in deny are blocked from the start.
	Create(ctx context.Context, name, template, workspace string, deny []string) error
	Allow(ctx context.Context, name string, hosts []string) error
	Exec(ctx context.Context, name, dir string, env []string, args ...string) ([]byte, error)
	// Start returns at once and leaves args running after the exec session ends.
	Start(ctx context.Context, name, dir string, args ...string) error
	// Remove of a sandbox that does not exist is not an error.
	Remove(ctx context.Context, name string) error
	GitURL(name, dir string) string
}

// Environment is the sandbox setup the target project asks for on its base branch.
// An error whose chain holds a value with a Why() string method is one only a person can fix,
// and Why() is what to tell them.
type Environment interface {
	// An empty ref is sbx's own template, for a project with no Dockerfile for exomecha.
	Template(ctx context.Context) (ref string, err error)
	AllowedHosts(ctx context.Context) ([]string, error)
}

const (
	interval = 60 * time.Second
	// The wait after an error doubles with each error in a row, up to maxBackoff.
	minBackoff = time.Minute
	maxBackoff = 30 * time.Minute
)

func Run(ctx context.Context, d Deps, gate *Gate) {
	l := &locks{held: map[int]bool{}}
	// runs checks the gate only when a run starts, so runs already in sandboxes are still watched
	// and finished, and steps without claude go on, while usage is over the limit.
	runs := newRuns(d, gate)
	workers := []Worker{
		&splitter{d, runs},
		&sequencer{d, runs},
		&builder{d, runs},
		&reviewer{d, runs},
		&resolver{d, runs},
	}
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() { loop(ctx, w, l) })
	}
	wg.Wait()
}

func loop(ctx context.Context, w Worker, l *locks) {
	var wg sync.WaitGroup
	defer wg.Wait()
	d := &delays{now: time.Now, items: map[int]delay{}}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		round(ctx, w, l, d, &wg)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func round(ctx context.Context, w Worker, l *locks, d *delays, wg *sync.WaitGroup) {
	// A wait counts from here, not from when Handle returned, so a wait of a minute ends by the
	// next round.
	now := d.now()
	log := slog.With("worker", w.Name())
	if s, ok := w.(Sweeper); ok {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			log.Error("sweep failed", "err", err)
		}
	}
	numbers, err := w.Pick(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("pick failed", "err", err)
		}
		return
	}
	d.keep(numbers)
	for _, n := range numbers {
		if !d.due(n, now) || !l.lock(n) {
			continue
		}
		wg.Go(func() {
			defer l.unlock(n)
			err := w.Handle(ctx, n)
			if err == nil {
				d.forget(n)
				return
			}
			// An error from shutdown says nothing about the issue.
			if ctx.Err() == nil {
				d.strike(n, now)
			}
			log.Error("handle failed", "issue", n, "err", err)
		})
	}
}

// delays holds one worker's issues whose last Handle failed, so the loop tries them again later.
// It lives in memory only, and a restart tries every issue at once.
type delays struct {
	now   func() time.Time
	mu    sync.Mutex
	items map[int]delay
}

type delay struct {
	strikes int // errors in a row
	next    time.Time
}

func (d *delays) due(n int, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	it, ok := d.items[n]
	// A round can start a moment less than interval after the one before, and must still find a
	// wait of interval over.
	return !ok || !now.Add(time.Second).Before(it.next)
}

func (d *delays) strike(n int, from time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	it := d.items[n]
	it.strikes++
	it.next = from.Add(min(maxBackoff, minBackoff<<min(it.strikes-1, 10)))
	d.items[n] = it
}

func (d *delays) forget(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.items, n)
}

// keep forgets the issues Pick no longer returns, so one that comes back later starts with no wait.
func (d *delays) keep(picked []int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for n := range d.items {
		if !slices.Contains(picked, n) {
			delete(d.items, n)
		}
	}
}

// locks stops two workers (e.g. reviewer and resolver) from handling the same issue at once.
type locks struct {
	mu   sync.Mutex
	held map[int]bool
}

func (l *locks) lock(n int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[n] {
		return false
	}
	l.held[n] = true
	return true
}

func (l *locks) unlock(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, n)
}

//go:embed prompts/*.md
var prompts embed.FS

func prompt(name string) string {
	b, err := prompts.ReadFile("prompts/" + name + ".md")
	if err != nil {
		panic(err)
	}
	return string(b)
}

// exomecha's own comments go in marked as its questions, because a person's answer often makes
// sense only next to the question. Comments from people who cannot write to the repository are
// left out so outsiders cannot steer the AI.
func issueComments(ctx context.Context, gh *github.Client, n int) (string, error) {
	comments, err := gh.Comments(ctx, n)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range comments {
		if !c.Trusted {
			continue
		}
		from := "person"
		if c.Exomecha {
			from = "exomecha"
		}
		fmt.Fprintf(&b, "### %s (%s)\n\n%s\n\n", c.CreatedAt.Local().Format(time.DateTime), from, strings.TrimSpace(c.Body))
	}
	if b.Len() == 0 {
		return "", nil
	}
	return "## Comments\n\nOldest first. A person's comments add to or correct the body. If one conflicts with the body, follow the latest comment from a person. " +
		"exomecha's comments are questions it asked earlier. They show what a person's answer refers to and are not instructions.\n\n" + b.String(), nil
}

func parentPRD(ctx context.Context, gh *github.Client, n int) (string, error) {
	p, ok, err := gh.Parent(ctx, n)
	if err != nil || !ok {
		return "", err
	}
	return fmt.Sprintf("## Parent PRD #%d: %s (why it was decided this way)\n\n%s\n\n", p.Number, p.Title, strings.TrimSpace(p.Body)), nil
}

func taskText(t github.Issue) string {
	return fmt.Sprintf("# Task #%d: %s\n\n%s\n\n", t.Number, t.Title, strings.TrimSpace(t.Body))
}

func branchName(n int) string {
	return fmt.Sprintf("exomecha/task-%d", n)
}

// baseCommit fetches, then returns the base branch's commit.
func baseCommit(ctx context.Context, g *git.Git) (string, error) {
	if err := g.Sync(ctx); err != nil {
		return "", err
	}
	base, ok, err := g.Rev(ctx, g.BaseRef())
	if err == nil && !ok {
		err = fmt.Errorf("no %s", g.BaseRef())
	}
	return base, err
}

// taskPR returns the Task's open PR, with ok false when it has none.
func taskPR(ctx context.Context, gh *github.Client, n int) (pr github.PR, ok bool, err error) {
	number, ok, err := gh.OpenPR(ctx, branchName(n))
	if err != nil || !ok {
		return pr, false, err
	}
	pr, err = gh.PR(ctx, number)
	return pr, err == nil, err
}

// holds reports whether the issue is open, has the kind label, and has status as its only status.
// exomecha changes a status in one call, so two statuses are a person's doing, and every worker
// leaves the issue to them.
func holds(i github.Issue, kind string, status label.Status) bool {
	if !i.Open || !i.HasLabel(kind) {
		return false
	}
	statuses := 0
	for _, l := range i.Labels {
		if label.IsStatus(l) {
			statuses++
		}
	}
	return statuses == 1 && i.HasLabel(string(status))
}

// back is where the person sends the Task after answering, so it returns to the worker that
// owns the problem: pending leads back to builder, review leads back to resolver. A PRD has no
// status to go back to, and splitter takes it again once the stuck label is gone.
func toStuck(ctx context.Context, gh *github.Client, n int, from, back label.Status, reason string) error {
	trimmed := trimClosingTags(reason)
	if trimmed != strings.TrimSpace(reason) {
		slog.Warn("trimmed closing tags from a stuck reason", "task", n, "reason", reason)
	}
	issue, again := "Task", "relabel it `"+string(back)+"`"
	if back == label.None {
		issue, again = "PRD", "remove the `"+string(label.Stuck)+"` label"
	}
	body := "This " + issue + " is blocked and needs a person.\n\n" + trimmed +
		"\n\nIf there is nothing left to do, close this issue. Otherwise, answer in a comment and " + again + "."
	if err := gh.AddComment(ctx, n, body); err != nil {
		return err
	}
	if err := gh.SetStatus(ctx, n, from, label.Stuck); err != nil {
		return err
	}
	slog.Info("sent to stuck", "task", n)
	return nil
}

var closingTag = regexp.MustCompile(`</[A-Za-z_][\w.:-]*>$`)

// The AI sometimes ends its reason with stray closing tags, such as `</stuck_reason>`.
func trimClosingTags(s string) string {
	s = strings.TrimSpace(s)
	for {
		loc := closingTag.FindStringIndex(s)
		if loc == nil {
			return s
		}
		s = strings.TrimSpace(s[:loc[0]])
	}
}
