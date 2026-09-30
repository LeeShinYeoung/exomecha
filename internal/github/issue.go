package github

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/LeeShinYeoung/coworker/internal/label"
)

// Marker starts every comment and review coworker writes, to tell them apart from people's.
const Marker = "<!-- coworker -->"

// trusted reports whether GitHub says the author can write to the repository. On a public
// repository anyone can open issues, comment, and review, and none of that may steer the AI.
func trusted(association string) bool {
	switch association {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

type Issue struct {
	ID     int64
	Number int
	Title  string
	Body   string
	Labels []string
	Open   bool
}

func (i Issue) HasLabel(name string) bool {
	return slices.Contains(i.Labels, name)
}

func (i Issue) Status() label.Status {
	for _, name := range i.Labels {
		if label.IsStatus(name) {
			return label.Status(name)
		}
	}
	return label.None
}

func issueFrom(i *gh.Issue) Issue {
	var labels []string
	for _, l := range i.Labels {
		labels = append(labels, l.GetName())
	}
	return Issue{
		ID:     i.GetID(),
		Number: i.GetNumber(),
		Title:  i.GetTitle(),
		Body:   i.GetBody(),
		Labels: labels,
		Open:   i.GetState() == "open",
	}
}

func (c *Client) ListOpen(ctx context.Context, labels ...string) ([]Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      labels,
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []Issue
	for i, err := range c.api.Issues.ListByRepoIter(ctx, c.owner, c.repo, opts) {
		if err != nil {
			return nil, err
		}
		// An issue template can put a label on an outsider's issue, so the label alone is not enough.
		if i.IsPullRequest() || !trusted(i.GetAuthorAssociation()) {
			continue
		}
		out = append(out, issueFrom(i))
	}
	slices.SortFunc(out, func(a, b Issue) int { return cmp.Compare(a.Number, b.Number) })
	return out, nil
}

func (c *Client) Issue(ctx context.Context, number int) (Issue, error) {
	i, _, err := c.api.Issues.Get(ctx, c.owner, c.repo, number)
	if err != nil {
		return Issue{}, err
	}
	return issueFrom(i), nil
}

func (c *Client) CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error) {
	i, _, err := c.api.Issues.Create(ctx, c.owner, c.repo, gh.CreateIssueRequest{
		Title:  title,
		Body:   gh.Ptr(body),
		Labels: labels,
	})
	if err != nil {
		return Issue{}, err
	}
	return issueFrom(i), nil
}

type CloseReason string

const (
	Completed  CloseReason = "completed"
	NotPlanned CloseReason = "not_planned"
)

func (c *Client) Close(ctx context.Context, number int, reason CloseReason) error {
	_, _, err := c.api.Issues.Update(ctx, c.owner, c.repo, number, gh.UpdateIssueRequest{
		State:       gh.Ptr("closed"),
		StateReason: gh.Ptr(string(reason)),
	})
	return err
}

type Comment struct {
	Body      string
	CreatedAt time.Time
	Coworker  bool
	Trusted   bool // written by someone who can write to the repository
}

func (c *Client) Comments(ctx context.Context, number int) ([]Comment, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var out []Comment
	for cm, err := range c.api.Issues.ListCommentsIter(ctx, c.owner, c.repo, number, opts) {
		if err != nil {
			return nil, err
		}
		out = append(out, Comment{
			Body:      withoutMarker(cm.GetBody()),
			CreatedAt: cm.GetCreatedAt().Time,
			Coworker:  byCoworker(cm.GetBody()),
			Trusted:   trusted(cm.GetAuthorAssociation()),
		})
	}
	slices.SortStableFunc(out, func(a, b Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

func (c *Client) AddComment(ctx context.Context, number int, body string) error {
	_, _, err := c.api.Issues.CreateComment(ctx, c.owner, c.repo, number, gh.IssueCommentRequest{
		Body: Marker + "\n" + body,
	})
	return err
}

func byCoworker(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), Marker)
}

func withoutMarker(body string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), Marker))
}
