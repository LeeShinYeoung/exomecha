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

const resolverSchema = `{
  "type": "object",
  "properties": {
    "result": {"type": "string", "enum": ["resolved", "stuck"]},
    "reason": {"type": "string"}
  },
  "required": ["result", "reason"],
  "additionalProperties": false
}`

var resolverRole = role{
	name:    "resolver",
	schema:  resolverSchema,
	project: true,
	from:    label.Review,
	back:    label.Review,
}

var resolverTable = table{
	// none, preparing, running, stalled, dead, gone, finished
	away:  {idle, drop, drop, drop, drop, drop, drop},
	clean: {idle, restart, idle, fail, fail, fail, harvest},
	ready: {begin, restart, idle, fail, fail, fail, harvest},
}

// resolver works like builder, one sandbox per Task, while the Task stays in review.
type resolver struct {
	Deps
	runs *runs
}

func (*resolver) Name() string { return "resolver" }

// Pick returns the Tasks in review whose PR conflicts, and every Task that has a resolver sandbox.
func (r *resolver) Pick(ctx context.Context) ([]int, error) {
	tasks, err := r.GitHub.ListOpen(ctx, label.Task, string(label.Review))
	if err != nil {
		return nil, err
	}
	found, err := r.runs.found(ctx, resolverRole)
	if err != nil {
		return nil, err
	}
	out := found
	for _, t := range tasks {
		p, err := r.place(ctx, t)
		if err != nil {
			return nil, err
		}
		if p == ready {
			out = append(out, t.Number)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func (r *resolver) Handle(ctx context.Context, n int) error {
	task, err := r.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	p, err := r.place(ctx, task)
	if err != nil {
		return err
	}
	return r.runs.step(ctx, resolverRole, n, resolverTable[p],
		func(ctx context.Context) error { return r.start(ctx, task) },
		func(ctx context.Context, run run) error { return r.collect(ctx, n, run.in) })
}

func (r *resolver) place(ctx context.Context, task github.Issue) (place, error) {
	if !holds(task, label.Task, label.Review) {
		return away, nil
	}
	pr, ok, err := taskPR(ctx, r.GitHub, task.Number)
	if err != nil {
		return away, err
	}
	if ok && pr.Mergeable != nil && !*pr.Mergeable {
		return ready, nil
	}
	return clean, nil
}

func (r *resolver) start(ctx context.Context, task github.Issue) error {
	n := task.Number
	base, err := baseCommit(ctx, r.Git)
	if err != nil {
		return err
	}
	branch := branchName(n)
	head, ok, err := r.Git.Rev(ctx, "origin/"+branch)
	if err != nil || !ok {
		return err
	}
	// The branch already has the base branch, and GitHub has not caught up yet. Leave before setup runs for nothing.
	if upToDate, err := r.Git.IsAncestor(ctx, base, head); err != nil || upToDate {
		return err
	}
	return r.runs.start(ctx, resolverRole, n, func(ctx context.Context) (spec, error) {
		comments, err := issueComments(ctx, r.GitHub, n)
		if err != nil {
			return spec{}, err
		}
		prd, err := parentPRD(ctx, r.GitHub, n)
		if err != nil {
			return spec{}, err
		}
		return spec{branch: branch, base: base, head: head, remote: head, merge: true, input: taskText(task) + comments + prd}, nil
	})
}

func (r *resolver) collect(ctx context.Context, n int, in inputs) error {
	var out struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
	}
	if ok, err := r.runs.result(ctx, resolverRole, n, &out); err != nil || !ok {
		return err
	}
	switch out.Result {
	case "stuck":
		// Removing the sandbox throws the unfinished merge away.
		return r.runs.stuck(ctx, resolverRole, n, out.Reason)
	case "resolved":
	default:
		return r.runs.failed(ctx, resolverRole, n, badResult, fmt.Sprintf("result %q", out.Result))
	}

	head, ok, err := r.runs.fetch(ctx, resolverRole, n)
	if err != nil {
		return err
	}
	if !ok {
		return r.runs.stuck(ctx, resolverRole, n, noCommit+"\n\n"+out.Reason)
	}
	moved, err := r.runs.moved(ctx, n, in, head)
	if err != nil {
		return err
	}
	if moved {
		return r.runs.restart(ctx, resolverRole, n, "the Task branch changed on GitHub during the run")
	}
	problem, err := r.unfinished(ctx, n, head, in)
	if err != nil {
		return err
	}
	if problem != "" {
		return r.runs.stuck(ctx, resolverRole, n, problem+"\n\n"+out.Reason)
	}
	if err := r.Git.PushCommit(ctx, head, branchName(n), false); err != nil {
		return err
	}
	// The Task stays in review, and reviewer looks at it again at its next check, a minute later.
	slog.Info("merged base branch and pushed", "task", n, "conflicts", len(in.Conflicts))
	return r.runs.done(ctx, resolverRole, n)
}

// unfinished returns what is still wrong after the AI reported the merge resolved, or "" if nothing is.
func (r *resolver) unfinished(ctx context.Context, n int, head string, in inputs) (string, error) {
	// rev-parse exits 1 when there is no merge in progress, which sh would report as an error.
	merging, err := r.runs.sh(ctx, resolverRole, n, "git rev-parse -q --verify MERGE_HEAD || true")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(merging) != "" {
		return "The AI said the conflicts were resolved, but the merge was not committed.", nil
	}
	left, err := r.Git.ConflictMarkers(ctx, head, in.Conflicts)
	if err != nil {
		return "", err
	}
	if len(left) > 0 {
		return "The AI said the conflicts were resolved, but conflict markers remain in: " + strings.Join(left, ", "), nil
	}
	for _, c := range []struct{ commit, problem string }{
		{in.Head, "The AI rewrote commits that were already on the branch, so they cannot be pushed."},
		{in.Base, "The AI committed, but the result does not include the base branch."},
	} {
		ok, err := r.Git.IsAncestor(ctx, c.commit, head)
		if err != nil {
			return "", err
		}
		if !ok {
			return c.problem, nil
		}
	}
	return "", nil
}
