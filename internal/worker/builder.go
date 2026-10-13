package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
	"github.com/LeeShinYeoung/exomecha/internal/logging"
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
	project: true,
	from:    label.Working,
	back:    label.Ready,
	ready:   label.Ready,
	model:   func(m Models) agent.Model { return m.Builder },
}

// begin waits while a blocker is open. A run of a ready PRD was left when a person sent the PRD
// back to ready, and it holds a slot for nothing.
var builderTable = table{
	// none, preparing, running, stalled, dead, gone, finished
	away:    {idle, drop, drop, drop, drop, drop, drop},
	ready:   {begin, drop, drop, drop, drop, drop, drop},
	working: {restart, restart, idle, fail, fail, fail, harvest},
}

// builder runs claude in one sandbox per PRD. Each round it looks at the PRD's label and its
// sandbox and takes one step, so after a restart it picks up whatever is left the same way.
type builder struct {
	Deps
	runs *runs
}

func (*builder) Name() string { return builderRole.name }

// Pick returns the ready PRDs with no open blocker, every working PRD, and every PRD that has a
// builder sandbox, whatever its label.
func (b *builder) Pick(ctx context.Context) ([]int, error) {
	readyPRDs, err := b.GitHub.ListOpen(ctx, label.PRD, string(label.Ready))
	if err != nil {
		return nil, err
	}
	workingPRDs, err := b.GitHub.ListOpen(ctx, label.PRD, string(label.Working))
	if err != nil {
		return nil, err
	}
	var picked []int
	for _, t := range readyPRDs {
		unblocked, err := b.unblocked(ctx, t.Number)
		if err != nil {
			return nil, err
		}
		if unblocked {
			picked = append(picked, t.Number)
		}
	}
	for _, t := range workingPRDs {
		picked = append(picked, t.Number)
	}
	return b.runs.pick(ctx, builderRole, picked)
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
	prd, err := b.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	p := b.place(prd)
	repos, why := b.prdRepos(prd)
	if p != away && why != "" {
		return b.runs.invalid(ctx, builderRole, prd, why)
	}
	return b.runs.step(ctx, builderRole, n, builderTable[p],
		func(ctx context.Context) error { return b.start(ctx, prd, repos) },
		func(ctx context.Context, got run) error { return b.collect(ctx, prd, got.in) })
}

func (b *builder) place(prd github.Issue) place {
	switch {
	case holds(prd, label.Ready):
		return ready
	case holds(prd, label.Working):
		return working
	}
	return away
}

func (b *builder) start(ctx context.Context, prd github.Issue, repos []*Repo) error {
	n := prd.Number
	if unblocked, err := b.unblocked(ctx, n); err != nil || !unblocked {
		return err
	}
	return b.runs.start(ctx, builderRole, n, func(ctx context.Context) (spec, error) {
		branch := branchName(n)
		s := spec{tier: label.TierOf(prd.Labels)}
		var shown []repoWithPR
		var logs string
		for _, repo := range repos {
			base, err := fetchBase(ctx, repo.Git)
			if err != nil {
				return spec{}, err
			}
			remote, hasRemote, err := repo.Git.Rev(ctx, repo.Git.RemoteRef(branch))
			if err != nil {
				return spec{}, err
			}
			pr, hasPR, err := repo.GitHub.OpenPR(ctx, branch)
			if err != nil {
				return spec{}, err
			}
			rs := repoSpec{repo: repo, branch: branch, base: base, head: base, remote: remote}
			if hasPR {
				if !hasRemote {
					return spec{}, fmt.Errorf("PR %s#%d has no branch %s", repo.Name, pr, branch)
				}
				rs.head = remote
				logs += b.ciLogs(ctx, repo, len(repos), remote)
			}
			s.repos = append(s.repos, rs)
			shown = append(shown, repoWithPR{repo: repo, pr: pr})
		}
		input, err := sharedInput(ctx, b.Deps, prd, shown)
		if err != nil {
			return spec{}, err
		}
		s.input = input
		if logs != "" {
			s.input += "## CI logs\n\nThe end of the log of each CI job that failed on the latest commit of the PRD's PRs.\n\n" + logs
		}
		return s, nil
	})
}

