package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
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
	// For a PRD with no tier label. A tier label takes its place.
	tier: label.Normal,
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
	ready, err := b.GitHub.ListOpen(ctx, label.PRD, string(label.Ready))
	if err != nil {
		return nil, err
	}
	working, err := b.GitHub.ListOpen(ctx, label.PRD, string(label.Working))
	if err != nil {
		return nil, err
	}
	var picked []int
	for _, t := range ready {
		unblocked, err := b.unblocked(ctx, t.Number)
		if err != nil {
			return nil, err
		}
		if unblocked {
			picked = append(picked, t.Number)
		}
	}
	for _, t := range working {
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
	return b.runs.step(ctx, builderRole, n, builderTable[b.place(prd)],
		func(ctx context.Context) error { return b.start(ctx, prd) },
		func(ctx context.Context, run run) error { return b.collect(ctx, prd, run.in) })
}

func (b *builder) place(prd github.Issue) place {
	switch {
	case holds(prd, label.PRD, label.Ready):
		return ready
	case holds(prd, label.PRD, label.Working):
		return working
	}
	return away
}

func (b *builder) start(ctx context.Context, prd github.Issue) error {
	n := prd.Number
	if ready, err := b.unblocked(ctx, n); err != nil || !ready {
		return err
	}
	return b.runs.start(ctx, builderRole, n, func(ctx context.Context) (spec, error) {
		branch := branchName(n)
		base, err := baseCommit(ctx, b.Git)
		if err != nil {
			return spec{}, err
		}
		remote, hasRemote, err := b.Git.Rev(ctx, b.Git.RemoteRef(branch))
		if err != nil {
			return spec{}, err
		}
		pr, hasPR, err := b.GitHub.OpenPR(ctx, branch)
		if err != nil {
			return spec{}, err
		}
		input, err := prdInput(ctx, b.GitHub, prd)
		if err != nil {
			return spec{}, err
		}
		s := spec{branch: branch, base: base, head: base, remote: remote, input: input,
			tier: label.TierOf(prd.Labels)}
		if !hasPR {
			return s, nil
		}
		if !hasRemote {
			return spec{}, fmt.Errorf("PR #%d has no branch %s", pr, branch)
		}
		s.head = remote
		s.input += fmt.Sprintf("## Existing PR\n\nPR #%d already has earlier work on this PRD. The current directory is its branch.\n\n", pr)
		onPR, err := prComments(ctx, b.GitHub, pr)
		if err != nil {
			return spec{}, err
		}
		s.input += onPR
		review, err := b.GitHub.LatestReview(ctx, pr)
		if err != nil {
			return spec{}, err
		}
		if review != "" {
			s.input += "## Last review (exomecha's latest review on this PR: changes to make or CI failures)\n\n" + review + "\n"
		}
		return s, nil
	})
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
		return b.runs.failed(ctx, builderRole, n, badResult, fmt.Sprintf("result %q", out.Result))
	}

	head, ok, err := b.runs.take(ctx, builderRole, n, in, out.Summary)
	if err != nil || !ok {
		return err
	}
	if head == in.Head {
		changes, err := b.runs.changes(ctx, b.runs.name(builderRole, n))
		if err != nil {
			return err
		}
		if changes != "" {
			return b.runs.stuck(ctx, builderRole, n, "The AI finished without committing its changes.\n\n"+out.Summary)
		}
		return b.runs.stuck(ctx, builderRole, n, "Nothing to change (it may already be done).\n\n"+out.Summary)
	}
	// A run that did not start from the branch began from the base branch, so it replaces any old
	// branch of the same name.
	if err := b.Git.PushCommit(ctx, head, branch, in.Head != in.Remote); err != nil {
		return err
	}
	pr, hasPR, err := b.GitHub.OpenPR(ctx, branch)
	if err != nil {
		return err
	}
	if !hasPR {
		// "Closes" is what makes GitHub link the PR and the PRD to each other.
		pr, err = b.GitHub.CreatePR(ctx, branch, b.Git.Base(), prd.Title, fmt.Sprintf("Closes #%d\n\n%s", n, out.Summary))
		if err != nil {
			return err
		}
	}
	if err := b.GitHub.SetStatus(ctx, n, label.Working, label.Review); err != nil {
		return err
	}
	slog.Info("pushed PR", "prd", n, "pr", pr)
	return b.runs.done(ctx, builderRole, n)
}
