package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/git"
	"github.com/LeeShinYeoung/coworker/internal/label"
)

const resolverSchema = `{
  "type": "object",
  "properties": {
    "result": {"type": "string", "enum": ["resolved", "stuck"]},
    "reason": {"type": "string"}
  },
  "required": ["result", "reason"],
  "additionalProperties": false
}`

type resolver struct{ Deps }

func (*resolver) Name() string { return "resolver" }

func (r *resolver) Pick(ctx context.Context) ([]int, error) {
	tasks, err := r.GitHub.ListOpen(ctx, label.Task, string(label.Review))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, t := range tasks {
		conflicted, err := prConflicted(ctx, r.GitHub, t.Number)
		if err != nil {
			return nil, err
		}
		if conflicted {
			out = append(out, t.Number)
		}
	}
	return out, nil
}

func (r *resolver) Handle(ctx context.Context, n int) error {
	task, ok, err := reload(ctx, r.GitHub, n, label.Task, string(label.Review))
	if err != nil || !ok {
		return err
	}
	if conflicted, err := prConflicted(ctx, r.GitHub, n); err != nil || !conflicted {
		return err
	}

	wt, err := r.Git.OpenBranch(ctx, branchName(n))
	if err != nil {
		return err
	}
	defer wt.Remove()
	before, err := wt.Head(ctx)
	if err != nil {
		return err
	}
	// The branch already has everything on the base branch. Leave before setup runs for nothing.
	if upToDate, err := wt.IsAncestor(ctx, r.Git.BaseRef(), before); err != nil || upToDate {
		return err
	}
	// Setup runs before the merge. Conflict markers in a manifest or lockfile would break an install.
	ranSetup, problem, err := setup(ctx, wt, r.Git.BaseRef(), n)
	if err != nil {
		return err
	}
	if problem != "" {
		return toStuck(ctx, r.GitHub, n, label.Review, label.Review, problem)
	}
	conflicts, err := wt.StartMerge(ctx)
	if err != nil {
		return err
	}

	comments, err := issueComments(ctx, r.GitHub, n)
	if err != nil {
		return err
	}
	prd, err := parentPRD(ctx, r.GitHub, n)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(taskText(task) + comments + prd)
	if ranSetup {
		b.WriteString(setupNote + " It ran before the merge started.\n\n")
	}
	fmt.Fprintf(&b, "## Conflicted files (merging `%s`)\n\n", r.Git.BaseRef())
	if len(conflicts) == 0 {
		b.WriteString("None. The merge only needs to be committed.\n")
	}
	for _, f := range conflicts {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	res, err := r.Agent.Run(ctx, agent.Request{
		Dir:          wt.Dir,
		Instructions: prompt("resolver"),
		Input:        b.String(),
		Schema:       resolverSchema,
	})
	if err != nil {
		return err
	}
	var out struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
	}
	if err := decode(res, &out); err != nil {
		return err
	}

	switch out.Result {
	case "resolved":
		problem, err := r.unfinished(ctx, wt, before, conflicts)
		if err != nil {
			return err
		}
		if problem != "" {
			return toStuck(ctx, r.GitHub, n, label.Review, label.Review, problem+"\n\n"+out.Reason)
		}
		if err := wt.Push(ctx); err != nil {
			return err
		}
		// The Task stays in review, and reviewer looks at it again at its next check, a minute later.
		slog.Info("merged base branch and pushed", "task", n, "conflicts", len(conflicts))
		return nil
	case "stuck":
		// Removing the worktree throws the unfinished merge away.
		return toStuck(ctx, r.GitHub, n, label.Review, label.Review, out.Reason)
	default:
		return fmt.Errorf("unknown resolver result: %q", out.Result)
	}
}

// unfinished returns what is still wrong after the AI reported the merge resolved, or "" if nothing is.
func (r *resolver) unfinished(ctx context.Context, wt *git.Worktree, before string, conflicts []string) (string, error) {
	merging, err := wt.MergeInProgress(ctx)
	if err != nil {
		return "", err
	}
	if merging {
		return "The AI said the conflicts were resolved, but the merge was not committed.", nil
	}
	left, err := wt.ConflictMarkers(conflicts)
	if err != nil {
		return "", err
	}
	if len(left) > 0 {
		return "The AI said the conflicts were resolved, but conflict markers remain in: " + strings.Join(left, ", "), nil
	}
	head, err := wt.Head(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range []struct{ commit, problem string }{
		{before, "The AI rewrote commits that were already on the branch, so they cannot be pushed."},
		{r.Git.BaseRef(), "The AI committed, but the result does not include the base branch."},
	} {
		ok, err := wt.IsAncestor(ctx, c.commit, head)
		if err != nil {
			return "", err
		}
		if !ok {
			return c.problem, nil
		}
	}
	return "", nil
}
