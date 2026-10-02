package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/github"
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

const resolverRole = "resolver"

// resolver works like builder, one sandbox per Task, while the Task stays in review.
type resolver struct {
	Deps
	box *taskbox
}

func (*resolver) Name() string { return "resolver" }

// Pick returns the Tasks in review whose PR conflicts, and every Task that has a resolver sandbox.
func (r *resolver) Pick(ctx context.Context) ([]int, error) {
	tasks, err := r.GitHub.ListOpen(ctx, label.Task, string(label.Review))
	if err != nil {
		return nil, err
	}
	found, err := r.box.found(ctx, resolverRole)
	if err != nil {
		return nil, err
	}
	mine := map[int]bool{}
	out := found
	for _, t := range tasks {
		mine[t.Number] = true
		conflicted, err := prConflicted(ctx, r.GitHub, t.Number)
		if err != nil {
			return nil, err
		}
		if conflicted {
			out = append(out, t.Number)
		}
	}
	r.box.prune(resolverRole, mine)
	slices.Sort(out)
	return slices.Compact(out), nil
}

// Handle returns an error only for a failed step of coworker's own (sbx, git, GitHub). The state
// stays as it is and the next try waits longer each time.
func (r *resolver) Handle(ctx context.Context, n int) error {
	name := r.box.name(n, resolverRole)
	err := r.handle(ctx, n, name)
	if err != nil && ctx.Err() == nil {
		r.box.strike(name)
	}
	return err
}

