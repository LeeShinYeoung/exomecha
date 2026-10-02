package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const builderSchema = `{
  "type": "object",
  "properties": {
    "result": {"type": "string", "enum": ["done", "stuck"]},
    "summary": {"type": "string"},
    "stuck_reason": {"type": "string"}
  },
  "required": ["result", "summary", "stuck_reason"],
  "additionalProperties": false
}`

const builderRole = "builder"

// builder runs claude in one sandbox per Task. Each round it looks at the Task's label and its
// sandbox and takes one step, so after a restart it picks up whatever is left the same way.
type builder struct {
	Deps
	runs *runs
}

func (*builder) Name() string { return "builder" }

// Pick returns the queued Tasks that are ready, every working Task, and every Task that has a
// builder sandbox, whatever its label.
func (b *builder) Pick(ctx context.Context) ([]int, error) {
	queued, err := b.GitHub.ListOpen(ctx, label.Task, string(label.Queued))
	if err != nil {
		return nil, err
	}
	working, err := b.GitHub.ListOpen(ctx, label.Task, string(label.Working))
	if err != nil {
		return nil, err
	}
	found, err := b.runs.found(ctx, builderRole)
	if err != nil {
		return nil, err
	}
	mine := map[int]bool{}
	out := found
	for _, t := range queued {
		mine[t.Number] = true
		ready, err := b.unblocked(ctx, t.Number)
		if err != nil {
			return nil, err
		}
		if ready {
			out = append(out, t.Number)
		}
	}
	for _, t := range working {
		mine[t.Number] = true
		out = append(out, t.Number)
	}
	b.runs.prune(builderRole, mine)
	slices.Sort(out)
	return slices.Compact(out), nil
}

func (b *builder) unblocked(ctx context.Context, n int) (bool, error) {
	blockers, err := b.GitHub.BlockedBy(ctx, n)
	if err != nil {
		return false, err
	}
	for _, x := range blockers {
		if x.Open {
			return false, nil
		}
	}
	return true, nil
}

// Handle returns an error only for a failed step of exomecha's own (sbx, git, GitHub). The state
// stays as it is and the next try waits longer each time.
func (b *builder) Handle(ctx context.Context, n int) error {
	name := b.runs.name(n, builderRole)
	err := b.handle(ctx, n, name)
	if err != nil && ctx.Err() == nil {
		b.runs.strike(name)
	}
	return err
}

func (b *builder) handle(ctx context.Context, n int, name string) error {
	task, err := b.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	r, err := b.runs.read(ctx, name)
	if err != nil {
		return err
	}
	if !holds(task, label.Queued, label.Working) {
		b.runs.forget(name)
		if r.state == none {
			return nil
		}
		slog.Info("removing the sandbox of a Task that left builder", "task", n, "state", r.state)
		return b.runs.remove(ctx, name, false)
	}
	working := task.HasLabel(string(label.Working))
	// A queued Task with a sandbox was sent back by a person, and its run holds a slot for nothing.
	if !working && r.state != none {
		slog.Info("removing the sandbox of a Task that was requeued", "task", n, "state", r.state)
		return b.runs.remove(ctx, name, false)
	}
	if r.state == running || !b.runs.due(name) {
		return nil
	}
	if !working {
		return b.start(ctx, task, name)
	}

	switch r.state {
	case none:
		slog.Info("requeued a working Task that has no sandbox", "task", n)
		return b.GitHub.SetStatus(ctx, n, label.Working, label.Queued)
	case preparing:
		// Only a start holds the Task's lock while preparing, so this one was cut off.
		if err := b.runs.remove(ctx, name, false); err != nil {
			return err
		}
		slog.Info("requeued a Task whose start was cut off", "task", n)
		return b.GitHub.SetStatus(ctx, n, label.Working, label.Queued)
	case stalled:
		return b.failed(ctx, n, name, fmt.Sprintf("claude's output did not change for %d minutes", int(stallAfter.Minutes())), "")
	case dead:
		return b.failed(ctx, n, name, "the sandbox stopped while claude was running", "")
	case gone:
		return b.failed(ctx, n, name, "the sandbox was removed while claude was running", "")
	case finished:
		return b.harvest(ctx, task, name, r.in)
	}
	return nil
}

