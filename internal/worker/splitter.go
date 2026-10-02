package worker

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/LeeShinYeoung/exomecha/internal/github"
	"github.com/LeeShinYeoung/exomecha/internal/label"
)

const splitterSchema = `{
  "type": "object",
  "properties": {
    "tasks": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "title": {"type": "string"},
          "body": {"type": "string"},
          "blocked_by": {"type": "array", "items": {"type": "integer"}}
        },
        "required": ["title", "body", "blocked_by"],
        "additionalProperties": false
      }
    }
  },
  "required": ["tasks"],
  "additionalProperties": false
}`

// A PRD has no status while splitter works on it. A failed split puts status:stuck on it, and
// once a person removes that label, splitter takes the PRD again.
var splitterRole = role{
	name:   "splitter",
	schema: splitterSchema,
	from:   label.None,
	back:   label.None,
}

var splitterTable = table{
	// none, preparing, running, stalled, dead, gone, finished
	away:  {idle, drop, drop, drop, drop, drop, drop},
	ready: {begin, restart, idle, fail, fail, fail, harvest},
}

// splitter splits each PRD into Tasks in a sandbox of its own, which gets only the code.
type splitter struct {
	Deps
	runs *runs
}

func (*splitter) Name() string { return splitterRole.name }

func (s *splitter) Pick(ctx context.Context) ([]int, error) {
	prds, err := s.GitHub.ListOpen(ctx, label.PRD)
	if err != nil {
		return nil, err
	}
	var picked []int
	for _, p := range prds {
		if s.place(p) == ready {
			picked = append(picked, p.Number)
		}
	}
	return s.runs.pick(ctx, splitterRole, picked)
}

func (s *splitter) Handle(ctx context.Context, n int) error {
	prd, err := s.GitHub.Issue(ctx, n)
	if err != nil {
		return err
	}
	return s.runs.step(ctx, splitterRole, n, splitterTable[s.place(prd)],
		func(ctx context.Context) error { return s.start(ctx, prd) },
		func(ctx context.Context, _ run) error { return s.collect(ctx, n) })
}

// A PRD gets a status only from a failed split, so any status keeps it away until a person removes it.
func (*splitter) place(prd github.Issue) place {
	if !prd.Open || !prd.HasLabel(label.PRD) || prd.HasLabel(label.Tasked) || prd.Status() != label.None {
		return away
	}
	return ready
}

func (s *splitter) start(ctx context.Context, prd github.Issue) error {
	n := prd.Number
	return s.runs.start(ctx, splitterRole, n, func(ctx context.Context) (spec, error) {
		base, err := baseCommit(ctx, s.Git)
		if err != nil {
			return spec{}, err
		}
		comments, err := issueComments(ctx, s.GitHub, n)
		if err != nil {
			return spec{}, err
		}
		input := fmt.Sprintf("# PRD #%d: %s\n\n%s\n\n%s", n, prd.Title, prd.Body, comments)
		return spec{base: base, head: base, input: input}, nil
	})
}

func (s *splitter) collect(ctx context.Context, n int) error {
	var out struct {
		Tasks []struct {
			Title     string `json:"title"`
			Body      string `json:"body"`
			BlockedBy []int  `json:"blocked_by"`
		} `json:"tasks"`
	}
	if ok, err := s.runs.result(ctx, splitterRole, n, &out); err != nil || !ok {
		return err
	}
	if len(out.Tasks) == 0 {
		return s.runs.failed(ctx, splitterRole, n, "the AI split the PRD into no Tasks", "")
	}

	// Sub-issues on an untasked PRD mean an earlier try stopped halfway: close them and start over.
	subs, err := s.GitHub.SubIssues(ctx, n)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if !sub.Open {
			continue
		}
		if err := s.GitHub.ClearStatus(ctx, sub.Number); err != nil {
			return err
		}
		if err := s.GitHub.Close(ctx, sub.Number, github.NotPlanned); err != nil {
			return err
		}
		slog.Info("closed task from an unfinished split", "prd", n, "task", sub.Number)
	}

	// pending goes on only after every Task is created and linked, and tasked goes on the PRD last, so no
	// worker picks up a half-built set. Each Task is attached right away so the cleanup above can find it.
	tasks := make([]github.Issue, len(out.Tasks))
	for i, t := range out.Tasks {
		tasks[i], err = s.GitHub.CreateIssue(ctx, t.Title, t.Body, []string{label.Task})
		if err != nil {
			return err
		}
		if err := s.GitHub.AddSubIssue(ctx, n, tasks[i]); err != nil {
			return err
		}
	}
	for i, t := range out.Tasks {
		for _, seq := range t.BlockedBy {
			// seq is 1-based and may only point at an earlier Task.
			if seq < 1 || seq > i {
				continue
			}
			if err := s.GitHub.AddBlockedBy(ctx, tasks[i].Number, tasks[seq-1]); err != nil {
				return err
			}
		}
	}
	for _, t := range tasks {
		if err := s.GitHub.SetStatus(ctx, t.Number, label.None, label.Pending); err != nil {
			return err
		}
	}
	if err := s.GitHub.AddLabel(ctx, n, label.Tasked); err != nil {
		return err
	}

	numbers := make([]int, len(tasks))
	for i, t := range tasks {
		numbers[i] = t.Number
	}
	slog.Info("split PRD into tasks", "prd", n, "tasks", numbers)
	return s.runs.done(ctx, splitterRole, n)
}
