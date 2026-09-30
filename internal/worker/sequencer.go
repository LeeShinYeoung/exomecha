package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/LeeShinYeoung/coworker/internal/agent"
	"github.com/LeeShinYeoung/coworker/internal/github"
	"github.com/LeeShinYeoung/coworker/internal/label"
)

const sequencerSchema = `{
  "type": "object",
  "properties": {
    "blocked_by": {"type": "array", "items": {"type": "integer"}}
  },
  "required": ["blocked_by"],
  "additionalProperties": false
}`

type sequencer struct{ Deps }

func (*sequencer) Name() string { return "sequencer" }

func (s *sequencer) Pick(ctx context.Context) ([]int, error) {
	tasks, err := s.GitHub.ListOpen(ctx, label.Task, string(label.Pending))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, t := range tasks {
		out = append(out, t.Number)
	}
	return out, nil
}

func (s *sequencer) Handle(ctx context.Context, n int) error {
	task, ok, err := reload(ctx, s.GitHub, n, label.Task, string(label.Pending))
	if err != nil || !ok {
		return err
	}
	blockers, err := s.GitHub.BlockedBy(ctx, n)
	if err != nil {
		return err
	}
	open, err := s.GitHub.ListOpen(ctx, label.Task)
	if err != nil {
		return err
	}
	// Only lower-numbered Tasks can become blockers, so two Tasks can never wait on each other.
	candidates := map[int]github.Issue{}
	var others []github.Issue
	for _, o := range open {
		if o.Number < n {
			candidates[o.Number] = o
			others = append(others, o)
		}
	}

	var added []int
	if len(others) > 0 {
		input, err := s.input(ctx, task, blockers, others)
		if err != nil {
			return err
		}
		wt, err := s.Git.OpenBase(ctx)
		if err != nil {
			return err
		}
		defer wt.Remove()
		res, err := s.Agent.Run(ctx, agent.Request{
			Dir:          wt.Dir,
			Instructions: prompt("sequencer"),
			Input:        input,
			Schema:       sequencerSchema,
		})
		if err != nil {
			return err
		}
		var out struct {
			BlockedBy []int `json:"blocked_by"`
		}
		if err := decode(res, &out); err != nil {
			return err
		}
		for _, b := range out.BlockedBy {
			o, ok := candidates[b]
			if !ok {
				continue
			}
			if err := s.GitHub.AddBlockedBy(ctx, n, o); err != nil {
				return err
			}
			added = append(added, b)
		}
	}

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
