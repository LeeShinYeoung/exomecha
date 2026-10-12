package github

import (
	"cmp"
	"context"
	"fmt"
	"slices"
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
			Color: new(d.Color),
		})
		if err != nil {
			return created, fmt.Errorf("create label %s: %w", d.Name, err)
		}
		created = append(created, d.Name)
	}
	return created, nil
}

// SetStatus replaces the issue's labels in one call, so a failure leaves the old status and never
// two. It writes nothing unless from is still the issue's only status: a person may have changed
// it after the caller read the issue.
func (c *Client) SetStatus(ctx context.Context, number int, from, to label.Status) error {
	if !label.CanTransition(from, to) {
		return fmt.Errorf("transition not allowed: %q → %q", from, to)
	}
	return c.replaceStatus(ctx, number, from, to)
}

func (c *Client) ClearStatus(ctx context.Context, number int) error {
	return c.replaceStatus(ctx, number, label.None, label.None)
}

// A from of None, which no transition starts from, clears whatever status the issue has.
func (c *Client) replaceStatus(ctx context.Context, number int, from, to label.Status) error {
	issue, err := c.Issue(ctx, number)
	if err != nil {
		return err
	}
	// Not nil: an issue left with no label must still send GitHub an empty list.
	labels := []string{}
	var current []string
	for _, name := range issue.Labels {
		if label.IsStatus(name) {
			current = append(current, name)
		} else {
			labels = append(labels, name)
		}
	}
	if from != label.None && !slices.Equal(current, []string{string(from)}) {
		return fmt.Errorf("the status of issue #%d is now %s, not %s", number, cmp.Or(strings.Join(current, ", "), "none"), from)
	}
	if to != label.None {
		labels = append(labels, string(to))
	}
	_, _, err = c.api.Issues.ReplaceLabelsForIssue(ctx, c.owner, c.repo, number, labels)
	return err
}
