package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
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
	name: "resolver",
	from: label.Review,
	back: label.Review,
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
		conflicted, err := prConflicted(ctx, r.GitHub, t.Number)
		if err != nil {
			return nil, err
		}
		if conflicted {
			out = append(out, t.Number)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// Handle returns an error only for a failed step of exomecha's own (sbx, git, GitHub). The state
// stays as it is, so the next try repeats the step.
func (r *resolver) Handle(ctx context.Context, n int) error {
	name := r.runs.name(resolverRole, n)
	task, err := r.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	run, err := r.runs.read(ctx, name)
	if err != nil {
		return err
	}
	if !holds(task, label.Review) {
		if run.state == none {
			return nil
		}
		return r.runs.drop(ctx, resolverRole, n)
	}
	if run.state == running {
		return nil
	}
	switch run.state {
	case none:
		return r.start(ctx, task, name)
	case preparing:
		// Only a start holds the Task's lock while preparing, so this one was cut off.
		return r.runs.restart(ctx, resolverRole, n, "its start was cut off")
	case stalled:
		return r.runs.failed(ctx, resolverRole, n, fmt.Sprintf("claude's output did not change for %d minutes", int(stallAfter.Minutes())), "")
	case dead:
		return r.runs.failed(ctx, resolverRole, n, "the sandbox stopped while claude was running", "")
	case gone:
		return r.runs.failed(ctx, resolverRole, n, "the sandbox was removed while claude was running", "")
	case finished:
		return r.harvest(ctx, n, run.in)
	}
	return nil
}

func (r *resolver) start(ctx context.Context, task github.Issue, name string) error {
	n := task.Number
	if conflicted, err := prConflicted(ctx, r.GitHub, n); err != nil || !conflicted {
		return err
	}
	if err := r.Git.Sync(ctx); err != nil {
		return err
	}
	branch := branchName(n)
	head, ok, err := r.Git.Rev(ctx, "origin/"+branch)
	if err != nil || !ok {
		return err
	}
	base, ok, err := r.Git.Rev(ctx, r.Git.BaseRef())
	if err != nil || !ok {
		return errors.Join(err, fmt.Errorf("no %s", r.Git.BaseRef()))
	}
	// The branch already has the base branch, and GitHub has not caught up yet. Leave before setup runs for nothing.
	if upToDate, err := r.Git.IsAncestor(ctx, base, head); err != nil || upToDate {
		return err
	}
	if ready, err := r.runs.reserve(ctx, name); err != nil || !ready {
		return err
	}
	defer r.runs.release(name)
	comments, err := issueComments(ctx, r.GitHub, n)
	if err != nil {
		return err
	}
	prd, err := parentPRD(ctx, r.GitHub, n)
	if err != nil {
		return err
	}

	problem, err := r.runs.open(ctx, launch{
		task:    n,
		name:    name,
		branch:  branch,
		in:      inputs{Base: base, Head: head, Remote: head},
		request: agent.Request{Instructions: prompt("resolver"), Schema: resolverSchema},
		// After setup, not before: conflict markers in a manifest or lockfile would break an install.
		input: func(ctx context.Context, ranSetup bool, in *inputs) (string, string, error) {
			conflicts, problem, err := r.merge(ctx, n)
			if problem != "" || err != nil {
				return "", problem, err
			}
			in.Conflicts = conflicts
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
			return b.String(), "", nil
		},
	})
	if problem == "" && err == nil {
		return nil
	}
	// Undo the start even when shutdown cancelled ctx.
	cleanup := context.WithoutCancel(ctx)
	if rerr := r.runs.remove(cleanup, name, false); rerr != nil {
		return errors.Join(err, rerr)
	}
	if problem != "" {
		return toStuck(cleanup, r.GitHub, n, label.Review, label.Review, problem)
	}
	if errors.Is(err, errNoSlot) {
		return nil
	}
	return err
}

// merge merges the base branch in the sandbox's repository but stops before the merge commit, so
// the AI makes that commit itself and sees whatever the project's commit checks report. problem is
// set when git refused the merge without a conflict.
func (r *resolver) merge(ctx context.Context, n int) (conflicts []string, problem string, err error) {
	// The exit status comes back on stdout, apart from sbx's own failures.
	out, err := r.runs.sh(ctx, resolverRole, n, `git merge --no-commit --no-ff "$1" 2>&1; echo "exit $?"`, r.Git.BaseRef())
	if err != nil {
		return nil, "", err
	}
	text := strings.TrimRight(out, "\n")
	output, last := "", text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		output, last = text[:i], text[i+1:]
	}
	code, err := strconv.Atoi(strings.TrimPrefix(last, "exit "))
	if err != nil {
		return nil, "", fmt.Errorf("read the merge's exit status: %q", last)
	}
	if code == 0 {
		return nil, "", nil
	}
	// -z keeps non-ASCII file names as they are instead of quoting them.
	list, err := r.runs.sh(ctx, resolverRole, n, "git diff --name-only -z --diff-filter=U")
	if err != nil {
		return nil, "", err
	}
	for f := range strings.SplitSeq(list, "\x00") {
		if f != "" {
			conflicts = append(conflicts, f)
		}
	}
	if len(conflicts) == 0 {
		// The same merge fails the same way on every try, so only a person can get past it. Like
		// setup's, the output stays in the log.
		slog.Warn("merge failed without a conflict", "task", n, "exit", code, "output", output)
		return nil, fmt.Sprintf("Merging `%s` into this Task's branch in the sandbox failed without a conflict, so the AI did not start. "+
			"git's output is in exomecha's log. Fix what it reports, then relabel this issue as below. You don't need to answer.", r.Git.Base()), nil
	}
	return conflicts, "", nil
}

func (r *resolver) harvest(ctx context.Context, n int, in inputs) error {
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
