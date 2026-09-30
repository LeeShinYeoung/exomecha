package github

import (
	"context"
	"slices"

	gh "github.com/google/go-github/v92/github"
)

func (c *Client) AddSubIssue(ctx context.Context, parent int, child Issue) error {
	subs, err := c.SubIssues(ctx, parent)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(subs, func(s Issue) bool { return s.ID == child.ID }) {
		return nil
	}
	// The API takes the issue ID, not the issue number.
	_, _, err = c.api.SubIssue.Add(ctx, c.owner, c.repo, int64(parent), gh.SubIssueRequest{SubIssueID: child.ID})
	return err
}

func (c *Client) SubIssues(ctx context.Context, parent int) ([]Issue, error) {
	var out []Issue
	for s, err := range c.api.SubIssue.ListByIssueIter(ctx, c.owner, c.repo, int64(parent), &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, err
		}
		out = append(out, issueFrom((*gh.Issue)(s)))
	}
	return out, nil
}

func (c *Client) Parent(ctx context.Context, number int) (parent Issue, ok bool, err error) {
	p, _, err := c.api.SubIssue.GetParentIssue(ctx, c.owner, c.repo, int64(number))
	if isNotFound(err) {
		return Issue{}, false, nil
	}
	if err != nil {
		return Issue{}, false, err
	}
	return issueFrom(p), true, nil
}

func (c *Client) AddBlockedBy(ctx context.Context, number int, blocker Issue) error {
	blockers, err := c.BlockedBy(ctx, number)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(blockers, func(b Issue) bool { return b.ID == blocker.ID }) {
		return nil
	}
	// The API takes the issue ID, not the issue number.
	_, _, err = c.api.Issues.AddBlockedBy(ctx, c.owner, c.repo, int64(number), gh.IssueDependencyRequest{IssueID: blocker.ID})
	return err
}

func (c *Client) BlockedBy(ctx context.Context, number int) ([]Issue, error) {
	var out []Issue
	for i, err := range c.api.Issues.ListBlockedByIter(ctx, c.owner, c.repo, int64(number), &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, err
		}
		out = append(out, issueFrom(i))
	}
	return out, nil
}
