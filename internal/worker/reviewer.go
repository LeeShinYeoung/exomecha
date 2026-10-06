package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
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

var reviewerRole = role{
	name:   "reviewer",
	schema: reviewerSchema,
	from:   label.Review,
	back:   label.Review,
	tier:   label.Heavy,
}

// merged, closed and failing drop the run, which was about a PR that no longer stands, and Handle
// then settles the PRD.
var reviewerTable = table{
	// none, preparing, running, stalled, dead, gone, finished
	away:    {idle, drop, drop, drop, drop, drop, drop},
	merged:  {idle, drop, drop, drop, drop, drop, drop},
	closed:  {idle, drop, drop, drop, drop, drop, drop},
	failing: {idle, drop, drop, drop, drop, drop, drop},
	waiting: {idle, restart, idle, fail, fail, fail, idle},
	ready:   {begin, restart, idle, fail, fail, fail, harvest},
}

// reviewer reviews each PRD's PR in a sandbox of its own, which gets only the code.
type reviewer struct {
	Deps
	runs *runs
}

func (*reviewer) Name() string { return reviewerRole.name }

// Pick returns the PRDs in review whose PR does not conflict, and every PRD that has a reviewer run.
func (r *reviewer) Pick(ctx context.Context) ([]int, error) {
	prds, err := r.GitHub.ListOpen(ctx, label.PRD, string(label.Review))
	if err != nil {
		return nil, err
	}
	var picked []int
	for _, t := range prds {
		// Leaving conflicting PRs to resolver also keeps the two from grabbing the same PRD
		// at the same moment every minute, which starved resolver.
		pr, ok, err := prdPR(ctx, r.GitHub, t.Number)
		if err != nil {
			return nil, err
		}
		if !ok || pr.Mergeable == nil || *pr.Mergeable {
			picked = append(picked, t.Number)
		}
	}
	return r.runs.pick(ctx, reviewerRole, picked)
}

func (r *reviewer) Handle(ctx context.Context, n int) error {
	prd, err := r.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	p, pr, failed, err := r.look(ctx, prd)
	if err != nil {
		return err
	}
	err = r.runs.step(ctx, reviewerRole, n, reviewerTable[p],
		func(ctx context.Context) error { return r.start(ctx, prd, pr) },
		func(ctx context.Context, run run) error { return r.collect(ctx, n, pr, run.in) })
	if err != nil {
		return err
	}
	switch p {
	case merged:
		if err := r.finish(ctx, n); err != nil {
			return err
		}
		slog.Info("closed a PRD whose PR was merged", "prd", n)
	case closed:
		// Retrying never brings a closed PR back, and exomecha never closes one itself, so a
		// person closed it. Only they know whether the work is still wanted.
		return toStuck(ctx, r.GitHub, n, label.Review, label.Ready,
			fmt.Sprintf("The PR on branch %s was closed without being merged.", branchName(n)))
	case failing:
		var b strings.Builder
		b.WriteString("CI failed.\n\n")
		for _, f := range failed {
			fmt.Fprintf(&b, "- %s: %s %s\n", f.Name, f.Conclusion, f.URL)
		}
		if err := r.requeue(ctx, n, pr.Number, b.String()); err != nil {
			return err
		}
		slog.Info("requeued after CI failure", "prd", n, "pr", pr.Number)
	}
	return nil
}

// look finds the PRD's place, with its open PR, and the CI runs that failed when the place is failing.
func (r *reviewer) look(ctx context.Context, prd github.Issue) (place, github.PR, []github.Run, error) {
	if !holds(prd, label.PRD, label.Review) {
		return away, github.PR{}, nil, nil
	}
	pr, ok, err := prdPR(ctx, r.GitHub, prd.Number)
	switch {
	case err != nil:
		return away, pr, nil, err
	case !ok:
		// A PR can be merged with its PRD still in review, since exomecha closes the PRD after
		// the merge and that can fail.
		done, err := r.GitHub.Merged(ctx, branchName(prd.Number))
		switch {
		case err != nil:
			return away, pr, nil, err
		case done:
			return merged, pr, nil, nil
		}
		return closed, pr, nil, nil
	// GitHub is still computing whether the PR merges.
	case pr.Mergeable == nil:
		return waiting, pr, nil, nil
	// A conflict is resolver's.
	case !*pr.Mergeable:
		return away, pr, nil, nil
	}
	ci, failed, err := r.GitHub.CheckCI(ctx, pr.HeadSHA)
	if err != nil {
		return away, pr, nil, err
	}
	switch ci {
	case github.CIRunning:
		return waiting, pr, nil, nil
	case github.CIFailed:
		return failing, pr, failed, nil
	case github.CINone:
		at, err := r.GitHub.CommitTime(ctx, pr.HeadSHA)
		if err != nil {
			return away, pr, nil, err
		}
		if time.Since(at) < ciGrace {
			return waiting, pr, nil, nil
		}
		// Still no runs after the grace period: treat it as a project without CI.
	}
	return ready, pr, nil, nil
}