// ciLogs is the end of the log of each CI job that failed on commit sha of repo. It goes to
// builder's input only and never into a comment, since a log holds whatever the project's CI
// printed. A log that cannot be read is left out, since builder can still run the checks itself.
// count is how many repositories the run has, which name theirs.
func (b *builder) ciLogs(ctx context.Context, repo *Repo, count int, sha string) string {
	ci, failed, err := repo.GitHub.CheckCI(ctx, sha)
	if err != nil {
		slog.WarnContext(ctx, "side task failed", logging.Task, "check CI for its logs", logging.Err, err)
		return ""
	}
	if ci != github.CIFailed {
		return ""
	}
	prefix := ""
	if count > 1 {
		prefix = repo.Name + ": "
	}
	var out strings.Builder
	for _, ciRun := range failed {
		logs, err := repo.GitHub.FailedJobLogs(ctx, ciRun.ID)
		if err != nil {
			slog.WarnContext(ctx, "side task failed", logging.Task, "read the logs of CI run "+ciRun.Name, logging.Err, err)
			continue
		}
		for _, l := range logs {
			fmt.Fprintf(&out, "### %s%s: %s\n\n```\n%s\n```\n\n", prefix, ciRun.Name, l.Job, l.Tail)
		}
	}
	return out.String()
}

func (b *builder) collect(ctx context.Context, prd github.Issue, in inputs) error {
	n := prd.Number
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
		return b.runs.failed(ctx, builderRole, n, badResult)
	}

	heads, ok, err := b.runs.take(ctx, builderRole, n, in.Repos, out.Summary)
	if err != nil || !ok {
		return err
	}
	var changed []int
	for i, rr := range in.Repos {
		if heads[i] != rr.Head {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		for _, rr := range in.Repos {
			changes, err := b.runs.changes(ctx, b.runs.name(builderRole, n), rr.Dir)
			if err != nil {
				return err
			}
			if changes != "" {
				return b.runs.stuck(ctx, builderRole, n, "The AI finished without committing its changes"+rr.where()+".\n\n"+out.Summary)
			}
		}
		return b.runs.stuck(ctx, builderRole, n, "Nothing to change (it may already be done).\n\n"+out.Summary)
	}
	// Merging one PR finishes the PRD only when it changes a single repository. With several,
	// exomecha closes the PRD once all its PRs are merged.
	keyword := "Closes"
	if len(in.Repos) > 1 {
		keyword = "Part of"
	}
	var prs []repoWithPR
	for i, rr := range in.Repos {
		repo, _ := b.runs.repo(rr.Repo)
		if slices.Contains(changed, i) {
			// A run that did not start from the branch began from the base branch, so it replaces any
			// old branch of the same name.
			if err := repo.Git.PushCommit(ctx, heads[i], branch, rr.Head != rr.Remote); err != nil {
				return fmt.Errorf("%s: %w", repo.Name, err)
			}
		}
		pr, hasPR, err := repo.GitHub.OpenPR(ctx, branch)
		if err != nil {
			return err
		}
		if !hasPR && slices.Contains(changed, i) {
			// The keyword is what makes GitHub link the PR and the PRD to each other.
			body := fmt.Sprintf("%s %s\n\n%s", keyword, b.ref(repo.Name, n), out.Summary)
			if pr, err = repo.GitHub.CreatePR(ctx, branch, repo.Git.Base(), prd.Title, body); err != nil {
				return err
			}
			hasPR = true
		}
		if hasPR {
			prs = append(prs, repoWithPR{repo: repo, pr: pr})
		}
	}
	// Every run's summary goes on the PRs, where the record shows the next worker what this run did.
	note := github.Note{Worker: builderRole.name, Kind: github.KindSummary}
	for _, p := range prs {
		already, err := posted(ctx, p.repo.GitHub, p.pr, note, out.Summary)
		if err != nil {
			return err
		}
		if already {
			continue
		}
		if err := p.repo.GitHub.AddComment(ctx, p.pr, note, out.Summary); err != nil {
			return err
		}
	}
	if err := b.GitHub.SetStatus(ctx, n, label.Working, label.Review); err != nil {
		return err
	}
	return b.runs.drop(ctx, builderRole, n)
}
