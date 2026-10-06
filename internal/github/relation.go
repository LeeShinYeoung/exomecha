package github

import (
	"context"

	gh "github.com/google/go-github/v92/github"
)

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
