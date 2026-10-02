package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

func (c *Client) PRFiles(ctx context.Context, number int) ([]string, error) {
	var out []string
	for f, err := range c.api.PullRequests.ListFilesIter(ctx, c.owner, c.repo, number, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, err
		}
		out = append(out, f.GetFilename())
	}
	return out, nil
}

// COMMENT only: the token owner authored the PR, and GitHub blocks approving or requesting changes on your own PR.
func (c *Client) AddReview(ctx context.Context, number int, body string) error {
	_, _, err := c.api.PullRequests.CreateReview(ctx, c.owner, c.repo, number, &gh.PullRequestReviewRequest{
		Body:  gh.Ptr(Marker + "\n" + body),
		Event: gh.Ptr("COMMENT"),
	})
	return err
}

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
	return latest, nil
}

// MergeRefused is GitHub refusing a merge for what the PR is. Status 409 means the head is no longer
// the sha asked for. Status 405 means the PR cannot be merged as it is, for a conflict or for a rule
// of the base branch alike.
type MergeRefused struct {
	Status  int
	Message string
}

func (e *MergeRefused) Error() string {
	return fmt.Sprintf("GitHub refused the merge (%d): %s", e.Status, e.Message)
}

// Passing the reviewed head SHA makes GitHub refuse the merge if anything was pushed since the review.
func (c *Client) Merge(ctx context.Context, number int, sha string) error {
	res, _, err := c.api.PullRequests.Merge(ctx, c.owner, c.repo, number, "", &gh.PullRequestOptions{
		MergeMethod: "merge",
		SHA:         sha,
	})
	var e *gh.ErrorResponse
	if errors.As(err, &e) && e.Response != nil {
		// GitHub answers 405 with these to a merge that raced another merge or push, and a later
		// try goes through.
		raced := strings.HasPrefix(e.Message, "Base branch was modified") || strings.HasPrefix(e.Message, "Head branch was modified")
		switch status := e.Response.StatusCode; {
		case status == http.StatusConflict, status == http.StatusMethodNotAllowed && !raced:
			return &MergeRefused{Status: status, Message: e.Message}
		}
	}
	if err != nil {
		return err
	}
	if !res.GetMerged() {
		return fmt.Errorf("PR #%d not merged: %s", number, res.GetMessage())
	}
	return nil
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
