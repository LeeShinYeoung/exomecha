package git

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type Git struct {
	root   string
	url    string
	base   string
	header string

	// mu serializes clone, fetch, and worktree add/remove on the shared clone. Work inside worktrees runs in parallel.
	mu sync.Mutex
}

func New(root, owner, repo, base, token string) *Git {
	return NewAt(root, fmt.Sprintf("https://github.com/%s/%s.git", owner, repo), base, token)
}

// NewAt clones from url instead of GitHub, e.g. a local repository in tests.
func NewAt(root, url, base, token string) *Git {
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return &Git{
		root:   root,
		url:    url,
		base:   base,
		header: "AUTHORIZATION: basic " + auth,
	}
}

func (g *Git) repoDir() string { return filepath.Join(g.root, "repo") }

// Give up on a transfer that stays under 1000 bytes/s for this many seconds. A fetch stuck on a
// dead connection would otherwise hold mu and stall every worker that needs a worktree.
var lowSpeedTime = "30"

// The token goes in as an auth header through env vars on every call, never into .git/config or argv.
func (g *Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0="+g.header,
		"GIT_CONFIG_KEY_1=http.lowSpeedLimit",
		"GIT_CONFIG_VALUE_1=1000",
		"GIT_CONFIG_KEY_2=http.lowSpeedTime",
		"GIT_CONFIG_VALUE_2="+lowSpeedTime,
		"GIT_TERMINAL_PROMPT=0",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
