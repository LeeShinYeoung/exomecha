package worker

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/git"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/logging"
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
	// GitHub is the PRD repository's. Its issues are the PRDs.
	GitHub *github.Client
	// PRDRepo is the PRD repository, as owner/name.
	PRDRepo string
	// Repos are the repositories PRDs may change.
	Repos []*Repo
	// SetupDir holds the files that prepare a project run's sandbox, which /exomecha:setup writes.
	SetupDir string
	// Secrets go into every project run's sandbox. sbx hands the AI only their placeholders.
	Secrets []sandbox.Secret

	Sandboxes   Sandboxes
	Environment Environment
	// SandboxDir holds one directory per run, shared with the run's sandbox.
	SandboxDir string
	// OutputDir keeps what the runs that went to stuck printed, by issue.
	OutputDir string
	// SandboxPrefix starts the name of every run's sandbox of this repository and nothing else.
	SandboxPrefix string
	// ClaudeDir holds the claude user settings every run's sandbox gets. Empty gives none.
	ClaudeDir string
	// How many sandboxes of project runs, and of reader runs, may exist at once.
	ProjectLimit int
	ReadLimit    int
	// How many times reviewer sends a PR back to builder before a person decides.
	ReviewLimit int
	// The commit identity for project runs, the token owner's. A sandbox has none of its own.
	GitName  string
	GitEmail string
	// Models has a model for every tier.
	Models        map[label.Tier]agent.Model
	ResolverModel agent.Model
	// Zero takes the default. Only the e2e tests shorten them.
	Interval time.Duration // between two checks of a worker, and the first wait after an error
	CIGrace  time.Duration
}

type Sandboxes interface {
	List(ctx context.Context) ([]sandbox.Box, error)
	// Create makes a sandbox that keeps running with no exec session open. An empty template
	// is sbx's own claude template. Hosts in deny are blocked from the start.
	Create(ctx context.Context, name, template, workspace string, deny []string) error
	Allow(ctx context.Context, name string, hosts []string) error
	Exec(ctx context.Context, name, dir string, env []string, args ...string) ([]byte, error)
	// Start returns at once and leaves args running after the exec session ends.
	Start(ctx context.Context, name, dir string, env []string, args ...string) error
	// SetSecret has sbx keep s for the sandbox only, until the sandbox is removed.
	SetSecret(ctx context.Context, name string, s sandbox.Secret) error
	// Remove of a sandbox that does not exist is not an error.
	Remove(ctx context.Context, name string) error
	GitURL(name, dir string) string
}

// Environment is the sandbox template and network policy the project's setup files ask for.
// An error whose chain holds a value with a Why() string method is one only a person can fix,
// and Why() is what to tell them. An Output() string method on it gives what the build printed.
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
	shared := newRuns(d, gate)
	workers := []Worker{
		&builder{d, shared},
		&reviewer{d, shared},
		&resolver{d, shared},
	}
	every := cmp.Or(d.Interval, interval)
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Go(func() { loop(ctx, w, l, every) })
	}
	wg.Wait()
}

