package worker

import (
	"context"
	"fmt"
	"slices"
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
	name:    "resolver",
	schema:  resolverSchema,
	project: true,
	from:    label.Review,
	back:    label.Review,
	// A conflict asks the same judgment whatever the PRD's tier.
	model: func(d Deps) agent.Model { return d.ResolverModel },
}

// begin does nothing when the PRD branch is missing or already has the base branch.
var resolverTable = table{
	// none, preparing, running, stalled, dead, gone, finished
	away:  {idle, drop, drop, drop, drop, drop, drop},
	clean: {idle, restart, idle, fail, fail, fail, harvest},
	ready: {begin, restart, idle, fail, fail, fail, harvest},
}

// resolver works like builder, one sandbox per PRD, while the PRD stays in review.
type resolver struct {
	Deps
	runs *runs
}

func (*resolver) Name() string { return resolverRole.name }

// Pick returns the PRDs in review or approved with a PR that conflicts, and every PRD that has a
// resolver sandbox.
func (r *resolver) Pick(ctx context.Context) ([]int, error) {
	review, err := r.GitHub.ListOpen(ctx, label.PRD, string(label.Review))
	if err != nil {
		return nil, err
	}
	approved, err := r.GitHub.ListOpen(ctx, label.PRD, string(label.Approved))
	if err != nil {
		return nil, err
	}
	var picked []int
	for _, t := range append(review, approved...) {
		p, _, err := r.place(ctx, t)
		if err != nil {
			return nil, err
		}
		if p == ready || p == stale {
			picked = append(picked, t.Number)
		}
	}
	return r.runs.pick(ctx, resolverRole, picked)
}

func (r *resolver) Handle(ctx context.Context, n int) error {
	prd, err := r.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	p, prs, err := r.place(ctx, prd)
	if err != nil {
		return err
	}
	if p == stale {
		// Resolving changes the code the approval was for, so the PRD goes back to review, where
		// the next check resolves it as any other.
		return r.GitHub.SetStatus(ctx, n, label.Approved, label.Review)
	}
	return r.runs.step(ctx, resolverRole, n, resolverTable[p],
		func(ctx context.Context) error { return r.start(ctx, prd, prs) },
		func(ctx context.Context, got run) error { return r.collect(ctx, n, got.in) })
}

// place is away for a PRD whose Repositories section is wrong, which reviewer sends to stuck.
func (r *resolver) place(ctx context.Context, prd github.Issue) (place, []repoPR, error) {
	approved := holds(prd, label.Approved)
	if !approved && !holds(prd, label.Review) {
		return away, nil, nil
	}
	repos, why := r.prdRepos(prd)
	if why != "" {
		return away, nil, nil
	}
	prs, err := prdPRs(ctx, repos, prd.Number)
	if err != nil {
		return away, nil, err
	}
	conflicts := conflicting(prs)
	switch {
	case approved && conflicts:
		return stale, prs, nil
	case approved:
		return away, prs, nil
	case conflicts:
		return ready, prs, nil
	}
	return clean, prs, nil
}

// start merges the base branch in each repository whose PR conflicts. The sandbox gets the PRD's
// other repositories too, as builder's does, so the AI can see how they fit.
func (r *resolver) start(ctx context.Context, prd github.Issue, prs []repoPR) error {
	n := prd.Number
	branch := branchName(n)
	var s spec
	var shown []repoWithPR
	for _, p := range prs {
		base, err := fetchBase(ctx, p.repo.Git)
		if err != nil {
			return err
		}
		rs := repoSpec{repo: p.repo, base: base, head: base}
		if p.open.Number != 0 {
			head, ok, err := p.repo.Git.Rev(ctx, p.repo.Git.RemoteRef(branch))
			if err != nil || !ok {
				return err
			}
			rs.branch, rs.head, rs.remote = branch, head, head
			if conflicting([]repoPR{p}) {
				// The branch may already have the base branch, and GitHub has not caught up yet.
				upToDate, err := p.repo.Git.IsAncestor(ctx, base, head)
				if err != nil {
					return err
				}
				rs.merge = !upToDate
			}
		}
		s.repos = append(s.repos, rs)
		shown = append(shown, repoWithPR{repo: p.repo, pr: p.open.Number})
	}
	// Leave before setup runs for nothing.
	if !slices.ContainsFunc(s.repos, func(rs repoSpec) bool { return rs.merge }) {
		return nil
	}
	return r.runs.start(ctx, resolverRole, n, func(ctx context.Context) (spec, error) {
		input, err := sharedInput(ctx, r.Deps, prd, shown)
		if err != nil {
			return spec{}, err
		}
		s.input = input
		return s, nil
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
		return r.runs.failed(ctx, resolverRole, n, badResult)
	}

	// Only the merges are resolver's to push. Anything else the AI changed stays in the sandbox.
	var merges []repoRun
	for _, rr := range in.Repos {
		if rr.Merge {
			merges = append(merges, rr)
		}
	}
	heads, ok, err := r.runs.take(ctx, resolverRole, n, merges, out.Reason)
	if err != nil || !ok {
		return err
	}
	for i, rr := range merges {
		wrong, err := r.unfinished(ctx, n, rr, heads[i])
		if err != nil {
			return err
		}
		if wrong != "" {
			return r.runs.stuck(ctx, resolverRole, n, wrong+"\n\n"+out.Reason)
		}
	}
	for i, rr := range merges {
		repo, _ := r.runs.repo(rr.Repo)
		if err := repo.Git.PushCommit(ctx, heads[i], branchName(n), false); err != nil {
			return fmt.Errorf("%s: %w", repo.Name, err)
		}
	}
	// The record shows reviewer and the next workers how the merge was settled.
	note := github.Note{Worker: resolverRole.name, Kind: github.KindResolved}
	for _, rr := range in.Repos {
		repo, ok := r.runs.repo(rr.Repo)
		if !ok {
			continue
		}
		pr, open, err := repo.GitHub.OpenPR(ctx, branchName(n))
		if err != nil {
			return err
		}
		if !open {
			continue
		}
		already, err := posted(ctx, repo.GitHub, pr, note, out.Reason)
		if err != nil {
			return err
		}
		if already {
			continue
		}
		if err := repo.GitHub.AddComment(ctx, pr, note, out.Reason); err != nil {
			return err
		}
	}
	// The PRD stays in review, and reviewer looks at it again at its next check, a minute later.
	return r.runs.drop(ctx, resolverRole, n)
}

// unfinished returns what is still wrong in one repository after the AI reported the merge
// resolved, or "" if nothing is.
func (r *resolver) unfinished(ctx context.Context, n int, rr repoRun, head string) (string, error) {
	repo, _ := r.runs.repo(rr.Repo)
	// rev-parse exits 1 when there is no merge in progress, which sh would report as an error.
	merging, err := r.runs.sh(ctx, resolverRole, n, rr.Dir, "git rev-parse -q --verify MERGE_HEAD || true")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(merging) != "" {
		return "The AI said the conflicts were resolved, but the merge" + rr.where() + " was not committed.", nil
	}
	left, err := repo.Git.ConflictedFiles(ctx, head, rr.Conflicts)
	if err != nil {
		return "", err
	}
	if len(left) > 0 {
		return "The AI said the conflicts were resolved, but conflict markers remain" + rr.where() + " in: " + strings.Join(left, ", "), nil
	}
	ok, err := repo.Git.IsAncestor(ctx, rr.Base, head)
	if err != nil || ok {
		return "", err
	}
	return "The AI committed, but the result" + rr.where() + " does not include the base branch.", nil
}
