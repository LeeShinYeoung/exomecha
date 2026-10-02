package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

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
	schema:  builderSchema,
	queued:  label.Queued,
	working: label.Working,
	from:    label.Working,
	back:    label.Pending,
}

// A run of a ready Task was left when a person sent the Task back to the queue, and it holds a
// slot for nothing.
var builderTable = table{
	// none, preparing, running, stalled, dead, gone, finished
	away:    {idle, drop, drop, drop, drop, drop, drop},
	ready:   {begin, drop, drop, drop, drop, drop, drop},
	working: {restart, restart, idle, fail, fail, fail, harvest},
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

func (b *builder) Handle(ctx context.Context, n int) error {
	task, err := b.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	return b.runs.step(ctx, builderRole, n, builderTable[b.place(task)],
		func(ctx context.Context) error { return b.start(ctx, task) },
		func(ctx context.Context, run run) error { return b.collect(ctx, task, run.in) })
}

func (b *builder) place(task github.Issue) place {
	switch {
	case !holds(task, label.Task, label.Queued, label.Working):
		return away
	// Both labels are left by a move between them that failed halfway. As working, the Task
	// starts over, which finishes the move to queued.
	case task.HasLabel(string(label.Working)):
		return working
	}
	return ready
}

func (b *builder) start(ctx context.Context, task github.Issue) error {
	n := task.Number
	if ready, err := b.unblocked(ctx, n); err != nil || !ready {
		return err
	}
	return b.runs.start(ctx, builderRole, n, func(ctx context.Context) (spec, error) {
		branch := branchName(n)
		base, err := baseCommit(ctx, b.Git)
		if err != nil {
			return spec{}, err
		}
		remote, hasRemote, err := b.Git.Rev(ctx, "origin/"+branch)
		if err != nil {
			return spec{}, err
		}
		pr, hasPR, err := b.GitHub.OpenPR(ctx, branch)
		if err != nil {
			return spec{}, err
		}
		comments, err := issueComments(ctx, b.GitHub, n)
		if err != nil {
			return spec{}, err
		}
		s := spec{branch: branch, base: base, head: base, remote: remote, input: taskText(task) + comments}
		if !hasPR {
			return s, nil
		}
		if !hasRemote {
			return spec{}, fmt.Errorf("PR #%d has no branch %s", pr, branch)
		}
		s.head = remote
		s.input += fmt.Sprintf("## Existing PR\n\nPR #%d already has earlier work on this Task. The current directory is its branch.\n\n", pr)
		review, err := b.GitHub.LatestReview(ctx, pr)
		if err != nil {
			return spec{}, err
		}
		if review != "" {
			s.input += "## Last review (exomecha's latest review on this PR: changes to make or CI failures)\n\n" + review + "\n"
		}
		return s, nil
	})
}

func (b *builder) collect(ctx context.Context, task github.Issue, in inputs) error {
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
