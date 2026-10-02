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

var builderRole = role{
	name:    "builder",
	queued:  label.Queued,
	working: label.Working,
	from:    label.Working,
	back:    label.Pending,
}

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
	out := found
	for _, t := range queued {
		ready, err := b.unblocked(ctx, t.Number)
		if err != nil {
			return nil, err
		}
		if ready {
			out = append(out, t.Number)
		}
	}
	for _, t := range working {
		out = append(out, t.Number)
	}
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
// stays as it is, so the next try repeats the step.
func (b *builder) Handle(ctx context.Context, n int) error {
	name := b.runs.name(builderRole, n)
	task, err := b.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	r, err := b.runs.read(ctx, name)
	if err != nil {
		return err
	}
	if !holds(task, label.Queued, label.Working) {
		if r.state == none {
			return nil
		}
		return b.runs.drop(ctx, builderRole, n)
	}
	working := task.HasLabel(string(label.Working))
	// A queued Task with a sandbox was sent back by a person, and its run holds a slot for nothing.
	if !working && r.state != none {
		return b.runs.drop(ctx, builderRole, n)
	}
	if r.state == running {
		return nil
	}
	if !working {
		return b.start(ctx, task, name)
	}

	switch r.state {
	case none:
		return b.runs.restart(ctx, builderRole, n, "the Task is working with no run")
	case preparing:
		// Only a start holds the Task's lock while preparing, so this one was cut off.
		return b.runs.restart(ctx, builderRole, n, "its start was cut off")
	case stalled:
		return b.runs.failed(ctx, builderRole, n, fmt.Sprintf("claude's output did not change for %d minutes", int(stallAfter.Minutes())), "")
	case dead:
		return b.runs.failed(ctx, builderRole, n, "the sandbox stopped while claude was running", "")
	case gone:
		return b.runs.failed(ctx, builderRole, n, "the sandbox was removed while claude was running", "")
	case finished:
		return b.harvest(ctx, task, r.in)
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
	in := inputs{Base: base, Head: base, Remote: remote}
	var existing string
	if hasPR {
		if !hasRemote {
			return "", fmt.Errorf("PR #%d has no branch %s", pr, branch)
		}
		in.Head = remote
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

func (b *builder) harvest(ctx context.Context, task github.Issue, in inputs) error {
	n := task.Number
	branch := branchName(n)
	var out struct {
		Result      string `json:"result"`
		Summary     string `json:"summary"`
		StuckReason string `json:"stuck_reason"`
	}
	if ok, err := b.runs.result(ctx, builderRole, n, &out); err != nil || !ok {
		return err
	}
	switch out.Result {
	case "stuck":
		return b.runs.stuck(ctx, builderRole, n, out.StuckReason)
	case "done":
	default:
		return b.runs.failed(ctx, builderRole, n, badResult, fmt.Sprintf("result %q", out.Result))
	}

	head, ok, err := b.runs.fetch(ctx, builderRole, n)
	if err != nil {
		return err
	}
	if !ok {
		return b.runs.stuck(ctx, builderRole, n, noCommit+"\n\n"+out.Summary)
	}
	moved, err := b.runs.moved(ctx, n, in, head)
	if err != nil {
		return err
	}
	if moved {
		return b.runs.restart(ctx, builderRole, n, "the Task branch changed on GitHub during the run")
	}

	if head == in.Head {
		changes, err := b.runs.sh(ctx, builderRole, n, "git status --porcelain")
		if err != nil {
			return err
		}
		if strings.TrimSpace(changes) != "" {
			return b.runs.stuck(ctx, builderRole, n, "The AI finished without committing its changes.\n\n"+out.Summary)
		}
		return b.runs.stuck(ctx, builderRole, n, "Nothing to change (it may already be done).\n\n"+out.Summary)
	}
	// GitHub refuses a push that rewrites the branch, and it would refuse it again on every retry.
	kept, err := b.Git.IsAncestor(ctx, in.Head, head)
	if err != nil {
		return err
	}
	if !kept {
		return b.runs.stuck(ctx, builderRole, n, "The AI rewrote commits that were already on the branch, so they cannot be pushed.\n\n"+out.Summary)
	}
	// A run that did not start from the branch began from the base branch, so it replaces any old
	// branch of the same name.
	if err := b.Git.PushCommit(ctx, head, branch, in.Head != in.Remote); err != nil {
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
	return b.runs.done(ctx, builderRole, n)
}
