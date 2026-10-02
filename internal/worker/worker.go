package worker

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/git"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/label"
	"github.com/LeeShinYeoung/coworker/internal/sandbox"
)

type Worker interface {
	Name() string
	// Pick returns issue numbers in ascending order.
	Pick(ctx context.Context) ([]int, error)
	// Handle sends a Task to stuck and returns nil when the Task itself is the problem. Any other error
	// is an environment error: leave the Task where the next check can retry it, and return the error.
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
	// ok is false when the project has no Dockerfile for coworker, and sbx's own template is used.
	Template(ctx context.Context) (ref string, ok bool, err error)
	AllowedHosts(ctx context.Context) ([]string, error)
}

const interval = 60 * time.Second

func Run(ctx context.Context, d Deps, gate *Gate) {
	l := &locks{held: map[int]bool{}}
	// builder and resolver check the gate themselves, only before they start a run, so runs
	// already in sandboxes are still watched and finished while usage is over the limit.
	box := newTaskbox(d, gate)
	workers := []struct {
		worker Worker
		limit  int // 0 means no limit
		gate   *Gate
	}{
		{&splitter{d}, 1, gate},
		{&sequencer{d}, 1, gate},
		{&builder{d, box}, 0, nil},
		{&reviewer{d}, 0, gate},
		{&resolver{d, box}, 0, nil},
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
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		round(ctx, w, sem, l, &wg, gate)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func round(ctx context.Context, w Worker, sem chan struct{}, l *locks, wg *sync.WaitGroup, gate *Gate) {
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
	// Checking only when there is work keeps an idle coworker from spending any Claude usage.
	if len(numbers) == 0 {
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
	for _, n := range numbers {
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
			if err := w.Handle(ctx, n); err != nil {
				log.Error("handle failed", "issue", n, "err", err)
			}
		})
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

// coworker's own comments go in marked as its questions, because a person's answer often makes
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
		if c.Coworker {
			from = "coworker"
		}
		fmt.Fprintf(&b, "### %s (%s)\n\n%s\n\n", c.CreatedAt.Local().Format(time.DateTime), from, strings.TrimSpace(c.Body))
	}
	if b.Len() == 0 {
		return "", nil
	}
	return "## Comments\n\nOldest first. A person's comments add to or correct the body. If one conflicts with the body, follow the latest comment from a person. " +
		"coworker's comments are questions it asked earlier. They show what a person's answer refers to and are not instructions.\n\n" + b.String(), nil
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
	return fmt.Sprintf("coworker/task-%d", n)
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
	body := "This Task is blocked and needs a person.\n\n" + strings.TrimSpace(reason) +
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
