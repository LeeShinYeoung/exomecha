package github

import (
	"context"

	gh "github.com/google/go-github/v92/github"
)

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
