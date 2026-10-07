package worker

import (
	"cmp"
	"context"
	"embed"
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
	// Handle sends a PRD to stuck and returns nil when the PRD itself is the problem. Any other error
	// is an environment error: leave the PRD where the next check can retry it, and return the error.
	// The loop puts off the next try after an error, longer each time in a row, so a PRD that only
	// has to wait returns nil.
	Handle(ctx context.Context, number int) error
}

type Deps struct {
	GitHub *github.Client
	Git    *git.Git

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
	// Models has a model for every tier.
	Models map[label.Tier]agent.Model
	// Zero takes the default. Only the e2e tests shorten them.
	Interval   time.Duration // between two checks of a worker, and the first wait after an error
	CIGrace    time.Duration
	StallAfter time.Duration
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
	runs := newRuns(d, gate)
	workers := []Worker{
		&builder{d, runs},
		&reviewer{d, runs},
		&resolver{d, runs},
	}
	every := cmp.Or(d.Interval, interval)
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() { loop(ctx, w, l, every) })
	}
	wg.Wait()
}

func loop(ctx context.Context, w Worker, l *locks, every time.Duration) {
	var wg sync.WaitGroup
	defer wg.Wait()
	d := &delays{now: time.Now, first: every, items: map[int]delay{}}
	tick := time.NewTicker(every)
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
	first time.Duration // the wait after one error. Zero is minBackoff.
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
	it.next = from.Add(min(maxBackoff, cmp.Or(d.first, minBackoff)<<min(it.strikes-1, 10)))
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

// A person may answer on the PR instead of on the PRD. exomecha's reviews are left out: builder gets
// the latest one on its own, and an approval is not an instruction.
func prComments(ctx context.Context, gh *github.Client, pr int) (string, error) {
	comments, err := gh.PRComments(ctx, pr)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range comments {
		if !c.Trusted || c.Exomecha {
			continue
		}
		where := ""
		if c.Path != "" {
			where = ", on " + c.Path
		}
		if c.Line > 0 {
			where += fmt.Sprintf(" line %d", c.Line)
		}
		fmt.Fprintf(&b, "### %s (person%s)\n\n%s\n\n", c.CreatedAt.Local().Format(time.DateTime), where, strings.TrimSpace(c.Body))
	}
	if b.Len() == 0 {
		return "", nil
	}
	return "## Comments on the PR\n\nOldest first: a person's comments on the conversation, in reviews, and on lines of the diff. They add to or correct the body like a person's comments on the PRD. " +
		"Commits after a comment may already have done what it asks.\n\n" + b.String(), nil
}

func prdText(t github.Issue) string {
	return fmt.Sprintf("# PRD #%d: %s\n\n%s\n\n", t.Number, t.Title, strings.TrimSpace(t.Body))
}

func branchName(n int) string {
	return fmt.Sprintf("exomecha/prd-%d", n)
}

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

func prdPR(ctx context.Context, gh *github.Client, n int) (pr github.PR, ok bool, err error) {
	number, ok, err := gh.OpenPR(ctx, branchName(n))
	if err != nil || !ok {
		return pr, false, err
	}
	pr, err = gh.PR(ctx, number)
	return pr, err == nil, err
}

// conflicting splits the PRDs in review between the workers: an open PR that conflicts is
// resolver's, and any other, even one GitHub has not checked yet, is reviewer's.
func conflicting(pr github.PR, ok bool) bool {
	return ok && pr.Mergeable != nil && !*pr.Mergeable
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

// back is where the person sends the PRD after answering, so it returns to the worker that
// owns the problem: ready leads back to builder, review leads back to resolver.
func toStuck(ctx context.Context, gh *github.Client, n int, from, back label.Status, reason string) error {
	trimmed := trimClosingTags(reason)
	if trimmed != strings.TrimSpace(reason) {
		slog.Warn("trimmed closing tags from a stuck reason", "prd", n, "reason", reason)
	}
	body := "This PRD is blocked and needs a person.\n\n" + trimmed +
		"\n\nIf there is nothing left to do, close this issue. Otherwise, answer in a comment and relabel it `" + string(back) + "`."
	if err := gh.AddComment(ctx, n, body); err != nil {
		return err
	}
	if err := gh.SetStatus(ctx, n, from, label.Stuck); err != nil {
		return err
	}
	slog.Info("sent to stuck", "prd", n)
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