func (b *builder) start(ctx context.Context, task github.Issue, name string) error {
	n := task.Number
	if ready, err := b.unblocked(ctx, n); err != nil || !ready {
		return err
	}
	if ready, err := b.runs.reserve(ctx, name); err != nil || !ready {
		return err
	}
	defer b.runs.release(name)
	if err := b.GitHub.SetStatus(ctx, n, label.Queued, label.Working); err != nil {
		return err
	}
	problem, err := b.launch(ctx, task, name)
	if problem == "" && err == nil {
		return nil
	}
	// Undo the start even when shutdown cancelled ctx.
	cleanup := context.WithoutCancel(ctx)
	if rerr := b.runs.remove(cleanup, name, false); rerr != nil {
		return errors.Join(err, rerr)
	}
	if problem != "" {
		return toStuck(cleanup, b.GitHub, n, label.Working, label.Pending, problem)
	}
	if rerr := b.GitHub.SetStatus(cleanup, n, label.Working, label.Queued); rerr != nil {
		return errors.Join(err, rerr)
	}
	if errors.Is(err, errNoSlot) {
		return nil
	}
	return err
}

func (b *builder) launch(ctx context.Context, task github.Issue, name string) (problem string, err error) {
	n := task.Number
	branch := branchName(n)
	if err := b.Git.Sync(ctx); err != nil {
		return "", err
	}
	base, ok, err := b.Git.Rev(ctx, b.Git.BaseRef())
	if err != nil || !ok {
		return "", errors.Join(err, fmt.Errorf("no %s", b.Git.BaseRef()))
	}
	remote, hasRemote, err := b.Git.Rev(ctx, "origin/"+branch)
	if err != nil {
		return "", err
	}
	pr, hasPR, err := b.GitHub.OpenPR(ctx, branch)
	if err != nil {
		return "", err
	}
	in := inputs{Head: base, Remote: remote, Base: base}
	var existing string
	if hasPR {
		if !hasRemote {
			return "", fmt.Errorf("PR #%d has no branch %s", pr, branch)
		}
		in.Head, in.PR = remote, pr
		existing = fmt.Sprintf("## Existing PR\n\nPR #%d already has earlier work on this Task. The current directory is its branch.\n\n", pr)
		review, err := b.GitHub.LatestReview(ctx, pr)
		if err != nil {
			return "", err
		}
		if review != "" {
			existing += "## Last review (exomecha's latest review on this PR: changes to make or CI failures)\n\n" + review + "\n"
		}
	}
	comments, err := issueComments(ctx, b.GitHub, n)
	if err != nil {
		return "", err
	}
	return b.runs.open(ctx, launch{
		task:    n,
		name:    name,
		branch:  branch,
		in:      in,
		request: agent.Request{Instructions: prompt("builder"), Schema: builderSchema},
		input: func(_ context.Context, ranSetup bool, _ *inputs) (string, string, error) {
			input := taskText(task) + comments
			if ranSetup {
				input += setupNote + " It ran before you started.\n\n"
			}
			return input + existing, "", nil
		},
	})
}