func (r *resolver) handle(ctx context.Context, n int, name string) error {
	task, err := r.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	run, err := r.box.read(ctx, name)
	if err != nil {
		return err
	}
	if !holds(task, label.Review) {
		r.box.forget(name)
		if run.state == none {
			return nil
		}
		slog.Info("removing the sandbox of a Task that left review", "task", n, "state", run.state)
		return r.box.remove(ctx, name, false)
	}
	if run.state == running || !r.box.due(name) {
		return nil
	}
	switch run.state {
	case none:
		return r.start(ctx, task, name)
	case preparing:
		// Only a start holds the Task's lock while preparing, so this one was cut off.
		return r.box.remove(ctx, name, false)
	case stalled:
		return r.failed(ctx, n, name, fmt.Sprintf("claude's output did not change for %d minutes", int(stallAfter.Minutes())), "")
	case dead:
		return r.failed(ctx, n, name, "the sandbox stopped while claude was running", "")
	case gone:
		return r.failed(ctx, n, name, "the sandbox was removed while claude was running", "")
	case finished:
		return r.harvest(ctx, name, run.start, n)
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
	if ready, err := r.box.ready(ctx, name); err != nil || !ready {
		return err
	}
	defer r.box.release(name)
	comments, err := issueComments(ctx, r.GitHub, n)
	if err != nil {
		return err
	}
	prd, err := parentPRD(ctx, r.GitHub, n)
	if err != nil {
		return err
	}

	problem, err := r.box.open(ctx, launch{
		task:    n,
		name:    name,
		branch:  branch,
		start:   started{Before: head, Remote: head, Base: base},
		request: agent.Request{Instructions: prompt("resolver"), Schema: resolverSchema},
		// After setup, not before: conflict markers in a manifest or lockfile would break an install.
		input: func(ctx context.Context, ranSetup bool, s *started) (string, string, error) {
			conflicts, problem, err := r.merge(ctx, name)
			if problem != "" || err != nil {
				return "", problem, err
			}
			s.Conflicts = conflicts
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
	if rerr := r.box.remove(cleanup, name, false); rerr != nil {
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
func (r *resolver) merge(ctx context.Context, name string) (conflicts []string, problem string, err error) {
	// The exit status comes back on stdout, apart from sbx's own failures.
	out, err := r.Sandboxes.Exec(ctx, name, repoDir, nil, "sh", "-c", `git merge --no-commit --no-ff "$1" 2>&1; echo "exit $?"`, "sh", r.Git.BaseRef())
	if err != nil {
		return nil, "", err
	}
	text := strings.TrimRight(string(out), "\n")
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
	list, err := r.box.git(ctx, name, repoDir, "diff", "--name-only", "-z", "--diff-filter=U")
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
		slog.Warn("merge failed without a conflict", "sandbox", name, "exit", code, "output", output)
		return nil, fmt.Sprintf("Merging `%s` into this Task's branch in the sandbox failed without a conflict, so the AI did not start. "+
			"git's output is in coworker's log. Fix what it reports, then relabel this issue as below. You don't need to answer.", r.Git.Base()), nil
	}
	return conflicts, "", nil
}

func (r *resolver) harvest(ctx context.Context, name string, s started, n int) error {
	branch := branchName(n)
	res, failure, detail, limited := r.box.outcome(name, true)
	if limited {
		slog.Warn("run ended at the Claude usage limit, starting over later", "task", n, "sandbox", name)
		r.box.strike(name)
		return r.box.remove(ctx, name, false)
	}
	if failure != "" {
		return r.failed(ctx, n, name, failure, detail)
	}
	var out struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
	}
	if err := decode(res, &out); err != nil {
		return r.failed(ctx, n, name, "the AI's result was not in the expected form", err.Error())
	}
	switch out.Result {
	case "stuck":
		// Removing the sandbox throws the unfinished merge away.
		return r.stuck(ctx, n, name, out.Reason)
	case "resolved":
	default:
		return r.failed(ctx, n, name, "the AI's result was not in the expected form", fmt.Sprintf("result %q", out.Result))
	}

	if committed, err := r.box.committed(ctx, name); err != nil {
		return err
	} else if !committed {
		return r.stuck(ctx, n, name, noCommit+"\n\n"+out.Reason)
	}
	head, err := r.box.fetch(ctx, name)
	if err != nil {
		return err
	}
	if err := r.Git.Sync(ctx); err != nil {
		return err
	}
	// head is there when coworker already pushed this run and a later step failed.
	if remote, _, err := r.Git.Rev(ctx, "origin/"+branch); err != nil {
		return err
	} else if remote != s.Remote && remote != head {
		slog.Info("Task branch changed on GitHub during the run, starting over", "task", n, "branch", branch)
		return r.box.remove(ctx, name, false)
	}
	problem, err := r.unfinished(ctx, name, head, s)
	if err != nil {
		return err
	}
	if problem != "" {
		return r.stuck(ctx, n, name, problem+"\n\n"+out.Reason)
	}
	if err := r.Git.PushCommit(ctx, head, branch, false); err != nil {
		return err
	}
	// The Task stays in review, and reviewer looks at it again at its next check, a minute later.
	slog.Info("merged base branch and pushed", "task", n, "conflicts", len(s.Conflicts))
	r.box.forget(name)
	return r.box.remove(ctx, name, false)
}

// unfinished returns what is still wrong after the AI reported the merge resolved, or "" if nothing is.
func (r *resolver) unfinished(ctx context.Context, name, head string, s started) (string, error) {
	// rev-parse exits 1 when there is no merge in progress, which Exec would report as an error.
	merging, err := r.Sandboxes.Exec(ctx, name, repoDir, nil, "sh", "-c", "git rev-parse -q --verify MERGE_HEAD || true")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(merging)) != "" {
		return "The AI said the conflicts were resolved, but the merge was not committed.", nil
	}
	left, err := r.Git.ConflictMarkers(ctx, head, s.Conflicts)
	if err != nil {
		return "", err
	}
	if len(left) > 0 {
		return "The AI said the conflicts were resolved, but conflict markers remain in: " + strings.Join(left, ", "), nil
	}
	for _, c := range []struct{ commit, problem string }{
		{s.Before, "The AI rewrote commits that were already on the branch, so they cannot be pushed."},
		{s.Base, "The AI committed, but the result does not include the base branch."},
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

func (r *resolver) stuck(ctx context.Context, n int, name, reason string) error {
	if err := toStuck(ctx, r.GitHub, n, label.Review, label.Review, reason); err != nil {
		return err
	}
	r.box.forget(name)
	return r.box.remove(ctx, name, false)
}

// failed removes a run that failed, or after maxFailures in a row sends the Task to stuck. The
// Task stays in review, so resolver starts over at a later round.
func (r *resolver) failed(ctx context.Context, n int, name, why, detail string) error {
	slog.Warn("resolver run failed", "task", n, "sandbox", name, "why", why, "detail", detail, "stderr", r.box.tail(name, "stderr.log"))
	// Stuck goes first. With the sandbox gone and the Task still in review, a failed GitHub call
	// would start a fourth run.
	if count := r.box.failures(name) + 1; count >= maxFailures {
		if err := toStuck(ctx, r.GitHub, n, label.Review, label.Review, failedReason(count, why)); err != nil {
			return err
		}
		r.box.forget(name)
		return r.box.remove(ctx, name, true)
	}
	if err := r.box.remove(ctx, name, true); err != nil {
		return err
	}
	r.box.fail(name)
	return nil
}
