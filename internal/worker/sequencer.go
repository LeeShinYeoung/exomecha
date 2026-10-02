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

const sequencerSchema = `{
  "type": "object",
  "properties": {
    "blocked_by": {"type": "array", "items": {"type": "integer"}}
  },
  "required": ["blocked_by"],
  "additionalProperties": false
}`

var sequencerRole = role{
	name:   "sequencer",
	schema: sequencerSchema,
	from:   label.Pending,
	back:   label.Pending,
}

// begin queues a Task with no lower open Task without a run.
var sequencerTable = table{
	// none, preparing, running, stalled, dead, gone, finished
	away:  {idle, drop, drop, drop, drop, drop, drop},
	ready: {begin, restart, idle, fail, fail, fail, harvest},
}

// sequencer puts each Task in line in a sandbox of its own, which gets only the code.
type sequencer struct {
	Deps
	runs *runs
}

func (*sequencer) Name() string { return sequencerRole.name }

func (s *sequencer) Pick(ctx context.Context) ([]int, error) {
	tasks, err := s.GitHub.ListOpen(ctx, label.Task, string(label.Pending))
	if err != nil {
		return nil, err
	}
	var picked []int
	for _, t := range tasks {
		picked = append(picked, t.Number)
	}
	return s.runs.pick(ctx, sequencerRole, picked)
}

func (s *sequencer) Handle(ctx context.Context, n int) error {
	task, err := s.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	return s.runs.step(ctx, sequencerRole, n, sequencerTable[s.place(task)],
		func(ctx context.Context) error { return s.start(ctx, task) },
		func(ctx context.Context, _ run) error { return s.collect(ctx, n) })
}

func (*sequencer) place(task github.Issue) place {
	if holds(task, label.Task, label.Pending) {
		return ready
	}
	return away
}

func (s *sequencer) start(ctx context.Context, task github.Issue) error {
	n := task.Number
	others, err := s.candidates(ctx, n)
	if err != nil {
		return err
	}
	// With no Task to wait for there is nothing for the AI to decide, so no run and no usage check.
	if len(others) == 0 {
		return s.queue(ctx, n, nil)
	}
	return s.runs.start(ctx, sequencerRole, n, func(ctx context.Context) (spec, error) {
		base, err := baseCommit(ctx, s.Git)
		if err != nil {
			return spec{}, err
		}
		blockers, err := s.GitHub.BlockedBy(ctx, n)
		if err != nil {
			return spec{}, err
		}
		input, err := s.input(ctx, task, blockers, others)
		if err != nil {
			return spec{}, err
		}
		return spec{base: base, head: base, input: input}, nil
	})
}

// candidates returns the open Tasks numbered below n. Only these can become n's blockers, so two
// Tasks can never wait on each other.
func (s *sequencer) candidates(ctx context.Context, n int) ([]github.Issue, error) {
	open, err := s.GitHub.ListOpen(ctx, label.Task)
	if err != nil {
		return nil, err
	}
	var out []github.Issue
	for _, o := range open {
		if o.Number < n {
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *sequencer) collect(ctx context.Context, n int) error {
	var out struct {
		BlockedBy []int `json:"blocked_by"`
	}
	if ok, err := s.runs.result(ctx, sequencerRole, n, &out); err != nil || !ok {
		return err
	}
	// Read again, since a candidate closed during the run is no longer worth waiting for.
	others, err := s.candidates(ctx, n)
	if err != nil {
		return err
	}
	var added []int
	for _, b := range out.BlockedBy {
		i := slices.IndexFunc(others, func(o github.Issue) bool { return o.Number == b })
		if i < 0 {
			continue
		}
		if err := s.GitHub.AddBlockedBy(ctx, n, others[i]); err != nil {
			return err
		}
		added = append(added, b)
	}
	if err := s.queue(ctx, n, added); err != nil {
		return err
	}
	return s.runs.done(ctx, sequencerRole, n)
}

func (s *sequencer) queue(ctx context.Context, n int, added []int) error {
	if err := s.GitHub.SetStatus(ctx, n, label.Pending, label.Queued); err != nil {
		return err
	}
	slog.Info("queued task", "task", n, "added_blocked_by", added)
	return nil
}

func (s *sequencer) input(ctx context.Context, task github.Issue, blockers, others []github.Issue) (string, error) {
	var b strings.Builder
	b.WriteString(taskText(task))

	b.WriteString("## Already blocked by\n\n")
	if len(blockers) == 0 {
		b.WriteString("None\n")
	}
	for _, x := range blockers {
		state := "open"
		if !x.Open {
			state = "closed"
		}
		fmt.Fprintf(&b, "- #%d %s (%s)\n", x.Number, x.Title, state)
	}

	b.WriteString("\n## Candidates (open Tasks with a lower number than this one)\n\n")
	for _, o := range others {
		fmt.Fprintf(&b, "### #%d %s (%s)\n\n", o.Number, o.Title, o.Status())
		pr, ok, err := s.GitHub.OpenPR(ctx, branchName(o.Number))
		if err != nil {
			return "", err
		}
		if ok {
			files, err := s.GitHub.PRFiles(ctx, pr)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "Files changed in PR #%d:\n", pr)
			for _, f := range files {
				fmt.Fprintf(&b, "- %s\n", f)
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(o.Body))
	}
	return b.String(), nil
}
