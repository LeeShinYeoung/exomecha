package github

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
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

func (c *Client) LastClosedPR(ctx context.Context, branch string) (number int, merged, ok bool, err error) {
	// GitHub lists the newest first.
	prs, _, err := c.api.PullRequests.List(ctx, c.owner, c.repo, &gh.PullRequestListOptions{
		State:       "closed",
		Head:        c.owner + ":" + branch,
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil || len(prs) == 0 {
		return 0, false, false, err
	}
	return prs[0].GetNumber(), prs[0].MergedAt != nil, true, nil
}

func (c *Client) CreatePR(ctx context.Context, branch, base, title, body string) (int, error) {
	pr, _, err := c.api.PullRequests.Create(ctx, c.owner, c.repo, gh.CreatePullRequest{
		Title: new(title),
		Head:  branch,
		Base:  base,
		Body:  new(body),
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
func (c *Client) AddReview(ctx context.Context, number int, n Note, body string) error {
	_, _, err := c.api.PullRequests.CreateReview(ctx, c.owner, c.repo, number, &gh.PullRequestReviewRequest{
		Body:  new(NoteBody(n, body)),
		Event: new("COMMENT"),
	})
	return err
}

// approval starts the review exomecha leaves when reviewer approves. Before reviews had a Note, it
// was all that told an approval apart.
const approval = "exomecha's reviewer approved this PR. A person reviews and merges it."

func (c *Client) AddApproval(ctx context.Context, number int, summary string) error {
	return c.AddReview(ctx, number, Note{Worker: "reviewer", Kind: KindApproved}, approval+"\n\n"+summary)
}

func isApproval(cm Comment) bool {
	return cm.Note.Kind == KindApproved || cm.Note.Kind == "" && strings.HasPrefix(cm.Body, approval)
}

// SendBacks counts the reviews in which exomecha sent PR pr's PRD back to builder after a person
// last stepped in. A person steps in after exomecha approves the PR, and after exomecha comments on
// the PRD to ask a person to answer. prd is the PRD's comments, and the PRD can be in another
// repository.
func (c *Client) SendBacks(ctx context.Context, pr int, prd []Comment) (int, error) {
	var since time.Time
	for _, cm := range prd {
		if cm.Exomecha && cm.Trusted && cm.CreatedAt.After(since) {
			since = cm.CreatedAt
		}
	}
	reviews, err := c.reviews(ctx, pr)
	if err != nil {
		return 0, err
	}
	var sent []time.Time
	for _, cm := range reviews {
		if !cm.Exomecha || !cm.Trusted {
			continue
		}
		switch {
		case isApproval(cm):
			if cm.CreatedAt.After(since) {
				since = cm.CreatedAt
			}
		case cm.Note.Kind == "" || cm.Note.Kind == KindChanges || cm.Note.Kind == KindCI:
			sent = append(sent, cm.CreatedAt)
		}
	}
	n := 0
	for _, at := range sent {
		if at.After(since) {
			n++
		}
	}
	return n, nil
}

// PRComments returns what was written on the PR, oldest first: comments on the conversation, the
// bodies of reviews, and comments on lines of the diff.
func (c *Client) PRComments(ctx context.Context, number int) ([]Comment, error) {
	// A PR's conversation is its issue's comments.
	out, err := c.Comments(ctx, number)
	if err != nil {
		return nil, err
	}
	reviews, err := c.reviews(ctx, number)
	if err != nil {
		return nil, err
	}
	out = append(out, reviews...)
	opts := &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for lc, err := range c.api.PullRequests.ListCommentsIter(ctx, c.owner, c.repo, number, opts) {
		if err != nil {
			return nil, err
		}
		cm := commentFrom(lc.GetBody(), lc.GetAuthorAssociation(), lc.GetCreatedAt().Time)
		cm.Path = lc.GetPath()
		// line is empty once a later push takes the line out of the diff.
		cm.Line = cmp.Or(lc.GetLine(), lc.GetOriginalLine())
		out = append(out, cm)
	}
	slices.SortStableFunc(out, func(a, b Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

func (c *Client) reviews(ctx context.Context, number int) ([]Comment, error) {
	var out []Comment
	for r, err := range c.api.PullRequests.ListReviewsIter(ctx, c.owner, c.repo, number, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, err
		}
		// The token owner's review that is not submitted yet is listed too. A review made only of
		// comments on lines has no body, and PRComments reads those comments on their own.
		if r.GetState() == "PENDING" || strings.TrimSpace(r.GetBody()) == "" {
			continue
		}
		out = append(out, commentFrom(r.GetBody(), r.GetAuthorAssociation(), r.GetSubmittedAt().Time))
	}
	return out, nil
}

type CI int

const (
	CINone CI = iota
	CIRunning
	CIFailed
	CIPassed
)

type Run struct {
	ID         int64
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
		if !passing(r.GetConclusion()) {
			failed = append(failed, Run{ID: r.GetID(), Name: r.GetName(), Conclusion: r.GetConclusion(), URL: r.GetHTMLURL()})
		}
	}
	if len(failed) > 0 {
		return CIFailed, failed, nil
	}
	return CIPassed, nil, nil
}

func passing(conclusion string) bool {
	switch conclusion {
	case "success", "skipped", "neutral":
		return true
	}
	return false
}

type JobLog struct {
	Job  string
	Tail string // the last lines of its log
}

// A job's failure is near the end of its log.
const logLines = 80

// The download goes to wherever GitHub redirects, which is not GitHub's API: it gets no token, and
// a log read once stays out of the API client's ETag store.
var logClient = &http.Client{Timeout: time.Minute}

func (c *Client) FailedJobLogs(ctx context.Context, runID int64) ([]JobLog, error) {
	var out []JobLog
	opts := &gh.ListWorkflowJobsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for j, err := range c.api.Actions.ListWorkflowJobsIter(ctx, c.owner, c.repo, runID, opts) {
		if err != nil {
			return nil, err
		}
		// A job not finished yet has no conclusion.
		if j.GetConclusion() == "" || passing(j.GetConclusion()) {
			continue
		}
		at, _, err := c.api.Actions.GetWorkflowJobLogs(ctx, c.owner, c.repo, j.GetID(), 1)
		if err != nil {
			return nil, err
		}
		tail, err := logTail(ctx, at.String())
		if err != nil {
			return nil, fmt.Errorf("read the log of %s: %w", j.GetName(), err)
		}
		out = append(out, JobLog{Job: j.GetName(), Tail: tail})
	}
	return out, nil
}

func logTail(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := logClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: %s", resp.Status)
	}
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "\ufeff")
		// Every line starts with when it was written, which says nothing about the failure.
		if stamp, rest, ok := strings.Cut(line, " "); ok {
			if _, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
				line = rest
			}
		}
		lines = append(lines, line)
		if len(lines) > logLines {
			lines = lines[1:]
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

func (c *Client) CommitTime(ctx context.Context, sha string) (time.Time, error) {
	commit, _, err := c.api.Git.GetCommit(ctx, c.owner, c.repo, sha)
	if err != nil {
		return time.Time{}, err
	}
	return commit.GetCommitter().GetDate().Time, nil
}
