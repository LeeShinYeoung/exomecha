package worker

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
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
	Agent  agent.Agent // runs claude on this machine, for splitter, sequencer and reviewer

	// builder and resolver run claude in sandboxes.
	Sandboxes   Sandboxes
	Environment Environment
	// SandboxDir holds one directory per task sandbox, shared with that sandbox.
	SandboxDir string
	// SandboxPrefix starts the name of every task sandbox of this repository and nothing else.
	SandboxPrefix string
	// SandboxLimit is how many task sandboxes may exist at once.
	SandboxLimit int
	// The commit identity for task sandboxes, the token owner's. A sandbox has none of its own.
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
	// builder and resolver check the gate themselves, only before they start a run, so runs
	// already in sandboxes are still watched and finished while usage is over the limit.
	runs := newRuns(d, gate)
	workers := []struct {
		worker Worker
		limit  int // 0 means no limit
		gate   *Gate
	}{
		{&splitter{d}, 1, gate},
		{&sequencer{d}, 1, gate},
		{&builder{d, runs}, 0, nil},
		{&reviewer{d}, 0, gate},
		{&resolver{d, runs}, 0, nil},
	}
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() { loop(ctx, w.worker, w.limit, l, w.gate) })
	}
	wg.Wait()
}

func loop(ctx context.Context, w Worker, limit int, l *locks, gate *Gate) {
	var sem chan struct{}
	if limit > 0 {
		sem = make(chan struct{}, limit)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	d := &delays{now: time.Now, items: map[int]delay{}}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		round(ctx, w, sem, l, d, &wg, gate)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func round(ctx context.Context, w Worker, sem chan struct{}, l *locks, d *delays, wg *sync.WaitGroup, gate *Gate) {
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
	var due []int
	for _, n := range numbers {
		if d.due(n) {
			due = append(due, n)
		}
	}
	// Checking only when there is work keeps an idle exomecha from spending any Claude usage.
	if len(due) == 0 {
		return
	}
	if gate != nil {
		if open, err := gate.Open(ctx); err != nil || !open {
			if err != nil && ctx.Err() == nil {
				log.Error("usage check failed", "err", err)
			}
			return
		}
	}
	for _, n := range due {
		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		release := func() {
			if sem != nil {
				<-sem
			}
		}
		if !l.lock(n) {
			release()
			continue
		}
		wg.Go(func() {
			defer release()
			defer l.unlock(n)
			err := w.Handle(ctx, n)
			if err == nil {
				d.forget(n)
				return
			}
			// An error from shutdown says nothing about the issue.
			if ctx.Err() == nil {
				d.strike(n)
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

func (d *delays) due(n int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	it, ok := d.items[n]
	return !ok || !d.now().Before(it.next)
}

func (d *delays) strike(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	it := d.items[n]
	it.strikes++
	it.next = d.now().Add(min(maxBackoff, minBackoff<<min(it.strikes-1, 10)))
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

func decode(res agent.Result, v any) error {
	if err := json.Unmarshal(res.Structured, v); err != nil {
		return fmt.Errorf("read AI result: %w", err)
	}
	return nil
}

func reload(ctx context.Context, gh *github.Client, n int, labels ...string) (issue github.Issue, ok bool, err error) {
	issue, err = gh.Issue(ctx, n)
	if err != nil {
		return issue, false, err
	}
	if !issue.Open {
		return issue, false, nil
	}
	for _, name := range labels {
		if !issue.HasLabel(name) {
			return issue, false, nil
		}
	}
	return issue, true, nil
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

// prConflicted reports whether the Task's open PR is one GitHub marks as conflicting.
// Only resolver handles those, so reviewer leaves them alone.
func prConflicted(ctx context.Context, gh *github.Client, n int) (bool, error) {
	number, ok, err := gh.OpenPR(ctx, branchName(n))
	if err != nil || !ok {
		return false, err
	}
	pr, err := gh.PR(ctx, number)
	if err != nil {
		return false, err
	}
	return pr.Mergeable != nil && !*pr.Mergeable, nil
}

// back is where the person sends the Task after answering, so it returns to the worker that
// owns the problem: pending leads back to builder, review leads back to resolver.
func toStuck(ctx context.Context, gh *github.Client, n int, from, back label.Status, reason string) error {
	trimmed := trimClosingTags(reason)
	if trimmed != strings.TrimSpace(reason) {
		slog.Warn("trimmed closing tags from a stuck reason", "task", n, "reason", reason)
	}
	body := "This Task is blocked and needs a person.\n\n" + trimmed +
		"\n\nIf there is nothing left to do, close this issue. Otherwise, answer in a comment and relabel it `" + string(back) + "`."
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
