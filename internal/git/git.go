package git

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// run is for every call that does not talk to GitHub. None of coworker's calls run the project's
// hooks. The AI's own git commands do not come through here, and the project's commit checks
// still run on them.
func (g *Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	return g.command(ctx, dir, nil, args...)
}

// The token goes in as an auth header through env vars, never into .git/config or argv. Anything
// git starts inherits those env vars, so only the calls that talk to GitHub get the header.
func (g *Git) runGitHub(ctx context.Context, dir string, args ...string) (string, error) {
	return g.command(ctx, dir, []string{"http.https://github.com/.extraheader", g.header}, args...)
}

// runSandbox is for transfers with a sandbox's repository, which git reaches by running a command
// named in the URL. Only coworker builds those URLs, and only these calls may run one. What comes
// back was made by the AI, so it is checked like anything from an untrusted server.
func (g *Git) runSandbox(ctx context.Context, dir string, args ...string) (string, error) {
	return g.command(ctx, dir, []string{"protocol.ext.allow", "always", "fetch.fsckObjects", "true"}, args...)
}

func (g *Git) command(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	config := []string{
		"http.lowSpeedLimit", "1000",
		"http.lowSpeedTime", lowSpeedTime,
		"core.hooksPath", os.DevNull,
		// File names come out as they are instead of quoted byte by byte.
		"core.quotePath", "false",
	}
	config = append(config, extra...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)/2))
	for i := 0; i < len(config); i += 2 {
		n := strconv.Itoa(i / 2)
		cmd.Env = append(cmd.Env, "GIT_CONFIG_KEY_"+n+"="+config[i], "GIT_CONFIG_VALUE_"+n+"="+config[i+1])
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