func (r *reviewer) start(ctx context.Context, prd github.Issue, pr github.PR) error {
	n := prd.Number
	return r.runs.start(ctx, reviewerRole, n, func(ctx context.Context) (spec, error) {
		base, err := baseCommit(ctx, r.Git)
		if err != nil {
			return spec{}, err
		}
		comments, err := issueComments(ctx, r.GitHub, n)
		if err != nil {
			return spec{}, err
		}
		ref := r.Git.BaseRef()
		input := prdText(prd) + comments +
			fmt.Sprintf("## Base branch\n\n`%s`. See this PR's changes with `git diff %s...HEAD`.\n", ref, ref)
		return spec{branch: branchName(n), base: base, head: pr.HeadSHA, remote: pr.HeadSHA, input: input}, nil
	})
}

func (r *reviewer) collect(ctx context.Context, n int, pr github.PR, in inputs) error {
	var out struct {
		Verdict string `json:"verdict"`
		Body    string `json:"body"`
	}
	if ok, err := r.runs.result(ctx, reviewerRole, n, &out); err != nil || !ok {
		return err
	}
	if in.moved(pr.HeadSHA, "") {
		return r.runs.restart(ctx, reviewerRole, n, "the PR branch changed during the review")
	}
	switch out.Verdict {
	case "changes":
		if err := r.requeue(ctx, n, pr.Number, out.Body); err != nil {
			return err
		}
		slog.Info("requeued with review comments", "prd", n, "pr", pr.Number)
	case "approve":
		// With the head the run reviewed, GitHub refuses the merge if anything was pushed since.
		err := r.GitHub.Merge(ctx, pr.Number, in.Head)
		var refused *github.MergeRefused
		switch {
		case errors.As(err, &refused) && refused.Status == http.StatusConflict:
			return r.runs.restart(ctx, reviewerRole, n, "the PR branch changed before the merge")
		case errors.As(err, &refused):
			return r.refused(ctx, n, pr.Number, refused.Message)
		case err != nil:
			// The result stays, so the next try merges without another review.
			return err
		}
		if err := r.finish(ctx, n); err != nil {
			return err
		}
		slog.Info("merged and closed PRD", "prd", n, "pr", pr.Number)
	default:
		return r.runs.failed(ctx, reviewerRole, n, badResult, fmt.Sprintf("verdict %q", out.Verdict))
	}
	return r.runs.done(ctx, reviewerRole, n)
}

// refused handles a merge GitHub refused for what the PR is. GitHub says the same for a conflict
// and for a rule of the base branch, so the PR tells them apart.
func (r *reviewer) refused(ctx context.Context, n, number int, message string) error {
	pr, err := r.GitHub.PR(ctx, number)
	if err != nil {
		return err
	}
	switch {
	case pr.Mergeable == nil:
		// The result stays until GitHub knows which it is.
		return fmt.Errorf("GitHub refused to merge PR #%d before it knew whether the PR conflicts: %s", number, message)
	case !*pr.Mergeable:
		// The next round finds the PR conflicted and drops the run, and resolver takes the PRD.
		return nil
	}
	return r.runs.stuck(ctx, reviewerRole, n, fmt.Sprintf("The AI approved PR #%d, but GitHub refused to merge it: %s\n\n"+
		"A rule of the base branch may need something exomecha cannot give, such as an approving review from someone else. "+
		"Merge the PR yourself, or change the rule and relabel this issue as below.", number, message))
}

// finish closes the PRD as completed and then clears its status. A PRD left open in review
// comes back to merged, and one closed with its status is left alone by every worker.
func (r *reviewer) finish(ctx context.Context, n int) error {
	if err := r.GitHub.Close(ctx, n, github.Completed); err != nil {
		return err
	}
	return r.GitHub.ClearStatus(ctx, n)
}

// requeue leaves a review on the PR and sends the PRD back to builder.
func (r *reviewer) requeue(ctx context.Context, n, pr int, review string) error {
	if err := r.GitHub.AddReview(ctx, pr, review); err != nil {
		return err
	}
	return r.GitHub.SetStatus(ctx, n, label.Review, label.Ready)
}
