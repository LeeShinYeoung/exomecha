package github

import (
	"context"
	"errors"
	"net/http"
	"time"

	gh "github.com/google/go-github/v92/github"
)

type Client struct {
	api   *gh.Client
	owner string
	repo  string
}

func New(owner, repo, token string) (*Client, error) {
	return newClient(owner, repo, gh.WithAuthToken(token))
}

// NewAt talks to the API at apiURL instead of GitHub, e.g. a fake server in tests.
func NewAt(apiURL, owner, repo, token string) (*Client, error) {
	return newClient(owner, repo, gh.WithAuthToken(token), gh.WithURLs(&apiURL, nil))
}

// requestTimeout bounds each API call. Without it a dead connection stalled every worker
// for minutes until the OS gave up on it.
var requestTimeout = 30 * time.Second

func newClient(owner, repo string, opts ...gh.ClientOptionsFunc) (*Client, error) {
	opts = append(opts, gh.WithTimeout(requestTimeout), gh.WithTransport(newETagTransport(http.DefaultTransport)))
	api, err := gh.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	return &Client{api: api, owner: owner, repo: repo}, nil
}

// WaitForRateLimit makes API calls under ctx wait for a used-up rate limit to reset instead of
// failing. A failure halfway through a Task can also fail the step that undoes it, which would
// leave the Task in status:working until coworker restarts.
func WaitForRateLimit(ctx context.Context) context.Context {
	return context.WithValue(ctx, gh.SleepUntilPrimaryRateLimitResetWhenRateLimited, true)
}

func isNotFound(err error) bool {
	var e *gh.ErrorResponse
	return errors.As(err, &e) && e.Response != nil && e.Response.StatusCode == http.StatusNotFound
}
