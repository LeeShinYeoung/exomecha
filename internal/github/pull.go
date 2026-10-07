package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"
)

type PR struct {
	Number    int
	HeadSHA   string
	Mergeable *bool // nil while GitHub is still computing it
}

func (c *Client) OpenPR(ctx context.Context, branch string) (number int, ok bool, err error) {
	prs, _, err := c.api.PullRequests.List(ctx, c.owner, c.repo, &gh.PullRequestListOptions{
		State: "open",
		Head:  c.owner + ":" + branch,
	})
	if err != nil {
		return 0, false, err
	}
	if len(prs) == 0 {
		return 0, false, nil
	}
	return prs[0].GetNumber(), true, nil
}

// Merged reports whether the newest closed PR from branch was merged.
func (c *Client) Merged(ctx context.Context, branch string) (bool, error) {
	// GitHub lists the newest first.
	prs, _, err := c.api.PullRequests.List(ctx, c.owner, c.repo, &gh.PullRequestListOptions{
		State:       "closed",
		Head:        c.owner + ":" + branch,
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil || len(prs) == 0 {
		return false, err
	}
	return prs[0].MergedAt != nil, nil
}

func (c *Client) CreatePR(ctx context.Context, branch, base, title, body string) (int, error) {
	pr, _, err := c.api.PullRequests.Create(ctx, c.owner, c.repo, gh.CreatePullRequest{
		Title: gh.Ptr(title),
		Head:  branch,
		Base:  base,
		Body:  gh.Ptr(body),
	})
	if err != nil {
		return 0, err
	}
	return pr.GetNumber(), nil
}

func (c *Client) PR(ctx context.Context, number int) (PR, error) {
	pr, _, err := c.api.PullRequests.Get(ctx, c.owner, c.repo, number)
	if err != nil {
		return PR{}, err
	}
	return PR{
		Number:    pr.GetNumber(),
		HeadSHA:   pr.GetHead().GetSHA(),
		Mergeable: pr.Mergeable,
	}, nil
}

// COMMENT only: the token owner authored the PR, and GitHub blocks approving or requesting changes on your own PR.
func (c *Client) AddReview(ctx context.Context, number int, body string) error {
	_, _, err := c.api.PullRequests.CreateReview(ctx, c.owner, c.repo, number, &gh.PullRequestReviewRequest{
		Body:  gh.Ptr(Marker + "\n" + body),
		Event: gh.Ptr("COMMENT"),
	})
	return err
}

// approval starts the review exomecha leaves when reviewer approves, so LatestReview can tell it
// from a review that asks for changes.
const approval = "exomecha's reviewer approved this PR. A person reviews and merges it."

func (c *Client) AddApproval(ctx context.Context, number int, summary string) error {
	return c.AddReview(ctx, number, approval+"\n\n"+summary)
}

// LatestReview returns exomecha's latest review, or "" when that review is an approval, since
// whatever an earlier review asked for was done before it.
func (c *Client) LatestReview(ctx context.Context, number int) (string, error) {
	var latest string
	for r, err := range c.api.PullRequests.ListReviewsIter(ctx, c.owner, c.repo, number, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return "", err
		}
		// Anyone can post a review that starts with the marker, so check who wrote it too.
		if body := r.GetBody(); byExomecha(body) && trusted(r.GetAuthorAssociation()) {
			latest = withoutMarker(body)
		}
	}
	if strings.HasPrefix(latest, approval) {
		return "", nil
	}
	return latest, nil
}

type CI int

const (
	CINone CI = iota
	CIRunning
	CIFailed
	CIPassed
)

type Run struct {
	Name       string
	Conclusion string
	URL        string
}

func (c *Client) CheckCI(ctx context.Context, sha string) (CI, []Run, error) {
	opts := &gh.ListWorkflowRunsOptions{HeadSHA: sha, ListOptions: gh.ListOptions{PerPage: 100}}
	var runs []*gh.WorkflowRun
	for r, err := range c.api.Actions.ListRepositoryWorkflowRunsIter(ctx, c.owner, c.repo, opts) {
		if err != nil {
			return CINone, nil, err
		}
		runs = append(runs, r)
	}
	if len(runs) == 0 {
		return CINone, nil, nil
	}
	for _, r := range runs {
		if r.GetStatus() != "completed" {
			return CIRunning, nil, nil
		}
	}
	var failed []Run
	for _, r := range runs {
		switch r.GetConclusion() {
		case "success", "skipped", "neutral":
		default:
			failed = append(failed, Run{Name: r.GetName(), Conclusion: r.GetConclusion(), URL: r.GetHTMLURL()})
		}
	}
	if len(failed) > 0 {
		return CIFailed, failed, nil
	}
	return CIPassed, nil, nil
}

func (c *Client) CommitTime(ctx context.Context, sha string) (time.Time, error) {
	commit, _, err := c.api.Git.GetCommit(ctx, c.owner, c.repo, sha)
	if err != nil {
		return time.Time{}, err
	}
	return commit.GetCommitter().GetDate().Time, nil
}
