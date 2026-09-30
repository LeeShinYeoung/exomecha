package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/git"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/label"
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

type builder struct{ Deps }

func (*builder) Name() string { return "builder" }

func (b *builder) Pick(ctx context.Context) ([]int, error) {
	tasks, err := b.GitHub.ListOpen(ctx, label.Task, string(label.Queued))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, t := range tasks {
		blockers, err := b.GitHub.BlockedBy(ctx, t.Number)
		if err != nil {
			return nil, err
		}
		ready := true
		for _, x := range blockers {
			if x.Open {
				ready = false
				break
			}
		}
		if ready {
			out = append(out, t.Number)
		}
	}
	return out, nil
}

func (b *builder) Handle(ctx context.Context, n int) error {
	task, ok, err := reload(ctx, b.GitHub, n, label.Task, string(label.Queued))
	if err != nil || !ok {
		return err
	}
	if err := b.GitHub.SetStatus(ctx, n, label.Queued, label.Working); err != nil {
		return err
	}
	if err := b.build(ctx, task); err != nil {
		// Requeue even when shutdown cancelled ctx.
		if rerr := b.GitHub.SetStatus(context.WithoutCancel(ctx), n, label.Working, label.Queued); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return nil
}

func (b *builder) build(ctx context.Context, task github.Issue) error {
	n := task.Number
	branch := branchName(n)
	pr, hasPR, err := b.GitHub.OpenPR(ctx, branch)
	if err != nil {
		return err
	}

	var wt *git.Worktree
	if hasPR {
		wt, err = b.Git.OpenBranch(ctx, branch)
	} else {
		wt, err = b.Git.NewBranch(ctx, branch)
	}
	if err != nil {
		return err
	}
	defer wt.Remove()
	before, err := wt.Head(ctx)
	if err != nil {
		return err
	}

	comments, err := issueComments(ctx, b.GitHub, n)
	if err != nil {
		return err
	}
	input := taskText(task) + comments
	if hasPR {
		input += fmt.Sprintf("## Existing PR\n\nPR #%d already has earlier work on this Task. The current directory is its branch.\n\n", pr)
		review, err := b.GitHub.LatestReview(ctx, pr)
		if err != nil {
			return err
		}
		if review != "" {
			input += "## Last review (coworker's latest review on this PR: changes to make or CI failures)\n\n" + review + "\n"
		}
	}

	res, err := b.Agent.Run(ctx, agent.Request{
		Dir:          wt.Dir,
		Instructions: prompt("builder"),
		Input:        input,
		Schema:       builderSchema,
	})
	if err != nil {
		return err
	}
	var out struct {
		Result      string `json:"result"`
		Summary     string `json:"summary"`
		StuckReason string `json:"stuck_reason"`
	}
	if err := decode(res, &out); err != nil {
		return err
	}
	switch out.Result {
	case "stuck":
		return toStuck(ctx, b.GitHub, n, label.Working, label.Pending, out.StuckReason)
	case "done":
	default:
		return fmt.Errorf("unknown builder result: %q", out.Result)
	}

	head, err := wt.Head(ctx)
	if err != nil {
		return err
	}
	if head == before {
		changed, err := wt.HasChanges(ctx)
		if err != nil {
			return err
		}
		if changed {
			return toStuck(ctx, b.GitHub, n, label.Working, label.Pending, "The AI finished without committing its changes.\n\n"+out.Summary)
		}
		return toStuck(ctx, b.GitHub, n, label.Working, label.Pending, "Nothing to change (it may already be done).\n\n"+out.Summary)
	}
	// GitHub refuses a push that rewrites the branch, and it would refuse it again on every retry.
	kept, err := wt.IsAncestor(ctx, before, head)
	if err != nil {
		return err
	}
	if !kept {
		return toStuck(ctx, b.GitHub, n, label.Working, label.Pending, "The AI rewrote commits that were already on the branch, so they cannot be pushed.\n\n"+out.Summary)
	}
	if err := wt.Push(ctx); err != nil {
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
	return nil
}