func (b *builder) harvest(ctx context.Context, task github.Issue, name string, in inputs) error {
	n := task.Number
	branch := branchName(n)
	res, failure, detail, limited := b.runs.outcome(name, true)
	if limited {
		slog.Warn("run ended at the Claude usage limit, starting over later", "task", n, "sandbox", name)
		if err := b.runs.remove(ctx, name, false); err != nil {
			return err
		}
		b.runs.strike(name)
		return b.GitHub.SetStatus(ctx, n, label.Working, label.Queued)
	}
	if failure != "" {
		return b.failed(ctx, n, name, failure, detail)
	}
	var out struct {
		Result      string `json:"result"`
		Summary     string `json:"summary"`
		StuckReason string `json:"stuck_reason"`
	}
	if err := decode(res, &out); err != nil {
		return b.failed(ctx, n, name, "the AI's result was not in the expected form", err.Error())
	}
	switch out.Result {
	case "stuck":
		return b.stuck(ctx, n, name, out.StuckReason)
	case "done":
	default:
		return b.failed(ctx, n, name, "the AI's result was not in the expected form", fmt.Sprintf("result %q", out.Result))
	}

	if committed, err := b.runs.committed(ctx, name); err != nil {
		return err
	} else if !committed {
		return b.stuck(ctx, n, name, noCommit+"\n\n"+out.Summary)
	}
	head, err := b.runs.fetch(ctx, name)
	if err != nil {
		return err
	}
	if err := b.Git.Sync(ctx); err != nil {
		return err
	}
	// head is there when exomecha already pushed this run and a later step failed.
	if remote, _, err := b.Git.Rev(ctx, "origin/"+branch); err != nil {
		return err
	} else if remote != in.Remote && remote != head {
		slog.Info("Task branch changed on GitHub during the run, starting over", "task", n, "branch", branch)
		if err := b.runs.remove(ctx, name, false); err != nil {
			return err
		}
		return b.GitHub.SetStatus(ctx, n, label.Working, label.Queued)
	}

	if head == in.Head {
		changes, err := b.runs.git(ctx, name, repoDir, "status", "--porcelain")
		if err != nil {
			return err
		}
		if strings.TrimSpace(changes) != "" {
			return b.stuck(ctx, n, name, "The AI finished without committing its changes.\n\n"+out.Summary)
		}
		return b.stuck(ctx, n, name, "Nothing to change (it may already be done).\n\n"+out.Summary)
	}
	// GitHub refuses a push that rewrites the branch, and it would refuse it again on every retry.
	kept, err := b.Git.IsAncestor(ctx, in.Head, head)
	if err != nil {
		return err
	}
	if !kept {
		return b.stuck(ctx, n, name, "The AI rewrote commits that were already on the branch, so they cannot be pushed.\n\n"+out.Summary)
	}
	// A run that started with no PR began from the base branch, so it replaces any old branch of the same name.
	if err := b.Git.PushCommit(ctx, head, branch, in.PR == 0); err != nil {
		return err
	}
	pr, hasPR, err := b.GitHub.OpenPR(ctx, branch)
	if err != nil {
		return err
	}
	if !hasPR {
		// "Closes" is what makes GitHub link the PR and the Task to each other.
		pr, err = b.GitHub.CreatePR(ctx, branch, b.Git.Base(), task.Title, fmt.Sprintf("Closes #%d\n\n%s", n, out.Summary))
		if err != nil {
			return err
		}
	}
	if err := b.GitHub.SetStatus(ctx, n, label.Working, label.Review); err != nil {
		return err
	}
	slog.Info("pushed PR", "task", n, "pr", pr)
	b.runs.forget(name)
	return b.runs.remove(ctx, name, false)
}

func (b *builder) stuck(ctx context.Context, n int, name, reason string) error {
	if err := toStuck(ctx, b.GitHub, n, label.Working, label.Pending, reason); err != nil {
		return err
	}
	b.runs.forget(name)
	return b.runs.remove(ctx, name, false)
}

// failed removes a run that failed and starts the Task over, or after maxFailures in a row sends it to stuck.
func (b *builder) failed(ctx context.Context, n int, name, why, detail string) error {
	slog.Warn("builder run failed", "task", n, "sandbox", name, "why", why, "detail", detail, "stderr", b.runs.tail(name, "stderr.log"))
	// Stuck goes first. A Task left working with no sandbox by a failed GitHub call would start again.
	if count := b.runs.failures(name) + 1; count >= maxFailures {
		if err := toStuck(ctx, b.GitHub, n, label.Working, label.Pending, failedReason(count, why)); err != nil {
			return err
		}
		b.runs.forget(name)
		return b.runs.remove(ctx, name, true)
	}
	if err := b.runs.remove(ctx, name, true); err != nil {
		return err
	}
	b.runs.fail(name)
	return b.GitHub.SetStatus(ctx, n, label.Working, label.Queued)
}
