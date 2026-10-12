package github

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/LeeShinYeoung/exomecha/internal/label"
)

// Marker starts the comments and reviews exomecha wrote before it named their worker. Newer ones
// start with "<!-- exomecha:<worker>:<kind> -->" and a line that names the worker to a person.
const Marker = "<!-- exomecha -->"

const markerStart = "<!-- exomecha"

// Note says which worker wrote a comment or review of exomecha's, and what it is. One that exomecha
// wrote with Marker alone has none.
type Note struct {
	Worker string
	Kind   string
}

const (
	KindSummary  = "summary"  // what builder did in a run
	KindChanges  = "changes"  // reviewer asks builder for changes
	KindApproved = "approved" // reviewer found nothing to fix
	KindCI       = "ci"       // reviewer sends the PR back for a failed CI
	KindResolved = "resolved" // how resolver settled a merge of the base branch
	KindStuck    = "stuck"    // a worker needs a person
)

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

func (c *Client) Close(ctx context.Context, number int) error {
	_, _, err := c.api.Issues.Update(ctx, c.owner, c.repo, number, gh.UpdateIssueRequest{
		State:       new("closed"),
		StateReason: new("completed"),
	})
	if err == nil {
		return nil
	}
	// GitHub closes a PRD itself when a PR that says "Closes" merges, and can then refuse this
	// close. An issue already closed must count, or the caller stops before what comes after the
	// close, such as clearing the PRD's status.
	if i, rerr := c.Issue(ctx, number); rerr == nil && !i.Open {
		return nil
	}
	return err
}

type Comment struct {
	Body      string
	CreatedAt time.Time
	Exomecha  bool
	Note      Note
	Trusted   bool // written by someone who can write to the repository
	// Only a comment on a line of a PR's diff has these.
	Path string
	Line int
}

func (c *Client) Comments(ctx context.Context, number int) ([]Comment, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var out []Comment
	for cm, err := range c.api.Issues.ListCommentsIter(ctx, c.owner, c.repo, number, opts) {
		if err != nil {
			return nil, err
		}
		out = append(out, commentFrom(cm.GetBody(), cm.GetAuthorAssociation(), cm.GetCreatedAt().Time))
	}
	slices.SortStableFunc(out, func(a, b Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// AddComment comments on an issue, or on the conversation of a PR, which is an issue too.
func (c *Client) AddComment(ctx context.Context, number int, n Note, body string) error {
	_, _, err := c.api.Issues.CreateComment(ctx, c.owner, c.repo, number, gh.IssueCommentRequest{
		Body: Noted(n, body),
	})
	return err
}

func commentFrom(body, association string, at time.Time) Comment {
	n, text, ok := ParseNote(body)
	return Comment{
		Body:      text,
		CreatedAt: at,
		Exomecha:  ok,
		Note:      n,
		Trusted:   trusted(association),
	}
}

// Noted is body as exomecha posts it for worker n.Worker.
func Noted(n Note, body string) string {
	return "<!-- exomecha:" + n.Worker + ":" + n.Kind + " -->\n" + header(n.Worker) + "\n\n" + body
}

func header(worker string) string { return "**exomecha " + worker + "**" }

// ParseNote reads what Noted wrote, or a body that starts with Marker alone. ok is false for a body
// without either, and text is then body as it is.
func ParseNote(body string) (n Note, text string, ok bool) {
	b := strings.TrimSpace(body)
	inner, rest, found := strings.Cut(strings.TrimPrefix(b, markerStart), "-->")
	if !strings.HasPrefix(b, markerStart) || !found {
		return Note{}, body, false
	}
	inner = strings.TrimSpace(inner)
	if inner != "" {
		named, isNamed := strings.CutPrefix(inner, ":")
		if !isNamed {
			return Note{}, body, false
		}
		n.Worker, n.Kind, _ = strings.Cut(named, ":")
	}
	rest = strings.TrimSpace(rest)
	if n.Worker != "" {
		rest = strings.TrimSpace(strings.TrimPrefix(rest, header(n.Worker)))
	}
	return n, rest, true
}