func loop(ctx context.Context, w Worker, l *locks, every time.Duration) {
	ctx = logging.With(ctx, logging.Worker, w.Name())
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
	numbers, err := w.Pick(ctx)
	if err != nil {
		if ctx.Err() == nil {
			attempt := d.missPick()
			slog.ErrorContext(ctx, "listing PRDs failed", logging.Attempt, attempt, logging.Err, err)
		}
		return
	}
	if attempt := d.resetPicks(); attempt > 1 {
		slog.InfoContext(ctx, "listing PRDs works again", logging.Attempt, attempt)
	}
	d.forgetUnpicked(numbers)
	for _, n := range numbers {
		if !d.due(n, now) || !l.tryLock(n) {
			continue
		}
		wg.Go(func() {
			defer l.unlock(n)
			ctx := logging.With(ctx, logging.PRD, n)
			err := w.Handle(ctx, n)
			if err == nil {
				if attempt := d.forget(n); attempt > 1 {
					slog.InfoContext(ctx, "handling the PRD works again", logging.Attempt, attempt)
				}
				return
			}
			// An error from shutdown says nothing about the issue, and the next start finishes the step.
			if ctx.Err() != nil {
				slog.InfoContext(ctx, "shutdown cut off a step", logging.Err, err)
				return
			}
			attempt := d.strike(n, now)
			slog.ErrorContext(ctx, "handling the PRD failed", logging.Attempt, attempt, logging.Err, err)
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
	picks int // Picks that failed in a row
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

// strike returns the attempt that failed.
func (d *delays) strike(n int, from time.Time) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	it := d.items[n]
	it.strikes++
	it.next = from.Add(min(maxBackoff, cmp.Or(d.first, minBackoff)<<min(it.strikes-1, 10)))
	d.items[n] = it
	return it.strikes
}

// forget returns the attempt that worked.
func (d *delays) forget(n int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	attempt := d.items[n].strikes + 1
	delete(d.items, n)
	return attempt
}

// missPick returns the attempt that failed. Only the loop calls it and resetPicks, so they need no
// lock.
func (d *delays) missPick() int {
	d.picks++
	return d.picks
}

// resetPicks returns the attempt that worked.
func (d *delays) resetPicks() int {
	attempt := d.picks + 1
	d.picks = 0
	return attempt
}

// forgetUnpicked forgets the issues Pick no longer returns, so one that comes back later starts
// with no wait.
func (d *delays) forgetUnpicked(picked []int) {
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

func (l *locks) tryLock(n int) bool {
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

// prompt is what a worker reads before its input: how exomecha works, which every worker reads
// the same, then the worker's own job.
func prompt(name string) string {
	var b strings.Builder
	for _, file := range []string{"process", name} {
		text, err := prompts.ReadFile("prompts/" + file + ".md")
		if err != nil {
			panic(err)
		}
		b.Write(text)
		b.WriteString("\n")
	}
	return b.String()
}

// repoWithPR is one repository of a run and the PRD's open PR there.
type repoWithPR struct {
	repo *Repo
	pr   int // the PRD's open PR there, 0 for none
}

// sharedInput is the start of every run's input: the PRD, where its work is, and everything
// written on the PRD and on its open PRs, oldest first. Every worker's run starts with it, so each
// sees what people asked and what the other workers did. Only what people who can write to the
// repository wrote goes in, so outsiders cannot steer the AI.
func sharedInput(ctx context.Context, d Deps, prd github.Issue, repos []repoWithPR) (string, error) {
	type entry struct {
		github.Comment
		where string
	}
	var entries []entry
	comments, err := d.GitHub.Comments(ctx, prd.Number)
	if err != nil {
		return "", err
	}
	for _, c := range comments {
		if c.Trusted {
			entries = append(entries, entry{c, "PRD"})
		}
	}
	seen := map[string]bool{}
	for _, rr := range repos {
		if rr.pr == 0 {
			continue
		}
		comments, err := rr.repo.GitHub.PRComments(ctx, rr.pr)
		if err != nil {
			return "", err
		}
		for _, c := range comments {
			// Before the dedupe, or an outsider's copy of an entry would hide it.
			if !c.Trusted {
				continue
			}
			// exomecha posts the same entry on each of a PRD's PRs.
			key := c.Note.Worker + "\x00" + c.Note.Kind + "\x00" + strings.TrimSpace(c.Body)
			if c.Exomecha && seen[key] {
				continue
			}
			seen[key] = c.Exomecha
			entries = append(entries, entry{c, "PR " + d.issueRef(d.PRDRepo, rr.repo.Name, rr.pr)})
		}
	}
	slices.SortStableFunc(entries, func(a, b entry) int { return a.CreatedAt.Compare(b.CreatedAt) })

	var b strings.Builder
	fmt.Fprintf(&b, "# PRD #%d: %s\n\n%s\n\n## Run\n\n", prd.Number, prd.Title, strings.TrimSpace(prd.Body))
	b.WriteString(d.runSection(prd.Number, repos))
	b.WriteString("## Record\n\nOldest first. Each entry says who wrote it and where.\n\n")
	for _, e := range entries {
		where := e.where
		if e.Path != "" {
			where += ", on " + e.Path
		}
		if e.Line > 0 {
			where += fmt.Sprintf(" line %d", e.Line)
		}
		fmt.Fprintf(&b, "### %s (%s, %s)\n\n%s\n\n", e.CreatedAt.Local().Format(time.DateTime), writer(e.Comment), where, strings.TrimSpace(e.Body))
	}
	if len(entries) == 0 {
		b.WriteString("Nothing yet.\n\n")
	}
	return b.String(), nil
}

// runSection is the input's Run section: where each repository is, its PR, and its base branch.
func (d Deps) runSection(n int, repos []repoWithPR) string {
	var b strings.Builder
	write := func(rr repoWithPR, gitCmd string) {
		if rr.pr != 0 {
			fmt.Fprintf(&b, "- PR %s, on branch `%s`.\n", d.issueRef(d.PRDRepo, rr.repo.Name, rr.pr), branchName(n))
		}
		base := rr.repo.Git.BaseRef()
		fmt.Fprintf(&b, "- Base branch `%s`. See the changes on this branch with `%s diff %s...HEAD`.\n", base, gitCmd, base)
		fmt.Fprintf(&b, "- Commit messages here end with `PRD: %s`.\n\n", d.ref(rr.repo.Name, n))
	}
	if len(repos) == 1 {
		write(repos[0], "git")
		return b.String()
	}
	b.WriteString("This PRD changes several repositories. Each is in a folder of its own in this directory.\n\n")
	for _, rr := range repos {
		fmt.Fprintf(&b, "### `%s/`: %s\n\n", rr.repo.folder(), rr.repo.Name)
		if rr.pr == 0 {
			b.WriteString("- No PR.\n")
		}
		write(rr, "git -C "+rr.repo.folder())
	}
	return b.String()
}

func writer(c github.Comment) string {
	switch {
	case !c.Exomecha:
		return "person"
	case c.Note.Worker == "":
		return "exomecha"
	case c.Note.Kind == github.KindCI:
		return c.Note.Worker + " CI failed"
	}
	return c.Note.Worker + " " + c.Note.Kind
}

func branchName(n int) string {
	return fmt.Sprintf("exomecha/prd-%d", n)
}

func fetchBase(ctx context.Context, g *git.Git) (string, error) {
	if err := g.Sync(ctx); err != nil {
		return "", err
	}
	base, ok, err := g.Rev(ctx, g.BaseRef())
	if err == nil && !ok {
		err = fmt.Errorf("no %s", g.BaseRef())
	}
	return base, err
}

// holds reports whether the issue is an open PRD with status as its only status.
// exomecha changes a status in one call, so two statuses are a person's doing, and every worker
// leaves the issue to them.
func holds(i github.Issue, status label.Status) bool {
	if !i.Open || !i.HasLabel(label.PRD) {
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
// owns the problem: ready leads back to builder, review leads back to resolver. kept is where the
// run's output is kept, or "" for none. The comment names the place and never holds the output.
func toStuck(ctx context.Context, gh *github.Client, n int, worker string, from, back label.Status, reason, kept string) error {
	body := "This PRD is blocked and needs a person.\n\n" + trimClosingTags(reason)
	if kept != "" {
		body += "\n\nThe output of this run is kept on the machine running exomecha, in `" + tilde(kept) + "`."
	}
	body += "\n\nIf there is nothing left to do, close this issue. Otherwise, answer in a comment and relabel it `" + string(back) + "`."
	if err := gh.AddComment(ctx, n, github.Note{Worker: worker, Kind: github.KindStuck}, body); err != nil {
		return err
	}
	return gh.SetStatus(ctx, n, from, label.Stuck)
}

// posted reports whether the newest note of n on PR pr already says text. When a later step
// fails, the next try collects the same run, or requeues for the same CI runs, and writes the same
// text, so a match is an earlier try's.
func posted(ctx context.Context, gh *github.Client, pr int, n github.Note, text string) (bool, error) {
	comments, err := gh.PRComments(ctx, pr)
	if err != nil {
		return false, err
	}
	for _, c := range slices.Backward(comments) {
		if !c.Exomecha || !c.Trusted || c.Note != n {
			continue
		}
		said := c.Body
		if n.Kind == github.KindApproved {
			// AddApproval puts a sentence of its own, with no blank line in it, before text.
			_, said, _ = strings.Cut(said, "\n\n")
		}
		return said == strings.TrimSpace(text), nil
	}
	return false, nil
}

// tilde shortens a path in the home directory, whose name gives away this machine's user.
func tilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.Join("~", rel)
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
