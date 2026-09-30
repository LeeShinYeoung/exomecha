package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/label"
)

const reviewerSchema = `{
  "type": "object",
  "properties": {
    "verdict": {"type": "string", "enum": ["approve", "changes"]},
    "body": {"type": "string"}
  },
  "required": ["verdict", "body"],
  "additionalProperties": false
}`

// How long a new head commit waits for CI runs to appear before we assume the project has no CI.
const ciGrace = 5 * time.Minute

type reviewer struct{ Deps }

func (*reviewer) Name() string { return "reviewer" }

// Sweep covers Tasks closed by a merge and Tasks a person closed by hand.
func (r *reviewer) Sweep(ctx context.Context) error {
	prds, err := r.GitHub.ListOpen(ctx, label.PRD, label.Tasked)
	if err != nil {
		return err
	}
	for _, prd := range prds {
		done, err := r.allClosed(ctx, prd.Number)
		if err != nil {
			return err
		}
		if !done {
			continue
		}
		if err := r.GitHub.Close(ctx, prd.Number, github.Completed); err != nil {
			return err
		}
		slog.Info("closed PRD", "prd", prd.Number)

		problem, ok, err := r.GitHub.Parent(ctx, prd.Number)
		if err != nil {
			return err
		}
		if !ok || !problem.Open {
			continue
		}
		done, err = r.allClosed(ctx, problem.Number)
		if err != nil {
			return err
		}
		if !done {
			continue
		}
		if err := r.GitHub.Close(ctx, problem.Number, github.Completed); err != nil {
			return err
		}
		slog.Info("closed problem", "problem", problem.Number)
	}
	return nil
}

func (r *reviewer) allClosed(ctx context.Context, n int) (bool, error) {
	subs, err := r.GitHub.SubIssues(ctx, n)
	if err != nil || len(subs) == 0 {
		return false, err
	}
	for _, s := range subs {
		if s.Open {
			return false, nil
		}
	}
	return true, nil
}

func (r *reviewer) Pick(ctx context.Context) ([]int, error) {
	tasks, err := r.GitHub.ListOpen(ctx, label.Task, string(label.Review))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, t := range tasks {
		// Leaving conflicting PRs to resolver also keeps the two from grabbing the same Task
		// at the same moment every minute, which starved resolver.
		conflicted, err := prConflicted(ctx, r.GitHub, t.Number)
		if err != nil {
			return nil, err
		}
		if !conflicted {
			out = append(out, t.Number)
		}
	}
	return out, nil
}

func (r *reviewer) Handle(ctx context.Context, n int) error {
	task, ok, err := reload(ctx, r.GitHub, n, label.Task, string(label.Review))
	if err != nil || !ok {
		return err
	}
	branch := branchName(n)
	number, ok, err := r.GitHub.OpenPR(ctx, branch)
	if err != nil {
		return err
	}
	if !ok {
		// Retrying never brings a closed PR back, and coworker never closes one itself, so a person
		// closed it. Only they know whether the work is still wanted.
		return toStuck(ctx, r.GitHub, n, label.Review, label.Pending,
			fmt.Sprintf("The PR on branch %s was closed without being merged.", branch))
	}
	pr, err := r.GitHub.PR(ctx, number)
	if err != nil {
		return err
	}
	// If GitHub is still computing mergeability, try again at the next check, a minute later. If the PR conflicts, resolver handles it.
	if pr.Mergeable == nil || !*pr.Mergeable {
		return nil
	}

	ci, failed, err := r.GitHub.CheckCI(ctx, pr.HeadSHA)
	if err != nil {
		return err
	}
	switch ci {
	case github.CIRunning:
		return nil
	case github.CIFailed:
		var b strings.Builder
		b.WriteString("CI failed.\n\n")
		for _, f := range failed {
			fmt.Fprintf(&b, "- %s: %s %s\n", f.Name, f.Conclusion, f.URL)
		}
		if err := r.GitHub.AddReview(ctx, number, b.String()); err != nil {
			return err
		}
		if err := r.GitHub.SetStatus(ctx, n, label.Review, label.Queued); err != nil {
			return err
		}
		slog.Info("requeued after CI failure", "task", n, "pr", number)
		return nil
	case github.CINone:
		at, err := r.GitHub.CommitTime(ctx, pr.HeadSHA)
		if err != nil {
			return err
		}
		if time.Since(at) < ciGrace {
			return nil
		}
		// Still no runs after the grace period: treat it as a project without CI.
	}

	wt, err := r.Git.OpenBranch(ctx, branch)
	if err != nil {
		return err
	}
	defer wt.Remove()
	comments, err := issueComments(ctx, r.GitHub, n)
	if err != nil {
		return err
	}
	prd, err := parentPRD(ctx, r.GitHub, n)
	if err != nil {
		return err
	}
	base := r.Git.BaseRef()
	input := taskText(task) + comments + prd +
		fmt.Sprintf("## Base branch\n\n`%s`. See this PR's changes with `git diff %s...HEAD`.\n", base, base)
	res, err := r.Agent.Run(ctx, agent.Request{
		Dir:          wt.Dir,
		Instructions: prompt("reviewer"),
		Input:        input,
		Schema:       reviewerSchema,
	})
	if err != nil {
		return err
	}
	var out struct {
		Verdict string `json:"verdict"`
		Body    string `json:"body"`
	}
	if err := decode(res, &out); err != nil {
		return err
	}

	switch out.Verdict {
	case "changes":
		if err := r.GitHub.AddReview(ctx, number, out.Body); err != nil {
			return err
		}
		if err := r.GitHub.SetStatus(ctx, n, label.Review, label.Queued); err != nil {
			return err
		}
		slog.Info("requeued with review comments", "task", n, "pr", number)
	case "approve":
		// A failed merge (branch protection etc.) is an environment error: the Task stays in review.
		if err := r.GitHub.Merge(ctx, number, pr.HeadSHA); err != nil {
			return err
		}
		if err := r.GitHub.ClearStatus(ctx, n); err != nil {
			return err
		}
		if err := r.GitHub.Close(ctx, n, github.Completed); err != nil {
			return err
		}
		slog.Info("merged and closed task", "task", n, "pr", number)
	default:
		return errors.New("unknown reviewer verdict: " + out.Verdict)
	}
	return nil
}
