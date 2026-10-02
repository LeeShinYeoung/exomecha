package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/LeeShinYeoung/exomecha/internal/agent"
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

type splitter struct{ Deps }

func (*splitter) Name() string { return "splitter" }

func (s *splitter) Pick(ctx context.Context) ([]int, error) {
	prds, err := s.GitHub.ListOpen(ctx, label.PRD)
	if err != nil {
		return nil, err
	}
	var out []int
	for _, p := range prds {
		if !p.HasLabel(label.Tasked) {
			out = append(out, p.Number)
		}
	}
	return out, nil
}

func (s *splitter) Handle(ctx context.Context, n int) error {
	prd, ok, err := reload(ctx, s.GitHub, n, label.PRD)
	if err != nil || !ok || prd.HasLabel(label.Tasked) {
		return err
	}

	// Sub-issues on an untasked PRD mean an earlier split stopped halfway: close them and start over.
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

	comments, err := issueComments(ctx, s.GitHub, n)
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
		Instructions: prompt("splitter"),
		Input:        fmt.Sprintf("# PRD #%d: %s\n\n%s\n\n%s", n, prd.Title, prd.Body, comments),
		Schema:       splitterSchema,
	})
	if err != nil {
		return err
	}
	var out struct {
		Tasks []struct {
			Title     string `json:"title"`
			Body      string `json:"body"`
			BlockedBy []int  `json:"blocked_by"`
		} `json:"tasks"`
	}
	if err := decode(res, &out); err != nil {
		return err
	}
	if len(out.Tasks) == 0 {
		return errors.New("AI returned no tasks")
	}

	// pending goes on only after every Task is created and linked, and tasked goes on the PRD last, so no
	// worker picks up a half-built set. Each Task is attached right away so the recovery above can find it.
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
	return nil
}
