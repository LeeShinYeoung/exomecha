package worker

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const reviewerSchema = `{
  "type": "object",
  "properties": {
    "verdict": {"type": "string", "enum": ["approve", "changes"]},
    "body": {"type": "string"},
    "details": {"type": "string"}
  },
  "required": ["verdict", "body", "details"],
  "additionalProperties": false
}`

// How long a new head commit waits for CI runs to appear before we assume the project has no CI.
const ciGrace = 5 * time.Minute

var reviewerRole = role{
	name:   "reviewer",
	schema: reviewerSchema,
	from:   label.Review,
	back:   label.Review,
	model:  func(m Models) agent.Model { return m.Reviewer },
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

// Pick returns the PRDs in review whose PRs do not conflict, the approved PRDs with no PR left open
// or a PR closed without a merge, and every PRD that has a reviewer run. A PRD whose Repositories
// section is wrong is picked too, so Handle sends it to stuck.
func (r *reviewer) Pick(ctx context.Context) ([]int, error) {
	prds, err := r.GitHub.ListOpen(ctx, label.PRD, string(label.Review))
	if err != nil {
		return nil, err
	}
	approved, err := r.GitHub.ListOpen(ctx, label.PRD, string(label.Approved))
	if err != nil {
		return nil, err
	}
	var picked []int
	for _, t := range approved {
		repos, why := r.prdRepos(t)
		if why != "" {
			picked = append(picked, t.Number)
			continue
		}
		// The same rule Handle follows, so a PRD that waits for a person is not picked every round.
		p, _, _, err := r.look(ctx, t, repos)
		if err != nil {
			return nil, err
		}
		if p != away {
			picked = append(picked, t.Number)
		}
	}
	for _, t := range prds {
		repos, why := r.prdRepos(t)
		if why != "" {
			picked = append(picked, t.Number)
			continue
		}
		// Leaving conflicting PRs to resolver also keeps the two from grabbing the same PRD
		// at the same moment every minute, which starved resolver.
		prs, err := prdPRs(ctx, repos, t.Number)
		if err != nil {
			return nil, err
		}
		if !conflicting(prs) {
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
	repos, why := r.prdRepos(prd)
	if why != "" && (holds(prd, label.Review) || holds(prd, label.Approved)) {
		return r.runs.invalid(ctx, reviewerRole, prd, why)
	}
	p, prs, failed, err := r.look(ctx, prd, repos)
	if err != nil {
		return err
	}
	err = r.runs.step(ctx, reviewerRole, n, reviewerTable[p],
		func(ctx context.Context) error { return r.start(ctx, prd, prs) },
		func(ctx context.Context, got run) error { return r.collect(ctx, n, prs, got.in) })
	if err != nil {
		return err
	}
	switch p {
	case merged:
		if err := r.finish(ctx, n); err != nil {
			return err
		}
	case closed:
		// Retrying never brings a closed PR back, and exomecha never closes one itself, so a
		// person closed it. Only they know whether the work is still wanted.
		return toStuck(ctx, r.GitHub, n, reviewerRole.name, prd.Status(), label.Ready, r.closedReason(n, prs), "", "")
	case failing:
		var b strings.Builder
		b.WriteString("CI failed.\n\n")
		for _, f := range failed {
			fmt.Fprintf(&b, "- %s: %s %s\n", f.Name, f.Conclusion, f.URL)
		}
		if err := r.requeue(ctx, n, openPRs(prs), github.KindCI, b.String()); err != nil {
			return err
		}
	}
	return nil
}

// closedReason says which of the PRD's PRs was closed without being merged.
func (r *reviewer) closedReason(n int, prs []repoPR) string {
	for _, p := range prs {
		if p.open.Number == 0 && p.closed != 0 && !p.closedMerged {
			return fmt.Sprintf("PR %s on branch %s was closed without being merged.", r.issueRef(r.PRDRepo, p.repo.Name, p.closed), branchName(n))
		}
	}
	return fmt.Sprintf("This PRD has no PR on branch %s, open or merged.", branchName(n))
}

// look finds the PRD's place, with its PRs, and the CI runs that failed when the place is failing.
// Each PR has a place of its own, and the PRD's is the one that comes first: a PR closed without a
// merge, then one a person or resolver has to act on, then one GitHub or CI has not checked yet.
func (r *reviewer) look(ctx context.Context, prd github.Issue, repos []*Repo) (place, []repoPR, []github.Run, error) {
	approved := holds(prd, label.Approved)
	if !approved && !holds(prd, label.Review) {
		return away, nil, nil, nil
	}
	prs, err := prdPRs(ctx, repos, prd.Number)
	if err != nil {
		return away, nil, nil, err
	}
	open := openPRs(prs)
	thrownAway, done := false, false
	for _, p := range prs {
		if p.open.Number == 0 && p.closed != 0 {
			thrownAway = thrownAway || !p.closedMerged
			done = done || p.closedMerged
		}
	}
	switch {
	case thrownAway:
		return closed, prs, nil, nil
	case len(open) == 0 && done:
		// GitHub closes the PRD on a merge only when its only PR targets the default branch, so
		// exomecha closes it otherwise.
		return merged, prs, nil, nil
	case len(open) == 0:
		return closed, prs, nil, nil
	// The PRs wait for a person to merge them. A conflict is resolver's.
	case approved:
		return away, prs, nil, nil
	case conflicting(prs):
		return away, prs, nil, nil
	}
	var failed []github.Run
	wait := false
	for _, p := range open {
		// GitHub is still computing whether the PR merges.
		if p.open.Mergeable == nil {
			return waiting, prs, nil, nil
		}
		ci, ciRuns, err := p.repo.GitHub.CheckCI(ctx, p.open.HeadSHA)
		if err != nil {
			return away, prs, nil, err
		}
		switch ci {
		case github.CIRunning:
			wait = true
		case github.CIFailed:
			for _, ciRun := range ciRuns {
				if len(open) > 1 {
					ciRun.Name = p.repo.Name + ": " + ciRun.Name
				}
				failed = append(failed, ciRun)
			}
		case github.CINone:
			at, err := p.repo.GitHub.CommitTime(ctx, p.open.HeadSHA)
			if err != nil {
				return away, prs, nil, err
			}
			// Still no runs after the grace period: treat it as a project without CI.
			wait = wait || time.Since(at) < cmp.Or(r.CIGrace, ciGrace)
		}
	}
	switch {
	case wait:
		return waiting, prs, nil, nil
	case len(failed) > 0:
		return failing, prs, failed, nil
	}
	return ready, prs, nil, nil
}

func (r *reviewer) start(ctx context.Context, prd github.Issue, prs []repoPR) error {
	n := prd.Number
	return r.runs.start(ctx, reviewerRole, n, func(ctx context.Context) (spec, error) {
		s := spec{tier: label.TierOf(prd.Labels)}
		var shown []repoWithPR
		for _, p := range prs {
			base, err := fetchBase(ctx, p.repo.Git)
			if err != nil {
				return spec{}, err
			}
			rs := repoSpec{repo: p.repo, base: base, head: base}
			if p.open.Number != 0 {
				rs.branch, rs.head, rs.remote = branchName(n), p.open.HeadSHA, p.open.HeadSHA
			}
			s.repos = append(s.repos, rs)
			shown = append(shown, repoWithPR{repo: p.repo, pr: p.open.Number})
		}
		input, err := sharedInput(ctx, r.Deps, prd, shown)
		if err != nil {
			return spec{}, err
		}
		s.input = input
		return s, nil
	})
}

func (r *reviewer) collect(ctx context.Context, n int, prs []repoPR, in inputs) error {
	var out struct {
		Verdict string `json:"verdict"`
		Body    string `json:"body"`
		Details string `json:"details"`
	}
	if ok, err := r.runs.result(ctx, reviewerRole, n, &out); err != nil || !ok {
		return err
	}
	body := github.Fold(out.Body, "Details", out.Details)
	open := openPRs(prs)
	for _, rr := range in.Repos {
		now := ""
		for _, p := range open {
			if p.repo.Name == rr.Repo {
				now = p.open.HeadSHA
			}
		}
		if rr.moved(now, "") {
			return r.runs.restart(ctx, reviewerRole, n)
		}
	}
	switch out.Verdict {
	case "changes":
		if err := r.requeue(ctx, n, open, github.KindChanges, body); err != nil {
			return err
		}
	case "approve":
		note := github.Note{Worker: reviewerRole.name, Kind: github.KindApproved}
		for _, p := range open {
			already, err := posted(ctx, p.repo.GitHub, p.open.Number, note, body)
			if err != nil {
				return err
			}
			if already {
				continue
			}
			if err := p.repo.GitHub.AddApproval(ctx, p.open.Number, body); err != nil {
				return err
			}
		}
		if err := r.GitHub.SetStatus(ctx, n, label.Review, label.Approved); err != nil {
			return err
		}
	default:
		return r.runs.failed(ctx, reviewerRole, n, badResult)
	}
	return r.runs.drop(ctx, reviewerRole, n)
}

// finish closes the PRD as completed and then clears its status. A PRD left open comes back to
// merged, and one closed with its status is left alone by every worker.
func (r *reviewer) finish(ctx context.Context, n int) error {
	if err := r.GitHub.Close(ctx, n); err != nil {
		return err
	}
	return r.GitHub.ClearStatus(ctx, n)
}

// requeue leaves a review on each open PR and sends the PRD back to builder, or to a person once
// reviewer has sent it back ReviewLimit times since a person last stepped in.
func (r *reviewer) requeue(ctx context.Context, n int, open []repoPR, kind, review string) error {
	comments, err := r.GitHub.Comments(ctx, n)
	if err != nil {
		return err
	}
	note := github.Note{Worker: reviewerRole.name, Kind: kind}
	// A PR opened later counts fewer send-backs, so the PRD's count is the largest.
	sent := 0
	var unreviewed []repoPR
	for _, p := range open {
		count, err := p.repo.GitHub.SendBacks(ctx, p.open.Number, comments)
		if err != nil {
			return err
		}
		already, err := posted(ctx, p.repo.GitHub, p.open.Number, note, review)
		if err != nil {
			return err
		}
		if already {
			// An earlier try left this review, so the count holds it. Leave it out, as that try did.
			count--
		} else {
			unreviewed = append(unreviewed, p)
		}
		sent = max(sent, count)
	}
	for _, p := range unreviewed {
		if err := p.repo.GitHub.AddReview(ctx, p.open.Number, note, review); err != nil {
			return err
		}
	}
	if sent >= r.ReviewLimit {
		// A review sent for a failed CI has no run, and keeps nothing.
		kept := r.runs.keepOutputs(ctx, reviewerRole, n, nil)
		return toStuck(ctx, r.GitHub, n, reviewerRole.name, label.Review, label.Ready, fmt.Sprintf(
			"reviewer sent %s back to builder %d times, the limit, and its latest review on the PR still asks for changes.", r.prRefs(open), sent), "", kept)
	}
	return r.GitHub.SetStatus(ctx, n, label.Review, label.Ready)
}
