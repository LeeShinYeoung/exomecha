package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v92/github"

	"github.com/LeeShinYeoung/exomecha/internal/label"
)

func (c *Client) EnsureLabels(ctx context.Context) ([]string, error) {
	have := map[string]bool{}
	for l, err := range c.api.Issues.ListLabelsIter(ctx, c.owner, c.repo, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, err
		}
		have[strings.ToLower(l.GetName())] = true
	}
	var created []string
	for _, d := range label.All() {
		if have[strings.ToLower(d.Name)] {
			continue
		}
		_, _, err := c.api.Issues.CreateLabel(ctx, c.owner, c.repo, gh.CreateIssueLabelRequest{
			Name:  d.Name,
			Color: gh.Ptr(d.Color),
		})
		if err != nil {
			return created, fmt.Errorf("create label %s: %w", d.Name, err)
		}
		created = append(created, d.Name)
	}
	return created, nil
}

// Adding before removing also clears an old status a person left behind when relabeling by hand.
func (c *Client) SetStatus(ctx context.Context, number int, from, to label.Status) error {
	if !label.CanTransition(from, to) {
		return fmt.Errorf("transition not allowed: %q → %q", from, to)
	}
	labels, _, err := c.api.Issues.AddLabelsToIssue(ctx, c.owner, c.repo, number, []string{string(to)})
	if err != nil {
		return err
	}
	for _, l := range labels {
		if name := l.GetName(); label.IsStatus(name) && name != string(to) {
			if err := c.RemoveLabel(ctx, number, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Client) ClearStatus(ctx context.Context, number int) error {
	issue, err := c.Issue(ctx, number)
	if err != nil {
		return err
	}
	for _, name := range issue.Labels {
		if label.IsStatus(name) {
			if err := c.RemoveLabel(ctx, number, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Client) AddLabel(ctx context.Context, number int, name string) error {
	_, _, err := c.api.Issues.AddLabelsToIssue(ctx, c.owner, c.repo, number, []string{name})
	return err
}

func (c *Client) RemoveLabel(ctx context.Context, number int, name string) error {
	_, err := c.api.Issues.RemoveLabelForIssue(ctx, c.owner, c.repo, number, name)
	if isNotFound(err) {
		return nil
	}
	return err
}
